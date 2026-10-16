package tui

// SQL syntax highlighting for the previews (DDL, view definitions, history,
// query files), in the colours of Helix's modus_vivendi_tinted by default.

import (
	"image/color"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/raikhan/sqlh/dialect"
)

type sqlClass int

const (
	sqlPlain sqlClass = iota
	sqlKeyword
	sqlComment
	sqlString
	sqlNumber
	sqlFunction
	sqlType
	sqlAlias
	sqlParameter
	sqlOperator
	numSQLClasses
)

// sqlClassNames: panel.syntax's class names, by class.
var sqlClassNames = [numSQLClasses]string{"", "keyword", "comment", "string", "number", "function", "type", "alias", "parameter", "operator"}

// defaultSyntax: modus_vivendi_tinted (Helix's keyword, comment, string,
// constant.numeric, function, type, variable.parameter colours).
var defaultSyntax = map[string]string{
	"keyword": "#b6a0ff", "comment": "#ff9f80", "string": "#79a8ff", "number": "#00bcff",
	"function": "#feacd0", "type": "#6ae4b9", "alias": "#00d3d0", "parameter": "#00d3d0",
}

// syntax: a style per class.
type syntax [numSQLClasses]lipgloss.Style

func newSyntax(over map[string]string) syntax {
	var sx syntax
	for c := sqlClass(1); c < numSQLClasses; c++ {
		col := defaultSyntax[sqlClassNames[c]]
		if v, ok := over[sqlClassNames[c]]; ok {
			col = v
		}
		if col != "" {
			sx[c] = lipgloss.NewStyle().Foreground(lipgloss.Color(col))
		}
	}
	return sx
}

type sqlTok struct {
	text  string
	class sqlClass
	word  string // upper case, for words
	kind  byte   // w word, s space, p punctuation, o other
}

// moreKeywords: keywords the completion list doesn't have.
var moreKeywords = map[string]bool{"RETURNING": true, "ILIKE": true, "INDEX": true, "CASCADE": true, "NULLS": true,
	"MINUS": true, "SEQUENCE": true, "COMMENT": true, "IF": true, "REPLACE": true, "TO": true, "OWNER": true,
	"LANGUAGE": true, "RETURNS": true, "MATERIALIZED": true, "FIRST": true, "LAST": true, "IMMUTABLE": true, "STABLE": true}

// notKeywords: words of the keyword list that are usually names.
var notKeywords = map[string]bool{"VALUE": true, "KEY": true, "USER": true, "DATA": true, "NAME": true, "LEVEL": true,
	"SOURCE": true, "RESULT": true, "POSITION": true, "ACTION": true, "STATUS": true, "ID": true, "TYPE": true, "CODE": true}

var typeWords = map[string]bool{"INT": true, "INTEGER": true, "BIGINT": true, "SMALLINT": true, "TINYINT": true,
	"NUMERIC": true, "DECIMAL": true, "NUMBER": true, "REAL": true, "FLOAT": true, "DOUBLE": true, "PRECISION": true,
	"VARCHAR": true, "VARCHAR2": true, "NVARCHAR": true, "NVARCHAR2": true, "CHAR": true, "NCHAR": true, "CHARACTER": true,
	"VARYING": true, "TEXT": true, "CLOB": true, "NCLOB": true, "BLOB": true, "BYTEA": true, "RAW": true, "BOOLEAN": true,
	"BOOL": true, "DATE": true, "TIME": true, "TIMESTAMP": true, "TIMESTAMPTZ": true, "INTERVAL": true, "UUID": true,
	"JSON": true, "JSONB": true, "XML": true, "SERIAL": true, "BIGSERIAL": true, "BINARY_DOUBLE": true, "BINARY_FLOAT": true,
	"LONG": true, "ROWID": true, "INET": true, "MONEY": true}

func isKeyword(w string) bool {
	if notKeywords[w] {
		return false
	}
	return moreKeywords[w] || dialect.MatchKeyword(w) != dialect.Unmatched
}

// lexSQL splits sql into tokens (whitespace and newlines included).
func lexSQL(sql string) []sqlTok {
	r := []rune(sql)
	var out []sqlTok
	emit := func(from, to int, class sqlClass, kind byte) {
		t := sqlTok{text: string(r[from:to]), class: class, kind: kind}
		if kind == 'w' {
			t.word = strings.ToUpper(t.text)
		}
		out = append(out, t)
	}
	isWordStart := func(c rune) bool { return c == '_' || unicode.IsLetter(c) }
	isWordPart := func(c rune) bool {
		return c == '_' || c == '$' || c == '#' || unicode.IsLetter(c) || unicode.IsDigit(c)
	}
	for i := 0; i < len(r); {
		c := r[i]
		j := i + 1
		switch {
		case unicode.IsSpace(c):
			for j < len(r) && unicode.IsSpace(r[j]) {
				j++
			}
			emit(i, j, sqlPlain, 's')
		case c == '-' && j < len(r) && r[j] == '-':
			for j < len(r) && r[j] != '\n' {
				j++
			}
			emit(i, j, sqlComment, 'o')
		case c == '/' && j < len(r) && r[j] == '*':
			j++
			for j < len(r) && !(r[j] == '/' && r[j-1] == '*' && j-1 > i+1) {
				j++
			}
			j = min(j+1, len(r))
			emit(i, j, sqlComment, 'o')
		case c == '\'':
			for j < len(r) {
				if r[j] == '\'' {
					if j+1 < len(r) && r[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			emit(i, j, sqlString, 'o')
		case c == '"' || c == '`':
			for j < len(r) && r[j] != c {
				j++
			}
			j = min(j+1, len(r))
			emit(i, j, sqlPlain, 'q')
		case c == '$' && j < len(r) && (r[j] == '$' || isWordStart(r[j])):
			// $$ … $$ / $tag$ … $tag$ (PostgreSQL bodies), else $name
			k := j
			for k < len(r) && isWordPart(r[k]) && r[k] != '$' {
				k++
			}
			if k < len(r) && r[k] == '$' {
				j = indexRunes(r, k+1, r[i:k+1])
				emit(i, j, sqlString, 'o')
			} else {
				emit(i, k, sqlParameter, 'o')
				j = k
			}
		case (c == '$' || c == ':' || c == '@') && j < len(r) && (unicode.IsDigit(r[j]) || isWordStart(r[j])) &&
			!(c == ':' && i > 0 && r[i-1] == ':'):
			for j < len(r) && isWordPart(r[j]) {
				j++
			}
			emit(i, j, sqlParameter, 'o')
		case c == '?':
			emit(i, j, sqlParameter, 'o')
		case unicode.IsDigit(c) || (c == '.' && j < len(r) && unicode.IsDigit(r[j]) && (i == 0 || !isWordPart(r[i-1]))):
			for j < len(r) && (unicode.IsDigit(r[j]) || r[j] == '.' || r[j] == 'e' || r[j] == 'E') {
				j++
			}
			emit(i, j, sqlNumber, 'o')
		case isWordStart(c):
			for j < len(r) && isWordPart(r[j]) {
				j++
			}
			emit(i, j, sqlPlain, 'w')
		case strings.ContainsRune("=<>!+-*/%|&^~", c):
			for j < len(r) && strings.ContainsRune("=<>!|&~*", r[j]) && j-i < 3 {
				j++
			}
			emit(i, j, sqlOperator, 'p')
		case c == ':' && j < len(r) && r[j] == ':':
			j++
			emit(i, j, sqlOperator, 'p')
		default:
			emit(i, j, sqlPlain, 'p')
		}
		i = j
	}
	classifyWords(out)
	return out
}

// classifyWords gives words a class from their neighbours: keywords,
// functions (before "("), types (after "::", in definitions), aliases (after
// AS, or after a table in FROM / JOIN).
func classifyWords(toks []sqlTok) {
	next := func(i int) int { // the next significant token
		for i++; i < len(toks); i++ {
			if toks[i].kind != 's' && toks[i].class != sqlComment {
				return i
			}
		}
		return -1
	}
	const (
		none = iota
		expectTable
		inTable
		afterTable
		expectAlias
	)
	state, inFrom := none, false
	prev := -1 // the previous significant token
	for i := range toks {
		t := &toks[i]
		if t.kind == 's' || t.class == sqlComment {
			continue
		}
		n := next(i)
		nextText := ""
		if n >= 0 {
			nextText = toks[n].text
		}
		prevText := ""
		if prev >= 0 {
			prevText = toks[prev].text
		}
		switch {
		case t.kind == 'w' || t.kind == 'q':
			w := t.word
			kw := t.kind == 'w' && isKeyword(w) && prevText != "." && nextText != "."
			if w == "KEY" && prev >= 0 && (toks[prev].word == "PRIMARY" || toks[prev].word == "FOREIGN") {
				kw = true
			}
			switch {
			case state == expectAlias && !kw:
				t.class, state = sqlAlias, none
			case state == afterTable && !kw:
				t.class, state = sqlAlias, none
			case prevText == "::":
				t.class = sqlType
			case state == expectTable || state == inTable:
				if kw && w != "LATERAL" && w != "ONLY" {
					state = none // FROM (SELECT …), FROM LATERAL …
					t.class = sqlKeyword
					break
				}
				if nextText == "." {
					state = inTable
				} else {
					state = afterTable
				}
			case t.kind == 'w' && typeWords[w] && prevText != ".":
				t.class = sqlType
			case nextText == "(" && t.kind == 'w' && !kw:
				t.class = sqlFunction
			case nextText == "(" && kw && dialect.MatchKeyword(w) == dialect.Matched && !structural[w]:
				t.class = sqlFunction // count(, coalesce(
			case kw:
				t.class = sqlKeyword
			}
			if t.class == sqlKeyword {
				switch w {
				case "FROM":
					state, inFrom = expectTable, true
				case "JOIN", "UPDATE", "INTO", "TABLE", "REFERENCES":
					state = expectTable
				case "AS":
					state = expectAlias
				case "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "UNION", "ON", "USING", "SELECT", "SET", "VALUES",
					"RETURNING", "WINDOW", "FETCH", "OFFSET", "EXCEPT", "INTERSECT", "MINUS":
					inFrom = false
					if state != expectAlias {
						state = none
					}
				default:
					if state == afterTable {
						state = none
					}
				}
			}
		default:
			switch {
			case t.text == "." && state == inTable:
			case t.text == "," && inFrom && (state == afterTable || state == none):
				state = expectTable
			case t.text == "(" && state == expectTable:
				state, inFrom = none, false
			default:
				if state == afterTable || state == inTable {
					state = none
				}
			}
		}
		prev = i
	}
}

// structural: keywords that take a parenthesis but are not functions.
var structural = map[string]bool{"IN": true, "VALUES": true, "EXISTS": true, "AS": true, "ON": true, "USING": true,
	"OVER": true, "FILTER": true, "AND": true, "OR": true, "NOT": true, "INTO": true, "TABLE": true, "KEY": true,
	"REFERENCES": true, "CHECK": true, "UNIQUE": true, "PRIMARY": true, "FROM": true, "JOIN": true, "WITH": true,
	"ALL": true, "ANY": true, "SOME": true, "WHEN": true, "THEN": true, "ELSE": true, "SELECT": true, "WHERE": true,
	"RETURNS": true, "VIEW": true, "INDEX": true, "PARTITION": true, "BY": true, "CASE": true}

// cell: a rune and its class, for wrapping.
type cell struct {
	r     rune
	class sqlClass
}

// sqlLines highlights sql and wraps it to w cells (w <= 0: no wrapping);
// each line is styled on its own (no style runs across lines).
func (sx syntax) sqlLines(sql string, w int) []string {
	return sx.sqlLinesBg(sql, w, nil)
}

// sqlLinesBg is sqlLines on a background (diffs).
func (sx syntax) sqlLinesBg(sql string, w int, bg color.Color) []string {
	sql = strings.TrimRight(strings.ReplaceAll(sql, "\t", "    "), "\n")
	var lines [][]cell
	cur := []cell{}
	for _, t := range lexSQL(sql) {
		for _, r := range t.text {
			if r == '\n' {
				lines = append(lines, cur)
				cur = []cell{}
				continue
			}
			if r == '\r' {
				continue
			}
			cur = append(cur, cell{r, t.class})
		}
	}
	lines = append(lines, cur)
	var out []string
	for _, l := range lines {
		for _, seg := range wrapCells(l, w) {
			out = append(out, sx.render(seg, bg))
		}
	}
	return out
}

// wrapCells breaks a line at the last space that fits (else at w).
func wrapCells(l []cell, w int) [][]cell {
	if w <= 0 {
		return [][]cell{l}
	}
	var out [][]cell
	for {
		width, cut, lastSpace := 0, -1, -1
		for i, c := range l {
			width += runewidth.RuneWidth(c.r)
			if width > w {
				cut = i
				break
			}
			if c.r == ' ' {
				lastSpace = i
			}
		}
		if cut < 0 {
			return append(out, l)
		}
		if lastSpace > 0 {
			out = append(out, l[:lastSpace])
			l = l[lastSpace+1:]
		} else {
			out = append(out, l[:cut])
			l = l[cut:]
		}
	}
}

func (sx syntax) render(cs []cell, bg color.Color) string {
	var b strings.Builder
	for i := 0; i < len(cs); {
		j := i
		for j < len(cs) && cs[j].class == cs[i].class {
			j++
		}
		var text strings.Builder
		for _, c := range cs[i:j] {
			text.WriteRune(c.r)
		}
		st := sx[cs[i].class]
		if bg != nil {
			st = st.Background(bg)
		}
		b.WriteString(st.Render(text.String()))
		i = j
	}
	if len(cs) == 0 && bg != nil {
		return lipgloss.NewStyle().Background(bg).Render(" ")
	}
	return b.String()
}

// indexRunes: the end of the first tag in r from from (len(r): none).
func indexRunes(r []rune, from int, tag []rune) int {
	for i := from; i+len(tag) <= len(r); i++ {
		if string(r[i:i+len(tag)]) == string(tag) {
			return i + len(tag)
		}
	}
	return len(r)
}
