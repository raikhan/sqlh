// Package lint finds obvious mistakes in SQL using the schema cache: unknown
// aliases/qualifiers, tables and columns. It never asks the database and
// stays silent whenever it can't be sure (subtle scoping, dynamic SQL,
// PL/SQL, constructs it doesn't parse): a missed mistake is acceptable, a
// false alarm is not.
package lint

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/token"
)

// Diagnostic is a problem at a document range (0-based lines, characters).
type Diagnostic struct {
	Start, End sqlsplit.Position
	Message    string
}

// Check lints every statement of a document.
func Check(text string, cat *Catalog) []Diagnostic {
	if cat == nil {
		return nil
	}
	stmts := sqlsplit.Split(text, cat.Dialect)
	// Tables the document creates (not cached yet) or alters (columns may
	// differ from the cache).
	doc := docDDL{created: ddlNames(text, createRe), altered: ddlNames(text, alterRe)}
	var out []Diagnostic
	for _, st := range stmts {
		if st.PLSQL {
			continue
		}
		out = append(out, checkStatement(st, cat, doc)...)
	}
	return out
}

type docDDL struct{ created, altered map[string]bool }

// Constructs the token-level checks don't understand: skip the statement.
// $tag$ quoting, Oracle q-quotes, templates (${x}, {{x}}, {%…%}, {x}).
var bailRe = regexp.MustCompile(`(?i)\$[a-z_]*\$|\bq'|\$\{|\{\{|\{%|\{\w+\}`)

// Oracle SQL*Plus substitution variables (&x, &&x).
var substRe = regexp.MustCompile(`&&?[A-Za-z_]`)

// DDL anywhere in the document, including dynamic SQL in strings
// (EXECUTE IMMEDIATE 'CREATE TABLE …').
var (
	createRe = regexp.MustCompile(`(?i)\bcreate\s+(or\s+replace\s+)?((global|local|private)\s+)?((temp|temporary)\s+)?(unlogged\s+)?(table|view|materialized\s+view|synonym)\s+(if\s+not\s+exists\s+)?([\w$#."]+)`)
	alterRe  = regexp.MustCompile(`(?i)\balter\s+table\s+(if\s+exists\s+)?(only\s+)?([\w$#."]+)`)
)

// ddlNames: names matched by re in the whole text (last group, schema dropped).
func ddlNames(text string, re *regexp.Regexp) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		name := m[len(m)-1]
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		out[strings.ToUpper(strings.Trim(name, `"`))] = true
	}
	return out
}

type tok struct {
	kind   token.Kind
	text   string // word value (unquoted) or token text
	upper  string // upper-cased word, "" for non-words
	quoted bool
	from   token.Pos
	to     token.Pos
}

func (t tok) isWord() bool         { return t.kind == token.SQLKeyword }
func (t tok) is(kw ...string) bool { return t.isWord() && !t.quoted && contains(kw, t.upper) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func tokenize(sql string) ([]tok, bool) {
	raw, err := token.NewTokenizer(bytes.NewBufferString(sql), &dialect.GenericSQLDialect{}).Tokenize()
	if err != nil {
		return nil, false
	}
	var out []tok
	for _, r := range raw {
		switch r.Kind {
		case token.Whitespace, token.Comment, token.MultilineComment:
			continue
		case token.ILLEGAL:
			return nil, false
		}
		t := tok{kind: r.Kind, from: r.From, to: r.To}
		switch v := r.Value.(type) {
		case *token.SQLWord:
			t.text, t.quoted = v.Value, v.QuoteStyle != 0
			t.upper = strings.ToUpper(v.Value)
		case string:
			t.text = v
			if r.Kind == token.SQLKeyword {
				t.upper = strings.ToUpper(v)
			}
		}
		out = append(out, t)
	}
	return out, true
}

// ref is a table reference in a FROM/JOIN/INTO/UPDATE/USING clause.
type ref struct {
	names   []string // upper: alias, table name (each usable as a qualifier)
	res     resolved // set when the exact table is known
	columns bool     // res has known columns and no column-alias list hides them
}

type checker struct {
	st      sqlsplit.Statement
	cat     *Catalog
	doc     docDDL
	toks    []tok
	used    []bool // token consumed as part of a table reference
	refs    []ref
	ctes    map[string]bool
	pending map[int]pendingRef // closing parenthesis → reference awaiting its alias
	// unsure: the statement has constructs that may introduce names the
	// checker doesn't see; unknown qualifiers aren't reported then.
	unsure bool
	diags  []Diagnostic
}

var statementKinds = []string{"SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE", "VALUES"}

func checkStatement(st sqlsplit.Statement, cat *Catalog, doc docDDL) []Diagnostic {
	if bailRe.MatchString(st.SQL) || (cat.Dialect == sqlsplit.Oracle && substRe.MatchString(st.SQL)) {
		return nil
	}
	toks, ok := tokenize(st.SQL)
	if !ok || len(toks) == 0 || !toks[0].is(statementKinds...) {
		return nil
	}
	c := &checker{st: st, cat: cat, doc: doc, toks: toks, used: make([]bool, len(toks)), ctes: map[string]bool{}, pending: map[int]pendingRef{}}
	c.collectCTEs()
	if !c.collectRefs() {
		return nil
	}
	c.checkQualifiers()
	c.checkUpdateSet()
	return c.diags
}

func (c *checker) report(from, to token.Pos, format string, args ...interface{}) {
	pos := func(p token.Pos) sqlsplit.Position {
		if p.Line == 0 {
			return sqlsplit.Position{Line: c.st.Start.Line, Char: c.st.Start.Char + p.Col}
		}
		return sqlsplit.Position{Line: c.st.Start.Line + p.Line, Char: p.Col}
	}
	c.diags = append(c.diags, Diagnostic{Start: pos(from), End: pos(to), Message: fmt.Sprintf(format, args...)})
}

func (c *checker) at(i int) tok {
	if i < 0 || i >= len(c.toks) {
		return tok{kind: token.ILLEGAL}
	}
	return c.toks[i]
}

// closing returns the index of the parenthesis matching the one at i.
func (c *checker) closing(i int) int {
	depth := 0
	for j := i; j < len(c.toks); j++ {
		switch c.toks[j].kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(c.toks) - 1
}

func (c *checker) isSubquery(lparen int) bool {
	return c.at(lparen+1).is("SELECT", "WITH", "VALUES")
}

// collectCTEs: WITH [RECURSIVE] name [(cols)] AS [NOT] [MATERIALIZED] (…), name …
func (c *checker) collectCTEs() {
	for i := 0; i < len(c.toks); i++ {
		if !c.toks[i].is("WITH") && c.toks[i].kind != token.Comma {
			continue
		}
		j := i + 1
		if c.at(j).is("RECURSIVE") {
			j++
		}
		name := c.at(j)
		if !name.isWord() {
			continue
		}
		k := j + 1
		if c.at(k).kind == token.LParen {
			k = c.closing(k) + 1
		}
		if c.at(k).is("AS") && (c.at(k+1).kind == token.LParen || c.at(k+1).is("NOT", "MATERIALIZED")) {
			c.ctes[name.upper] = true
		}
	}
}

// Words that end a table reference instead of being its alias.
var notAlias = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`WHERE JOIN INNER LEFT RIGHT FULL OUTER CROSS NATURAL ON USING GROUP ORDER
		HAVING LIMIT OFFSET FETCH UNION INTERSECT EXCEPT MINUS FOR CONNECT START WINDOW SET VALUES
		RETURNING RETURN PARTITION SAMPLE TABLESAMPLE LATERAL PIVOT UNPIVOT MODEL WHEN THEN ELSE END AS OF
		SELECT FROM INTO WITH LOCK NOWAIT SKIP STRAIGHT_JOIN APPLY MATCH_RECOGNIZE QUALIFY KEEP DEFAULT
		BY AND OR NOT IS IN LIKE BETWEEN DO CONFLICT LOG INDEXED FORCE IGNORE USE ONLY VERSIONS SEARCH
		CYCLE NULL`) {
		notAlias[w] = true
	}
}

// collectRefs finds table references at query level (not inside function
// calls like extract(year from d)). It walks every token, so FROM clauses
// inside subqueries are found too.
func (c *checker) collectRefs() bool {
	kind := c.toks[0].upper
	var parens []bool // per open parenthesis: is a subquery
	seenReturning := false
	for i := 0; i < len(c.toks); i++ {
		t := c.toks[i]
		switch t.kind {
		case token.LParen:
			parens = append(parens, c.isSubquery(i))
			continue
		case token.RParen:
			if len(parens) > 0 {
				parens = parens[:len(parens)-1]
			}
			if p, ok := c.pending[i]; ok { // end of (subquery) or func(…): alias follows
				delete(c.pending, i)
				i = c.afterRef(p.r, i, false, p.list)
			}
			continue
		}
		if len(parens) > 0 && !parens[len(parens)-1] {
			continue // inside a function call / expression list
		}
		prev := c.at(i - 1)
		switch {
		case t.is("FROM"):
			if prev.is("DISTINCT") { // IS [NOT] DISTINCT FROM
				continue
			}
			i = c.tableRef(i+1, false, true)
		case t.is("JOIN"):
			i = c.tableRef(i+1, false, false)
		case t.is("USING"):
			if c.at(i+1).kind == token.LParen && !c.isSubquery(i+1) {
				continue // JOIN … USING (cols)
			}
			i = c.tableRef(i+1, false, true)
		case t.is("UPDATE"):
			if i == 0 || prev.kind == token.RParen {
				i = c.tableRef(i+1, false, false)
			}
		case t.is("DELETE"):
			if (i == 0 || prev.kind == token.RParen) && !c.at(i+1).is("FROM") {
				i = c.tableRef(i+1, false, false) // Oracle: DELETE t WHERE …
			}
		case t.is("RETURNING"):
			seenReturning = true
		case t.is("INTO"):
			if (kind == "INSERT" || kind == "MERGE" || kind == "WITH") && !seenReturning {
				i = c.tableRef(i+1, true, false)
			}
		}
	}
	return len(c.pending) == 0
}

type pendingRef struct {
	r    ref
	list bool
}

// tableRef reads the head of a reference starting at i and returns the index
// of the last token it consumed. A parenthesised part (subquery, function
// arguments) is left for collectRefs to walk; its alias is read at the
// closing parenthesis. into: INSERT INTO name [alias] (columns). list: a
// comma may continue with another reference (FROM a, b).
func (c *checker) tableRef(i int, into, list bool) int {
	start := i
	for c.at(i).is("ONLY", "LATERAL") {
		i++
	}
	t := c.at(i)
	switch {
	case t.kind == token.LParen:
		if c.isSubquery(i) {
			c.pending[c.closing(i)] = pendingRef{list: list}
		}
		return i - 1 // collectRefs continues at the parenthesis
	case t.isWord():
		parts := []tok{t}
		for c.at(i+1).kind == token.Period && c.at(i+2).isWord() {
			parts = append(parts, c.at(i+2))
			i += 2
		}
		for j := start; j <= i; j++ {
			c.used[j] = true
		}
		if c.at(i+1).kind == token.LParen && !into { // table function: generate_series(…)
			c.pending[c.closing(i+1)] = pendingRef{r: ref{names: []string{parts[len(parts)-1].upper}}, list: list}
			return i
		}
		return c.afterRef(c.resolveTable(parts), i, into, list)
	}
	c.unsure = true // something unexpected where a table should be
	return i - 1
}

// Clauses after a table that may introduce names this checker doesn't follow.
var confusing = []string{"PARTITION", "SAMPLE", "TABLESAMPLE", "VERSIONS", "PIVOT", "UNPIVOT", "MATCH_RECOGNIZE", "MODEL"}

// afterRef reads what follows a table reference ending at i: alias, column
// alias list, INSERT column list, ", next reference".
func (c *checker) afterRef(r ref, i int, into, list bool) int {
	if c.at(i+1).is("WITH") && c.at(i+2).is("ORDINALITY") {
		i += 2
	}
	j := i + 1
	if c.at(j).is("AS") {
		if c.at(j + 1).is("OF") { // Oracle flashback: AS OF …
			c.unsure = true
			c.refs = append(c.refs, r)
			return i
		}
		j++
	}
	if a := c.at(j); a.isWord() && (a.quoted || !notAlias[a.upper]) {
		r.names = append(r.names, a.upper)
		c.used[j] = true
		i = j
		if c.at(i+1).kind == token.LParen && !into { // alias(col, …) hides the real columns
			r.columns = false
			i = c.closing(i + 1)
		}
	}
	if c.at(i + 1).is(confusing...) {
		c.unsure = true
	}
	if into && c.at(i+1).kind == token.LParen {
		c.checkInsertColumns(r, i+1)
		i = c.closing(i + 1)
	}
	c.refs = append(c.refs, r)
	if list && c.at(i+1).kind == token.Comma {
		return c.tableRef(i+2, false, true)
	}
	return i
}

// resolveTable checks a named table and returns its reference.
func (c *checker) resolveTable(parts []tok) ref {
	last := parts[len(parts)-1]
	r := ref{names: []string{last.upper}}
	if strings.ContainsAny(last.text, "@") || len(parts) > 2 {
		return r // dblink, db.schema.table: can't tell
	}
	var schema string
	if len(parts) == 2 {
		schema = parts[0].text
	} else if c.ctes[last.upper] || c.doc.created[last.upper] || c.implicitTable(last.upper) {
		return r
	}
	res, found, sure := c.cat.resolve(schema, last.text)
	if !found && sure {
		name := last.text
		if schema != "" {
			name = schema + "." + last.text
		}
		msg := fmt.Sprintf("unknown table %s", name)
		if s := suggest(last.upper, c.cat.tableNames(schema)); s != "" {
			msg += fmt.Sprintf(" (did you mean %s?)", strings.ToLower(s))
		}
		c.report(parts[0].from, last.to, "%s", msg)
		return r
	}
	r.res = res
	if _, ok := c.cat.columnsOf(res); ok && !c.doc.altered[last.upper] {
		r.columns = true
	}
	return r
}

// implicitTable: names available without being in the cache.
func (c *checker) implicitTable(upper string) bool {
	switch c.cat.Dialect {
	case sqlsplit.PostgreSQL:
		return strings.HasPrefix(upper, "PG_")
	case sqlsplit.SQLite:
		return strings.HasPrefix(upper, "SQLITE_") || strings.HasPrefix(upper, "PRAGMA_")
	case sqlsplit.Oracle:
		return upper == "DUAL"
	}
	return false
}

// Columns every table has without listing them.
var pseudoColumns = map[sqlsplit.Dialect][]string{
	sqlsplit.Oracle:     {"ROWID", "ROWNUM", "ORA_ROWSCN", "OBJECT_VALUE", "OBJECT_ID", "COLUMN_VALUE"},
	sqlsplit.PostgreSQL: {"CTID", "OID", "XMIN", "XMAX", "CMIN", "CMAX", "TABLEOID"},
	sqlsplit.SQLite:     {"ROWID", "OID", "_ROWID_"},
}

func (c *checker) hasColumn(r ref, upper string) (exists, known bool) {
	if !r.columns {
		return false, false
	}
	cols, ok := c.cat.columnsOf(r.res)
	if !ok {
		return false, false
	}
	return cols[upper] || contains(pseudoColumns[c.cat.Dialect], upper), true
}

func (c *checker) unknownColumn(r ref, name tok) {
	cols, _ := c.cat.columnsOf(r.res)
	names := make([]string, 0, len(cols))
	for col := range cols {
		names = append(names, col)
	}
	msg := fmt.Sprintf("%s has no column %s", strings.ToLower(r.res.table), name.text)
	if s := suggest(name.upper, names); s != "" {
		msg += fmt.Sprintf(" (did you mean %s?)", strings.ToLower(s))
	}
	c.report(name.from, name.to, "%s", msg)
}

// checkInsertColumns: INSERT INTO t (a, b, …) — a plain list of names.
func (c *checker) checkInsertColumns(r ref, lparen int) {
	end := c.closing(lparen)
	for j := lparen + 1; j < end; j += 2 {
		name, sep := c.at(j), c.at(j+1)
		if !name.isWord() || (sep.kind != token.Comma && j+1 != end) {
			return // not a simple column list
		}
	}
	for j := lparen + 1; j < end; j += 2 {
		name := c.at(j)
		if exists, known := c.hasColumn(r, name.upper); known && !exists {
			c.unknownColumn(r, name)
		}
	}
}

// checkUpdateSet: UPDATE t SET a = …, b = … (single target table).
func (c *checker) checkUpdateSet() {
	if !c.toks[0].is("UPDATE") || len(c.refs) == 0 {
		return
	}
	target := c.refs[0]
	depth := 0
	inSet := false
	expectCol := false
	for i, t := range c.toks {
		switch t.kind {
		case token.LParen:
			depth++
			expectCol = false
			continue
		case token.RParen:
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		switch {
		case t.is("SET"):
			inSet, expectCol = true, true
		case inSet && t.is("FROM", "WHERE", "RETURNING"):
			return
		case inSet && t.kind == token.Comma:
			expectCol = true
		case inSet && expectCol && t.isWord() && c.at(i+1).kind == token.Eq:
			if exists, known := c.hasColumn(target, t.upper); known && !exists {
				c.unknownColumn(target, t)
			}
			expectCol = false
		default:
			expectCol = false
		}
	}
}

// checkQualifiers: qualifier.column references.
func (c *checker) checkQualifiers() {
	byName := map[string][]ref{}
	for _, r := range c.refs {
		for _, n := range r.names {
			byName[n] = append(byName[n], r)
		}
	}
	var aliases []string
	for _, r := range c.refs {
		if len(r.names) > 1 {
			aliases = append(aliases, strings.ToLower(r.names[len(r.names)-1]))
		} else if len(r.names) == 1 {
			aliases = append(aliases, strings.ToLower(r.names[0]))
		}
	}
	for i := 0; i < len(c.toks); i++ {
		q := c.toks[i]
		if c.used[i] || !q.isWord() || c.at(i+1).kind != token.Period {
			continue
		}
		// whole chain q . a [. b …]
		end := i + 2
		for c.at(end).isWord() && c.at(end+1).kind == token.Period {
			end += 2
		}
		member := c.at(i + 2)
		chainStart := i
		i = end
		prev := c.at(chainStart - 1)
		switch {
		case prev.kind == token.Colon || prev.kind == token.Ampersand || prev.kind == token.Period:
			continue // :new.col, &var., part of something else
		case c.at(end+1).kind == token.LParen:
			continue // schema.func(…), pkg.proc(…)
		case !member.isWord() && member.kind != token.Mult:
			continue
		case strings.ContainsAny(q.text, "@$") || q.is("EXCLUDED", "NEW", "OLD"):
			continue
		case member.is("NEXTVAL", "CURRVAL"):
			continue
		}
		refs, ok := byName[q.upper]
		if !ok {
			if c.unsure || c.cat.hasSchema(q.upper) || c.ctes[q.upper] {
				continue
			}
			if c.cat.Dialect == sqlsplit.Oracle && !c.oracleSureNotPackage(member, byName) {
				continue
			}
			if c.cat.Dialect == sqlsplit.Generic {
				continue
			}
			msg := fmt.Sprintf("%s is not a table or alias in this statement", q.text)
			if len(aliases) > 0 {
				sort.Strings(aliases)
				msg += fmt.Sprintf(" (in scope: %s)", strings.Join(dedupe(aliases), ", "))
			}
			c.report(q.from, q.to, "%s", msg)
			continue
		}
		if member.kind == token.Mult {
			continue
		}
		// column: must exist in at least one table the name may refer to
		missing := true
		for _, r := range refs {
			exists, known := c.hasColumn(r, member.upper)
			if !known || exists {
				missing = false
				break
			}
		}
		if missing {
			c.unknownColumn(refs[0], member)
		}
	}
}

// oracleSureNotPackage: in Oracle, x.y may be pkg.function or pkg.constant.
// Only call it a bad alias when y is * or a column of a table in scope.
func (c *checker) oracleSureNotPackage(member tok, byName map[string][]ref) bool {
	if member.kind == token.Mult {
		return true
	}
	for _, r := range c.refs {
		if exists, known := c.hasColumn(r, member.upper); known && exists {
			return true
		}
	}
	return false
}

func dedupe(s []string) []string {
	var out []string
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// suggest returns the closest name within a small edit distance.
func suggest(upper string, names []string) string {
	best, bestD := "", 3
	if len(upper) < 4 {
		bestD = 2
	}
	sort.Strings(names)
	for _, n := range names {
		if d := distance(upper, strings.ToUpper(n)); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
