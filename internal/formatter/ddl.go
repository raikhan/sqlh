package formatter

import (
	"strings"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/ddl"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/token"
)

// DDL statements get sqlh's own layout instead of the sqls formatter's
// (which reads every name( as a call and breaks lines at ON, OR, commas):
//
//	CREATE TABLE t (          ALTER TABLE t                CREATE INDEX i ON t (a, b);
//	    id int PRIMARY KEY,       ADD COLUMN a int,         DROP TABLE IF EXISTS t;
//	    total numeric(12, 2)      DROP COLUMN b;
//	);
//
// CREATE VIEW/TABLE … AS: the header on one line, the query below it laid
// out by the regular formatter. Statements with comments are left to the
// regular formatter.

// ddlWords: words cased as keywords besides the lexer's keywords.
var ddlWords = func() map[string]bool {
	m := ddl.Words()
	for _, w := range strings.Fields(`IF EXISTS NOT NULL ON TO FROM AS IS OF FOR IN AND OR BY WITH ADD
		ALTER DROP CREATE TABLE VIEW COLUMN KEY PRIMARY FOREIGN REFERENCES UNIQUE CHECK DEFAULT INDEX
		USING WHERE CONSTRAINT RENAME SET TYPE DATA ONLY ALL`) {
		m[w] = true
	}
	for _, d := range []dialect.DatabaseDriver{dialect.DatabaseDriverPostgreSQL, dialect.DatabaseDriverOracle, dialect.DatabaseDriverSQLite3, ""} {
		for _, t := range dialect.DataTypes(d) {
			for _, w := range strings.Fields(t.Name) {
				m[w] = true
			}
		}
	}
	return m
}()

// layoutDDL rewrites the DDL statements of out (the formatter's output for
// src, a whitespace-collapsed document) with the DDL layout.
func layoutDDL(out, src string, params lsp.DocumentFormattingParams, cfg *config.Config) string {
	srcStmts := sqlsplit.Split(src, sqlsplit.Generic)
	outStmts := sqlsplit.Split(out, sqlsplit.Generic)
	if len(srcStmts) != len(outStmts) {
		return out
	}
	type repl struct {
		from, to int
		text     string
	}
	var reps []repl
	for i, st := range srcStmts {
		a, _, ok := ddl.InStatement(st.SQL, sqlsplit.Generic)
		if !ok || a.Kind == "" || a.DML {
			continue
		}
		text, ok := ddlLayout(st.SQL, a, params, cfg)
		if !ok {
			continue
		}
		o := outStmts[i]
		from, ok := offsetOf(out, lsp.Position{Line: o.Start.Line, Character: o.Start.Char})
		if !ok || !strings.HasPrefix(out[from:], o.SQL) {
			continue
		}
		reps = append(reps, repl{from, from + len(o.SQL), text})
	}
	for k := len(reps) - 1; k >= 0; k-- {
		r := reps[k]
		out = out[:r.from] + r.text + out[r.to:]
	}
	return out
}

func indentUnit(o lsp.FormattingOptions) string {
	if !o.InsertSpaces {
		return "\t"
	}
	return strings.Repeat(" ", int(o.TabSize))
}

// ddlLayout lays out one DDL statement (whitespace already collapsed);
// ok is false to leave it to the regular formatter.
func ddlLayout(sql string, a *ddl.Analysis, params lsp.DocumentFormattingParams, cfg *config.Config) (string, bool) {
	toks := vtokenize(sql)
	idents := map[string]bool{}
	addName := func(n ddl.Name) {
		idents[strings.ToUpper(n.Name)] = true
		if n.Schema != "" {
			idents[strings.ToUpper(n.Schema)] = true
		}
	}
	for _, r := range a.Refs {
		addName(r.Name)
		if r.Table != nil {
			addName(*r.Table)
		}
	}
	for _, d := range a.Defined {
		addName(d)
	}
	for i, t := range toks {
		switch t.kind {
		case vLineComment, vBlockComment:
			return "", false
		case vSpace:
			if strings.Contains(t.text, "\n") {
				return "", false
			}
		case vWord:
			toks[i].text = caseWord(t.text, idents, cfg.LowercaseKeywords)
		}
	}
	unit := indentUnit(params.Options)
	switch {
	case a.Body >= 0:
		return layoutBody(sql, toks, a, params, cfg)
	case a.Kind == "CREATE TABLE":
		if s, ok := layoutElements(toks, unit); ok {
			return s, true
		}
	case a.Kind == "ALTER TABLE":
		if s, ok := layoutActions(toks, unit); ok {
			return s, true
		}
	}
	return render(toks), true
}

// caseWord: keywords upper (lower) case; identifiers as written.
func caseWord(w string, idents map[string]bool, lower bool) string {
	u := strings.ToUpper(w)
	if strings.ContainsAny(w, ".$#@") || idents[u] {
		return w
	}
	if token.MakeKeyword(w, 0).Kind == dialect.Unmatched && !ddlWords[u] {
		return w
	}
	if lower {
		return strings.ToLower(w)
	}
	return u
}

func render(toks []vtoken) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.text)
	}
	return strings.TrimSpace(b.String())
}

func isPunct(t vtoken, s string) bool { return t.kind == vPunct && t.text == s }

// closingParen: index of the ')' matching the '(' at i (-1 if none).
func closingParen(toks []vtoken, i int) int {
	depth := 0
	for j := i; j < len(toks); j++ {
		switch {
		case isPunct(toks[j], "("):
			depth++
		case isPunct(toks[j], ")"):
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// splitTop splits toks at commas outside parentheses.
func splitTop(toks []vtoken) [][]vtoken {
	var out [][]vtoken
	depth, start := 0, 0
	for j, t := range toks {
		switch {
		case isPunct(t, "("):
			depth++
		case isPunct(t, ")"):
			depth--
		case isPunct(t, ",") && depth == 0:
			out = append(out, toks[start:j])
			start = j + 1
		}
	}
	return append(out, toks[start:])
}

// layoutElements: CREATE TABLE t ( one column or constraint per line ) tail
func layoutElements(toks []vtoken, unit string) (string, bool) {
	open := -1
	for j, t := range toks {
		if isPunct(t, "(") {
			open = j
			break
		}
	}
	if open < 0 {
		return "", false
	}
	end := closingParen(toks, open)
	if end < 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(render(toks[:open+1]))
	for k, el := range splitTop(toks[open+1 : end]) {
		if k > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n" + unit + render(el))
	}
	b.WriteString("\n)")
	if tail := render(toks[end+1:]); tail != "" {
		b.WriteString(" " + tail)
	}
	return b.String(), true
}

// layoutActions: ALTER TABLE t, then several actions one per line.
func layoutActions(toks []vtoken, unit string) (string, bool) {
	// ALTER TABLE [IF EXISTS] [ONLY] name
	j, words := 0, 0
	for ; j < len(toks); j++ {
		t := toks[j]
		if t.kind == vSpace {
			continue
		}
		u := strings.ToUpper(t.text)
		if words < 2 || u == "IF" || u == "EXISTS" || u == "ONLY" {
			words++
			continue
		}
		break // the table name
	}
	if j >= len(toks) {
		return "", false
	}
	actions := splitTop(toks[j+1:])
	if len(actions) < 2 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(render(toks[:j+1]))
	for k, act := range actions {
		if k > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n" + unit + render(act))
	}
	return b.String(), true
}

// layoutBody: CREATE VIEW/TABLE … AS on one line, the query below it.
func layoutBody(sql string, toks []vtoken, a *ddl.Analysis, params lsp.DocumentFormattingParams, cfg *config.Config) (string, bool) {
	_, dtoks, ok := ddl.InStatement(sql, sqlsplit.Generic)
	if !ok || a.Body >= len(dtoks) || dtoks[a.Body].From.Line != 0 {
		return "", false
	}
	// rune column → byte offset (the statement is one line: collapsed)
	off, col := 0, 0
	for i := range sql {
		if col == dtoks[a.Body].From.Col {
			off = i
			break
		}
		col++
	}
	if off == 0 {
		return "", false
	}
	header := render(headerTokens(toks, len([]rune(sql[:off]))))
	body := strings.TrimSpace(sql[off:])
	edits, err := formatFn(body, params, cfg)
	if err != nil || len(edits) != 1 {
		return "", false
	}
	e := edits[0]
	start, ok1 := offsetOf(body, e.Range.Start)
	end, ok2 := offsetOf(body, e.Range.End)
	if !ok1 || !ok2 || start > end {
		return "", false
	}
	return header + "\n" + strings.TrimSpace(body[:start]+e.NewText+body[end:]), true
}

// headerTokens: the tokens covering the first n runes.
func headerTokens(toks []vtoken, n int) []vtoken {
	pos := 0
	for i, t := range toks {
		if pos >= n {
			return toks[:i]
		}
		pos += len([]rune(t.text))
	}
	return toks
}
