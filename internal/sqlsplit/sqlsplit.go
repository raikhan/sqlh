// Package sqlsplit splits SQL scripts into executable statements.
//
// It is used for execution, not for completion: the completion parser splits
// on every ';', which breaks PL/SQL blocks, PostgreSQL dollar-quoted bodies and
// SQLite triggers. Rules:
//
//   - ';' terminates a statement, outside quotes, comments and blocks.
//   - Oracle: a line containing only '/' terminates a statement (SQL*Plus).
//     PL/SQL units (BEGIN, DECLARE, CREATE [OR REPLACE] PROCEDURE/FUNCTION/
//     PACKAGE/TRIGGER/TYPE/LIBRARY) are terminated only by '/' or end of text,
//     and keep their final "END;". Supports q'[...]' quoting.
//   - PostgreSQL: $tag$ ... $tag$ dollar quoting.
//   - SQLite: CREATE TRIGGER ... BEGIN ... END; ends at the ';' after END.
//
// Returned SQL has the trailing ';' removed (Oracle rejects it for plain SQL;
// other databases don't need it), except for PL/SQL units.
package sqlsplit

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type Dialect int

const (
	Generic Dialect = iota
	Oracle
	PostgreSQL
	SQLite
)

// DialectOf maps an sqlh driver name to a Dialect.
func DialectOf(driver string) Dialect {
	switch driver {
	case "oracle":
		return Oracle
	case "postgresql", "postgres":
		return PostgreSQL
	case "sqlite3", "sqlite":
		return SQLite
	}
	return Generic
}

// Position is an LSP position: 0-based line, UTF-16 code unit column.
type Position struct {
	Line int
	Char int
}

func (p Position) Before(q Position) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Char < q.Char)
}

// Statement is one executable statement.
type Statement struct {
	// SQL is the text to execute (comments preserved, terminator removed).
	SQL string
	// Start is the first significant character; End is just past the
	// terminator (or the last significant character when unterminated).
	Start, End Position
	// PLSQL is true for Oracle PL/SQL units.
	PLSQL bool
	// Terminated: ended by ';' or a '/' line, not by the end of the text.
	Terminated bool
}

var (
	plsqlStartRe   = regexp.MustCompile(`(?is)^(begin|declare|create\s+(or\s+replace\s+)?((editionable|noneditionable)\s+)?(procedure|function|package|trigger|type|library))\b`)
	sqliteTrigRe   = regexp.MustCompile(`(?is)^create\s+((temp|temporary)\s+)?trigger\b`)
	trailingSemiRe = regexp.MustCompile(`;\s*$`)
)

type scanner struct {
	src  []rune
	i    int
	line int
	col  int // UTF-16 units
	// hazards, when non-nil, collects constructs found while scanning
	hazards *[]Hazard
}

func (s *scanner) note(what string) {
	if s.hazards != nil {
		*s.hazards = append(*s.hazards, Hazard{Line: s.line, What: what})
	}
}

func (s *scanner) eof() bool { return s.i >= len(s.src) }

func (s *scanner) peek(off int) rune {
	if s.i+off < len(s.src) {
		return s.src[s.i+off]
	}
	return 0
}

func (s *scanner) pos() Position { return Position{Line: s.line, Char: s.col} }

func (s *scanner) advance() {
	r := s.src[s.i]
	s.i++
	if r == '\n' {
		s.line++
		s.col = 0
	} else if r > 0xFFFF {
		s.col += 2
	} else {
		s.col++
	}
}

// atLineStart reports whether only spaces/tabs precede index i on its line.
func (s *scanner) atLineStart(i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch s.src[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return true
}

// slashLine reports whether index i begins a line containing only '/'.
// It returns the index just past the line (excluding the newline).
func (s *scanner) slashLine(i int) (int, bool) {
	if s.src[i] != '/' || !s.atLineStart(i) {
		return 0, false
	}
	j := i + 1
	for j < len(s.src) && (s.src[j] == ' ' || s.src[j] == '\t' || s.src[j] == '\r') {
		j++
	}
	if j < len(s.src) && s.src[j] != '\n' {
		return 0, false
	}
	return j, true
}

// skipQuoted consumes a quoted section starting at the current position if
// there is one, and reports whether it did.
func (s *scanner) skipQuoted(d Dialect) bool {
	r := s.peek(0)
	switch {
	case r == '-' && s.peek(1) == '-':
		for !s.eof() && s.peek(0) != '\n' {
			s.advance()
		}
		return true
	case r == '/' && s.peek(1) == '*':
		s.advance()
		s.advance()
		for !s.eof() && !(s.peek(0) == '*' && s.peek(1) == '/') {
			s.advance()
		}
		if !s.eof() {
			s.advance()
			s.advance()
		}
		return true
	case d == Oracle && (r == 'q' || r == 'Q') && s.peek(1) == '\'' && s.peek(2) != 0 && s.identBoundaryBefore():
		s.note("Oracle q'...' string")
		return s.skipQQuote(0)
	case d == Oracle && (r == 'n' || r == 'N') && (s.peek(1) == 'q' || s.peek(1) == 'Q') && s.peek(2) == '\'' && s.peek(3) != 0 && s.identBoundaryBefore():
		s.note("Oracle q'...' string")
		s.advance()
		return s.skipQQuote(0)
	case r == '\'' || r == '"' || r == '`':
		s.advance()
		for !s.eof() {
			if s.peek(0) == r {
				if s.peek(1) == r { // doubled quote escape
					s.advance()
					s.advance()
					continue
				}
				s.advance()
				break
			}
			s.advance()
		}
		return true
	case d == PostgreSQL && r == '$':
		if tag, ok := s.dollarTag(); ok {
			s.note("dollar-quoted body ($$)")
			for k := 0; k < len(tag); k++ {
				s.advance()
			}
			for !s.eof() {
				if s.peek(0) == '$' && s.hasPrefix(tag) {
					for k := 0; k < len(tag); k++ {
						s.advance()
					}
					break
				}
				s.advance()
			}
			return true
		}
	}
	return false
}

func (s *scanner) identBoundaryBefore() bool {
	if s.i == 0 {
		return true
	}
	p := s.src[s.i-1]
	return !(unicode.IsLetter(p) || unicode.IsDigit(p) || p == '_' || p == '$' || p == '#')
}

// skipQQuote consumes Oracle q'<d>...<d>' at the current 'q'.
func (s *scanner) skipQQuote(_ int) bool {
	s.advance() // q
	s.advance() // '
	open := s.peek(0)
	closeR := open
	switch open {
	case '[':
		closeR = ']'
	case '(':
		closeR = ')'
	case '{':
		closeR = '}'
	case '<':
		closeR = '>'
	}
	s.advance()
	for !s.eof() {
		if s.peek(0) == closeR && s.peek(1) == '\'' {
			s.advance()
			s.advance()
			break
		}
		s.advance()
	}
	return true
}

func (s *scanner) hasPrefix(p []rune) bool {
	if s.i+len(p) > len(s.src) {
		return false
	}
	for k, r := range p {
		if s.src[s.i+k] != r {
			return false
		}
	}
	return true
}

// dollarTag returns the $tag$ at the current position (PostgreSQL).
func (s *scanner) dollarTag() ([]rune, bool) {
	if s.peek(0) != '$' {
		return nil, false
	}
	j := s.i + 1
	for j < len(s.src) && (s.src[j] == '_' || unicode.IsLetter(s.src[j]) || (j > s.i+1 && unicode.IsDigit(s.src[j]))) {
		j++
	}
	if j < len(s.src) && s.src[j] == '$' {
		return s.src[s.i : j+1], true
	}
	return nil, false
}

// Split splits text into statements.
func Split(text string, d Dialect) []Statement {
	return split(text, d, nil)
}

func split(text string, d Dialect, hazards *[]Hazard) []Statement {
	s := &scanner{src: []rune(text), hazards: hazards}
	var out []Statement

	for {
		// skip whitespace and comments before the statement
		for !s.eof() {
			r := s.peek(0)
			if unicode.IsSpace(r) {
				s.advance()
				continue
			}
			if (r == '-' && s.peek(1) == '-') || (r == '/' && s.peek(1) == '*') {
				s.skipQuoted(d)
				continue
			}
			if r == ';' { // empty statement
				s.advance()
				continue
			}
			if d == Oracle {
				if end, ok := s.slashLine(s.i); ok { // stray '/'
					s.note("'/' terminator line")
					for s.i < end {
						s.advance()
					}
					continue
				}
			}
			break
		}
		if s.eof() {
			return out
		}

		startIdx, start := s.i, s.pos()
		head := string(s.src[startIdx:min(len(s.src), startIdx+200)])
		plsql := d == Oracle && plsqlStartRe.MatchString(head)
		sqliteTrigger := d == SQLite && sqliteTrigRe.MatchString(head)

		endIdx := -1 // exclusive end of statement text
		var end Position
		terminated := false
		lastSig, lastSigPos := startIdx, start // after last significant rune
		for !s.eof() {
			if d == Oracle && s.peek(0) == '/' {
				if lineEnd, ok := s.slashLine(s.i); ok {
					s.note("'/' terminator line")
					endIdx, end = lastSig, lastSigPos
					terminated = true
					for s.i < lineEnd {
						s.advance()
					}
					break
				}
			}
			if s.skipQuoted(d) {
				lastSig, lastSigPos = s.i, s.pos()
				continue
			}
			r := s.peek(0)
			if r == ';' && !plsql {
				if !sqliteTrigger || endsWithEND(s.src[startIdx:s.i]) {
					endIdx = s.i
					s.advance()
					end = s.pos()
					terminated = true
					break
				}
			}
			s.advance()
			if !unicode.IsSpace(r) {
				lastSig, lastSigPos = s.i, s.pos()
			}
		}
		if endIdx < 0 { // end of text
			endIdx, end = lastSig, lastSigPos
		}
		sql := strings.TrimSpace(string(s.src[startIdx:endIdx]))
		if !plsql {
			sql = strings.TrimSpace(trailingSemiRe.ReplaceAllString(sql, ""))
		}
		if sql != "" {
			out = append(out, Statement{SQL: sql, Start: start, End: end, PLSQL: plsql, Terminated: terminated})
		}
	}
}

// endsWithEND reports whether the text ends with the keyword END.
func endsWithEND(r []rune) bool {
	t := strings.TrimRightFunc(string(r), unicode.IsSpace)
	if len(t) < 3 || !strings.EqualFold(t[len(t)-3:], "END") {
		return false
	}
	if len(t) == 3 {
		return true
	}
	p := rune(t[len(t)-4])
	return !(unicode.IsLetter(p) || unicode.IsDigit(p) || p == '_')
}

// AtCursor picks the statement to run for a cursor position:
//   - the statement whose span contains the cursor;
//   - between statements: the following statement when the cursor is in a
//     comment block attached to it (no blank line in between), otherwise the
//     preceding statement (e.g. cursor on the blank line after it).
func AtCursor(text string, stmts []Statement, cur Position) (Statement, bool) {
	if len(stmts) == 0 {
		return Statement{}, false
	}
	var prev, next *Statement
	for i := range stmts {
		st := &stmts[i]
		if !cur.Before(st.Start) && !st.End.Before(cur) {
			return *st, true
		}
		if st.End.Before(cur) || st.End == cur {
			prev = st
		} else if next == nil && cur.Before(st.Start) {
			next = st
		}
	}
	if next != nil && (prev == nil || attached(text, cur.Line, next.Start.Line, prev)) {
		return *next, true
	}
	if prev != nil {
		return *prev, true
	}
	return *next, true
}

// InLines picks the statements to run for a selection of whole lines
// (0-based, inclusive): every statement with a part on one of them. A
// selection within one line, or one touching no statement (blank lines,
// comments), falls back to AtCursor at cur.
func InLines(text string, stmts []Statement, from, to int, cur Position) ([]Statement, bool) {
	if to > from {
		var sel []Statement
		for _, st := range stmts {
			if st.Start.Line <= to && st.End.Line >= from {
				sel = append(sel, st)
			}
		}
		if len(sel) > 0 {
			return sel, true
		}
	}
	st, ok := AtCursor(text, stmts, cur)
	if !ok {
		return nil, false
	}
	return []Statement{st}, true
}

// attached reports whether lines (from, to) contain no blank line and the
// cursor is not on the line where the previous statement ends.
func attached(text string, from, to int, prev *Statement) bool {
	if prev != nil && prev.End.Line == from {
		return false
	}
	lines := strings.Split(text, "\n")
	for l := from; l < to && l < len(lines); l++ {
		if strings.TrimSpace(lines[l]) == "" {
			return false
		}
	}
	return true
}

// Hazard is a construct the SQL formatter cannot handle safely.
type Hazard struct {
	Line int // 0-based
	What string
}

var (
	// a transaction BEGIN (PostgreSQL/SQLite) is not a PL/SQL block
	beginTxnRe = regexp.MustCompile(`(?is)^begin\s*(;|$|(transaction|work|isolation|read|deferred|immediate|exclusive)\b)`)
	// triggers (any dialect) only carry a body when they have BEGIN/DECLARE;
	// a PostgreSQL "... EXECUTE FUNCTION f()" trigger formats fine
	triggerRe     = regexp.MustCompile(`(?is)^create\s+(or\s+replace\s+)?((editionable|noneditionable|temp|temporary)\s+)?trigger\b`)
	triggerBodyRe = regexp.MustCompile(`(?is)\b(begin|declare)\b`)
)

// FormatHazards reports constructs that the formatter would corrupt: Oracle
// '/' terminator lines, q'...' strings and PL/SQL blocks, PostgreSQL
// dollar-quoted bodies and SQLite trigger bodies. It is dialect-agnostic
// because a file is not necessarily written for the current connection.
func FormatHazards(text string) []Hazard {
	var hz []Hazard
	split(text, Oracle, &hz)     // '/' lines, q-quotes
	split(text, PostgreSQL, &hz) // dollar quotes
	for _, st := range split(text, Generic, nil) {
		head := st.SQL
		switch {
		case triggerRe.MatchString(head):
			if triggerBodyRe.MatchString(head) {
				hz = append(hz, Hazard{Line: st.Start.Line, What: "trigger body"})
			}
		case plsqlStartRe.MatchString(head) && !beginTxnRe.MatchString(head):
			hz = append(hz, Hazard{Line: st.Start.Line, What: "PL/SQL block"})
		}
	}
	sort.SliceStable(hz, func(i, j int) bool { return hz[i].Line < hz[j].Line })
	return hz
}
