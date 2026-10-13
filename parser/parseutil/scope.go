package parseutil

// Query scopes: which tables, derived tables and WITH queries (CTEs) a position
// in a statement can see.
//
// A statement is a tree of query levels: the statement itself and every
// parenthesised subquery. A position sees the relations of its own level (the
// FROM/JOIN of the SELECT branch it is in) and, for correlated references, those
// of the enclosing levels. The FROM clauses of nested subqueries, CTE bodies and
// other UNION branches are not visible. CTEs defined by a WITH clause of any
// enclosing level are visible by name.

import (
	"strings"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/token"
)

// maxResolveDepth bounds the resolution of derived tables and CTEs that refer
// to each other.
const maxResolveDepth = 16

type cteDef struct {
	name     string
	colNames []string // explicit column list: WITH x (a, b) AS (…)
	body     ast.TokenList
	nameNode ast.Node

	resolved  bool
	resolving bool
	cols      []*SubQueryColumn
}

// queryScope holds the relations of one query level (one SELECT branch).
type queryScope struct {
	parent *queryScope
	ctes   []*cteDef
	tables []*TableInfo
	rels   []*SubQueryInfo
	// joined: the references that follow a JOIN keyword, in order (for join completion).
	joined []joinedRef
	depth  int
	// sawTableKeyword: a FROM/JOIN/… was found at this level.
	sawTableKeyword bool
}

type joinedRef struct {
	node  ast.Node
	table *TableInfo // nil for derived tables and CTE references
	// hasOn: followed by a written join condition (ON a = b).
	hasOn bool
}

func (s *queryScope) findCTE(name string) *cteDef {
	for sc := s; sc != nil; sc = sc.parent {
		for i := len(sc.ctes) - 1; i >= 0; i-- {
			if strings.EqualFold(sc.ctes[i].name, name) {
				return sc.ctes[i]
			}
		}
	}
	return nil
}

func (s *queryScope) visibleCTEs() []*cteDef {
	var levels [][]*cteDef
	for sc := s; sc != nil; sc = sc.parent {
		levels = append(levels, sc.ctes)
	}
	var res []*cteDef
	seen := map[string]bool{}
	// innermost definitions shadow outer ones
	for _, ctes := range levels {
		for _, c := range ctes {
			key := strings.ToUpper(c.name)
			if seen[key] {
				continue
			}
			seen[key] = true
			res = append(res, c)
		}
	}
	return res
}

func (s *queryScope) child() *queryScope {
	return &queryScope{parent: s, depth: s.depth + 1}
}

// ---- keyword helpers -------------------------------------------------------

func isWhitespaceNode(n ast.Node) bool {
	if it, ok := n.(*ast.Item); ok {
		return it.Tok.MatchKind(token.Whitespace) || it.Tok.MatchKind(token.Comment) ||
			it.Tok.MatchKind(token.MultilineComment)
	}
	return false
}

// keywordOf returns the upper-case keyword(s) of an Item or MultiKeyword node,
// "" for anything else.
func keywordOf(n ast.Node) string {
	switch v := n.(type) {
	case *ast.Item:
		if v.Tok.MatchKind(token.SQLKeyword) {
			return strings.ToUpper(v.Tok.String())
		}
		return ""
	case *ast.MultiKeyword:
		words := make([]string, 0, len(v.GetKeywords()))
		for _, k := range v.GetKeywords() {
			words = append(words, strings.ToUpper(k.String()))
		}
		return strings.Join(words, " ")
	}
	return ""
}

// isWord: a keyword or identifier spelled as one of words (for non-reserved
// words such as LATERAL that the lexer may not know as keywords).
func isWord(n ast.Node, words ...string) bool {
	switch n.(type) {
	case *ast.Item, *ast.Identifier:
		s := strings.ToUpper(n.String())
		for _, w := range words {
			if s == w {
				return true
			}
		}
	}
	return false
}

func isKeyword(n ast.Node, kws ...string) bool {
	k := keywordOf(n)
	if k == "" {
		return false
	}
	for _, kw := range kws {
		if k == kw {
			return true
		}
	}
	return false
}

var tableKeywords = map[string]bool{
	"FROM": true, "UPDATE": true, "INTO": true, "INSERT INTO": true, "DELETE FROM": true,
	"USING": true, "TABLE": true,
}

var joinKeywords = map[string]bool{
	"JOIN": true, "INNER JOIN": true, "CROSS JOIN": true, "OUTER JOIN": true,
	"LEFT JOIN": true, "RIGHT JOIN": true, "FULL JOIN": true,
	"LEFT OUTER JOIN": true, "RIGHT OUTER JOIN": true, "FULL OUTER JOIN": true,
	"NATURAL JOIN": true, "NATURAL LEFT JOIN": true, "NATURAL RIGHT JOIN": true,
	"NATURAL LEFT OUTER JOIN": true, "NATURAL RIGHT OUTER JOIN": true,
}

var setOperators = map[string]bool{
	"UNION": true, "INTERSECT": true, "EXCEPT": true, "MINUS": true,
}

// isSubQueryList: a parenthesised query, (SELECT …) or (WITH … SELECT …).
func isSubQueryList(n ast.Node) bool {
	p, ok := n.(*ast.Parenthesis)
	if !ok {
		return false
	}
	for _, t := range innerNodes(p) {
		if isWhitespaceNode(t) {
			continue
		}
		return isKeyword(t, "SELECT", "WITH")
	}
	return false
}

// innerNodes returns the tokens of a list, without the surrounding parentheses
// of a Parenthesis.
func innerNodes(list ast.TokenList) []ast.Node {
	toks := list.GetTokens()
	if _, ok := list.(*ast.Parenthesis); ok {
		if len(toks) > 0 && toks[0].String() == "(" {
			toks = toks[1:]
		}
		if len(toks) > 0 && toks[len(toks)-1].String() == ")" {
			toks = toks[:len(toks)-1]
		}
	}
	return toks
}

func nextNonWS(nodes []ast.Node, i int) (int, ast.Node) {
	for j := i + 1; j < len(nodes); j++ {
		if !isWhitespaceNode(nodes[j]) {
			return j, nodes[j]
		}
	}
	return len(nodes), nil
}

func identName(n ast.Node) (string, bool) {
	switch v := n.(type) {
	case *ast.Identifier:
		return v.NoQuoteString(), true
	case *ast.Item:
		// unreserved keywords used as names (e.g. a CTE called "data")
		if v.Tok.MatchKind(token.SQLKeyword) {
			return v.NoQuoteString(), true
		}
	}
	return "", false
}

// ---- WITH clause -------------------------------------------------------------

// parseWith reads a leading WITH clause from nodes. It returns the CTE
// definitions and the index of the first node after the clause (0 when there
// is none). Incomplete clauses (while typing) yield what was read so far.
func parseWith(nodes []ast.Node) ([]*cteDef, int) {
	i, n := nextNonWS(nodes, -1)
	if n == nil || !isKeyword(n, "WITH") {
		return nil, 0
	}
	if j, m := nextNonWS(nodes, i); m != nil && isWord(m, "RECURSIVE") {
		i = j
	}
	var defs []*cteDef
	for {
		j, m := nextNonWS(nodes, i)
		if m == nil {
			return defs, len(nodes)
		}
		def := &cteDef{nameNode: m}
		switch v := m.(type) {
		case *ast.FunctionLiteral: // x(a, b) without a space
			toks := v.GetTokens()
			if len(toks) == 0 {
				return defs, j
			}
			name, ok := identName(toks[0])
			if !ok {
				return defs, j
			}
			def.name = name
			def.nameNode = toks[0]
			if len(toks) > 1 {
				if p, ok := toks[len(toks)-1].(*ast.Parenthesis); ok {
					def.colNames = listNames(p)
				}
			}
		default:
			name, ok := identName(m)
			if !ok || isKeyword(m, "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "AS") {
				return defs, j
			}
			def.name = name
		}
		i = j
		// optional column list, AS, [NOT] MATERIALIZED, body
		var body *ast.Parenthesis
		for body == nil {
			k, o := nextNonWS(nodes, i)
			if o == nil {
				defs = append(defs, def)
				return defs, len(nodes)
			}
			switch {
			case isKeyword(o, "AS") || isWord(o, "NOT", "MATERIALIZED"):
				i = k
			case isSubQueryList(o):
				body = o.(*ast.Parenthesis)
				i = k
			case func() bool { _, ok := o.(*ast.Parenthesis); return ok }() && def.colNames == nil:
				def.colNames = listNames(o.(*ast.Parenthesis))
				i = k
			default:
				// not a CTE body (yet): stop here
				defs = append(defs, def)
				return defs, k
			}
		}
		def.body = body
		defs = append(defs, def)
		k, o := nextNonWS(nodes, i)
		if o == nil {
			return defs, len(nodes)
		}
		if t, ok := o.(*ast.Item); ok && t.Tok.MatchKind(token.Comma) {
			i = k
			continue
		}
		return defs, k
	}
}

// listNames returns the identifiers of a parenthesised name list: (a, b, c).
func listNames(p *ast.Parenthesis) []string {
	var names []string
	for _, n := range innerNodes(p) {
		switch v := n.(type) {
		case *ast.IdentifierList:
			for _, id := range v.GetIdentifiers() {
				if name, ok := identName(id); ok {
					names = append(names, name)
				}
			}
		default:
			if name, ok := identName(v); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// ---- levels and branches -------------------------------------------------------

// queryLevels returns the statement followed by the subqueries enclosing pos,
// outermost first.
func queryLevels(stmt ast.TokenList, pos token.Pos) []ast.TokenList {
	levels := []ast.TokenList{stmt}
	for _, r := range NewNodeWalker(stmt, pos).Paths {
		p, ok := r.CurNode.(*ast.Parenthesis)
		if !ok || !isSubQueryList(p) {
			continue
		}
		// strictly inside the parentheses
		if token.ComparePos(pos, p.Pos()) <= 0 || token.ComparePos(pos, p.End()) >= 0 {
			continue
		}
		levels = append(levels, p)
	}
	return levels
}

// branch returns the nodes of the SELECT branch (between set operators) of
// nodes that contains pos; the last branch when pos is after all of them and
// the first one when it is not inside nodes at all.
func branch(nodes []ast.Node, pos *token.Pos) []ast.Node {
	var branches [][]ast.Node
	start := 0
	for i, n := range nodes {
		if setOperators[keywordOf(n)] {
			branches = append(branches, nodes[start:i])
			start = i + 1
		}
	}
	branches = append(branches, nodes[start:])
	if len(branches) == 1 || pos == nil {
		return branches[0]
	}
	for i, b := range branches {
		if len(b) == 0 {
			continue
		}
		last := i == len(branches)-1
		if last || token.ComparePos(*pos, b[len(b)-1].End()) <= 0 {
			if token.ComparePos(*pos, b[0].Pos()) >= 0 || i == 0 {
				return b
			}
		}
	}
	return branches[0]
}

// ---- scope construction --------------------------------------------------------

// buildScope builds the scope of the query list (statement or subquery body)
// for the branch containing pos (nil: first branch).
func buildScope(list ast.TokenList, parent *queryScope, pos *token.Pos) *queryScope {
	sc := &queryScope{parent: parent}
	if parent != nil {
		sc.depth = parent.depth + 1
	}
	nodes := innerNodes(list)
	defs, start := parseWith(nodes)
	sc.ctes = defs
	if sc.depth > maxResolveDepth {
		return sc
	}
	if pos != nil {
		for _, d := range defs {
			if d.body != nil && token.ComparePos(*pos, d.body.Pos()) > 0 && token.ComparePos(*pos, d.body.End()) < 0 {
				// pos is in a CTE body: the main query is not an enclosing query of it
				return sc
			}
		}
	}
	b := branch(nodes[start:], pos)
	sc.collect(b)
	if !sc.sawTableKeyword {
		// The FROM keyword may be swallowed by an unterminated quote while
		// typing (SELECT ` FROM (…) t): take top-level aliased subqueries.
		for _, n := range b {
			if a, ok := n.(*ast.Aliased); ok && isSubQueryList(a.RealName) {
				sc.addRef(a, false)
			}
		}
	}
	return sc
}

// collect gathers the table references of nodes (recursing into everything but
// subqueries and function calls, which have scopes of their own).
func (s *queryScope) collect(nodes []ast.Node) {
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		kw := keywordOf(n)
		isJoin := joinKeywords[kw]
		if tableKeywords[kw] || isJoin {
			s.sawTableKeyword = true
			j, ref := nextNonWS(nodes, i)
			if ref != nil && isWord(ref, "LATERAL", "ONLY") {
				j, ref = nextNonWS(nodes, j)
			}
			if ref == nil {
				return
			}
			s.addRef(ref, isJoin)
			if isJoin && len(s.joined) > 0 {
				if k, on := nextNonWS(nodes, j); on != nil && isKeyword(on, "ON") {
					if _, cond := nextNonWS(nodes, k); cond != nil && cond.Type() == ast.TypeComparison {
						s.joined[len(s.joined)-1].hasOn = true
					}
				}
			}
			i = j
			continue
		}
		if isSubQueryList(n) {
			continue
		}
		switch n.(type) {
		case *ast.FunctionLiteral:
			continue
		}
		if list, ok := n.(ast.TokenList); ok {
			s.collect(list.GetTokens())
		}
	}
}

func (s *queryScope) addRef(n ast.Node, joined bool) {
	switch v := n.(type) {
	case *ast.Identifier:
		s.addNamed(n, "", v.NoQuoteString(), "", joined)
	case *ast.MemberIdentifier:
		if v.Parent == nil || v.Child == nil {
			return
		}
		s.addNamed(n, v.Parent.String(), v.GetChild().String(), "", joined)
	case *ast.Aliased:
		alias := ""
		if name, ok := identName(v.AliasedName); ok {
			alias = name
		}
		switch real := v.RealName.(type) {
		case *ast.Identifier:
			s.addNamed(n, "", real.NoQuoteString(), alias, joined)
		case *ast.MemberIdentifier:
			if real.Parent == nil || real.Child == nil {
				return
			}
			s.addNamed(n, real.Parent.String(), real.GetChild().String(), alias, joined)
		case *ast.Parenthesis:
			if isSubQueryList(real) {
				s.addDerived(n, real, alias, joined)
			}
		}
	case *ast.Parenthesis:
		if isSubQueryList(v) {
			s.addDerived(n, v, "", joined)
		}
	case *ast.IdentifierList:
		for _, id := range v.GetIdentifiers() {
			s.addRef(id, joined)
		}
	}
}

func (s *queryScope) addNamed(n ast.Node, schema, name, alias string, joined bool) {
	if schema == "" {
		if def := s.findCTE(name); def != nil {
			visible := name
			if alias != "" {
				visible = alias
			}
			s.rels = append(s.rels, &SubQueryInfo{
				Name:  visible,
				Kind:  SubQueryCTE,
				CTE:   def.name,
				Views: []*SubQueryView{{SubQueryColumns: def.columns(s)}},
			})
			if joined {
				s.joined = append(s.joined, joinedRef{node: n})
			}
			return
		}
	}
	ti := &TableInfo{DatabaseSchema: schema, Name: name, Alias: alias}
	s.tables = append(s.tables, ti)
	if joined {
		s.joined = append(s.joined, joinedRef{node: n, table: ti})
	}
}

func (s *queryScope) addDerived(n ast.Node, body *ast.Parenthesis, alias string, joined bool) {
	s.rels = append(s.rels, &SubQueryInfo{
		Name:  alias,
		Kind:  SubQueryDerived,
		Views: []*SubQueryView{{SubQueryColumns: selectColumns(body, s)}},
	})
	if joined {
		s.joined = append(s.joined, joinedRef{node: n})
	}
}

// columns resolves (once) the output columns of a CTE.
func (d *cteDef) columns(from *queryScope) []*SubQueryColumn {
	if d.resolved {
		return d.cols
	}
	if d.resolving || d.body == nil {
		// recursive reference: only the explicit column list is known
		return namedColumns(d.colNames)
	}
	d.resolving = true
	cols := selectColumns(d.body, from.definingScope(d))
	d.resolving = false
	if d.colNames != nil {
		renamed := make([]*SubQueryColumn, len(d.colNames))
		for i, name := range d.colNames {
			c := &SubQueryColumn{ColumnName: name}
			if i < len(cols) && cols[i].ColumnName != "*" {
				cp := *cols[i]
				cp.AliasName = name
				if cp.ColumnName == "" {
					cp.ColumnName = name
				}
				c = &cp
			}
			renamed[i] = c
		}
		cols = renamed
	}
	d.cols, d.resolved = cols, true
	return cols
}

// definingScope returns the scope that defines d (so its body sees the CTEs of
// the same WITH clause and the enclosing ones).
func (s *queryScope) definingScope(d *cteDef) *queryScope {
	for sc := s; sc != nil; sc = sc.parent {
		for _, c := range sc.ctes {
			if c == d {
				return sc
			}
		}
	}
	return s
}

func namedColumns(names []string) []*SubQueryColumn {
	cols := make([]*SubQueryColumn, len(names))
	for i, n := range names {
		cols[i] = &SubQueryColumn{ColumnName: n}
	}
	return cols
}

// ---- select list ----------------------------------------------------------------

// selectColumns returns the output columns of a query body (its first SELECT
// branch). Columns whose name can't be known (unaliased expressions) are left
// out; star columns of database tables are kept as "*" with their table, to be
// expanded against the schema cache.
func selectColumns(body ast.TokenList, outer *queryScope) []*SubQueryColumn {
	if outer != nil && outer.depth >= maxResolveDepth {
		return nil
	}
	sc := buildScope(body, outer, nil)
	nodes := innerNodes(body)
	_, start := parseWith(nodes)
	nodes = branch(nodes[start:], nil)

	// SELECT [DISTINCT | ALL | UNIQUE | DISTINCTROW] [ON (…)] [TOP n] list
	i, n := nextNonWS(nodes, -1)
	if n == nil || !isKeyword(n, "SELECT") {
		return nil
	}
	for {
		i, n = nextNonWS(nodes, i)
		if n == nil {
			return nil
		}
		if isKeyword(n, "DISTINCT", "ALL", "UNIQUE", "DISTINCTROW") {
			continue
		}
		if isKeyword(n, "ON", "TOP") { // DISTINCT ON (…), TOP n: skip the argument too
			i, _ = nextNonWS(nodes, i)
			continue
		}
		break
	}
	if isKeyword(n, "FROM") {
		return nil
	}

	var items []ast.Node
	if il, ok := n.(*ast.IdentifierList); ok {
		items = il.GetIdentifiers()
	} else {
		items = []ast.Node{n}
	}
	var cols []*SubQueryColumn
	for _, item := range items {
		cols = append(cols, sc.itemColumns(item)...)
	}
	return cols
}

func (s *queryScope) itemColumns(item ast.Node) []*SubQueryColumn {
	switch v := item.(type) {
	case *ast.Identifier:
		name := v.NoQuoteString()
		if name == "*" {
			return s.starColumns("")
		}
		return []*SubQueryColumn{s.resolveColumn("", name)}
	case *ast.MemberIdentifier:
		if v.Parent == nil || v.Child == nil {
			return nil
		}
		parent := v.GetParentIdent().NoQuoteString()
		child := v.GetChildIdent().NoQuoteString()
		if child == "*" {
			return s.starColumns(parent)
		}
		return []*SubQueryColumn{s.resolveColumn(parent, child)}
	case *ast.Aliased:
		alias, ok := identName(v.AliasedName)
		if !ok || alias == "" {
			return nil
		}
		var col *SubQueryColumn
		switch real := v.RealName.(type) {
		case *ast.Identifier:
			col = s.resolveColumn("", real.NoQuoteString())
		case *ast.MemberIdentifier:
			if real.Parent != nil && real.Child != nil {
				col = s.resolveColumn(real.GetParentIdent().NoQuoteString(), real.GetChildIdent().NoQuoteString())
			}
		}
		if col == nil {
			// expression: only the name is known
			return []*SubQueryColumn{{ColumnName: alias}}
		}
		cp := *col
		cp.AliasName = alias
		return []*SubQueryColumn{&cp}
	}
	return nil
}

// resolveColumn finds where a column referenced in a select list comes from.
func (s *queryScope) resolveColumn(parent, name string) *SubQueryColumn {
	col := &SubQueryColumn{ParentName: parent, ColumnName: name}
	if parent != "" {
		for _, t := range s.tables {
			if t.isMatchTableNameFold(parent) {
				col.ParentTable = t
				return col
			}
		}
		for _, r := range s.rels {
			if strings.EqualFold(r.Name, parent) {
				if src := r.findColumn(name); src != nil {
					return originOf(src, parent, name)
				}
				return col
			}
		}
		return col
	}
	for _, r := range s.rels {
		if src := r.findColumn(name); src != nil {
			return originOf(src, "", name)
		}
	}
	if len(s.tables) == 1 && len(s.rels) == 0 {
		col.ParentTable = s.tables[0]
	}
	return col
}

// originOf: a column taken from a derived table/CTE keeps the origin of the
// source column (for documentation), under the name it is referenced by.
func originOf(src *SubQueryColumn, parent, name string) *SubQueryColumn {
	c := &SubQueryColumn{ParentName: parent, ColumnName: name, ParentTable: src.ParentTable}
	if src.ParentTable != nil && src.ColumnName != "" && !strings.EqualFold(src.ColumnName, name) {
		// referenced under the derived name, defined as another column
		c.ColumnName = src.ColumnName
		c.AliasName = name
	}
	return c
}

func (s *queryScope) starColumns(parent string) []*SubQueryColumn {
	var cols []*SubQueryColumn
	for _, t := range s.tables {
		if parent == "" || t.isMatchTableNameFold(parent) {
			cols = append(cols, &SubQueryColumn{ParentName: parent, ColumnName: "*", ParentTable: t})
		}
	}
	for _, r := range s.rels {
		if parent == "" || strings.EqualFold(r.Name, parent) {
			cols = append(cols, r.columns()...)
		}
	}
	return cols
}

func (ti *TableInfo) isMatchTableNameFold(name string) bool {
	if ti.Alias != "" {
		return strings.EqualFold(ti.Alias, name)
	}
	return strings.EqualFold(ti.Name, name)
}

func (si *SubQueryInfo) columns() []*SubQueryColumn {
	var cols []*SubQueryColumn
	for _, v := range si.Views {
		cols = append(cols, v.SubQueryColumns...)
	}
	return cols
}

// findColumn returns the column of a derived table/CTE by its visible name.
func (si *SubQueryInfo) findColumn(name string) *SubQueryColumn {
	for _, c := range si.columns() {
		if c.ColumnName == "*" {
			continue
		}
		if strings.EqualFold(c.DisplayName(), name) {
			return c
		}
	}
	return nil
}

// ---- entry points -------------------------------------------------------------

// scopeAt builds the scopes from the statement down to the innermost query
// level enclosing pos; the last one is the position's own level.
func scopeAt(parsed ast.TokenList, pos token.Pos) ([]*queryScope, error) {
	scopes, _, err := scopeAndLevelsAt(parsed, pos)
	return scopes, err
}

func scopeAndLevelsAt(parsed ast.TokenList, pos token.Pos) ([]*queryScope, []ast.TokenList, error) {
	stmt, err := extractFocusedStatement(parsed, pos)
	if err != nil {
		return nil, nil, err
	}
	levels := queryLevels(stmt, pos)
	scopes := make([]*queryScope, 0, len(levels))
	var parent *queryScope
	for _, level := range levels {
		p := pos
		sc := buildScope(level, parent, &p)
		scopes = append(scopes, sc)
		parent = sc
	}
	return scopes, levels, nil
}

// ExtractCTEs returns the WITH queries visible at pos, with their columns.
func ExtractCTEs(parsed ast.TokenList, pos token.Pos) ([]*SubQueryInfo, error) {
	scopes, err := scopeAt(parsed, pos)
	if err != nil {
		return nil, err
	}
	cur := scopes[len(scopes)-1]
	var res []*SubQueryInfo
	for _, d := range cur.visibleCTEs() {
		res = append(res, &SubQueryInfo{
			Name:  d.name,
			Kind:  SubQueryCTE,
			CTE:   d.name,
			Views: []*SubQueryView{{SubQueryColumns: d.columns(cur)}},
		})
	}
	return res, nil
}

// CTEDefinition returns the node naming the WITH query name visible at pos
// (for go-to-definition).
func CTEDefinition(parsed ast.TokenList, pos token.Pos, name string) ast.Node {
	scopes, err := scopeAt(parsed, pos)
	if err != nil {
		return nil
	}
	if d := scopes[len(scopes)-1].findCTE(name); d != nil {
		return d.nameNode
	}
	return nil
}
