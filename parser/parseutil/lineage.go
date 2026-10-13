package parseutil

import (
	"strings"

	"github.com/raikhan/sqlh/ast"
)

// QueryLineage describes the output of a query, e.g. a view's SELECT.
type QueryLineage struct {
	// Columns of the (first) SELECT branch. ParentTable is set for plain column
	// references to a database table ("*" columns included, to be expanded
	// against the schema); expressions have only a name.
	Columns []*SubQueryColumn
	// Compound: the query has set operators (UNION, …), so its rows may also
	// come from other tables than the first branch's.
	Compound bool
	// Tables: the database tables referenced anywhere in the query (FROM/JOIN
	// of every level, CTE bodies included; CTE names excluded).
	Tables []*TableInfo
}

// ExtractQueryLineage analyses the first statement of parsed.
func ExtractQueryLineage(parsed ast.TokenList) *QueryLineage {
	var stmt ast.TokenList
	for _, n := range parsed.GetTokens() {
		if s, ok := n.(*ast.Statement); ok {
			stmt = s
			break
		}
	}
	if stmt == nil {
		stmt = parsed
	}
	res := &QueryLineage{Columns: selectColumns(stmt, nil)}
	nodes := innerNodes(stmt)
	_, start := parseWith(nodes)
	for _, n := range nodes[start:] {
		if setOperators[keywordOf(n)] {
			res.Compound = true
		}
	}
	ctes := map[string]bool{}
	collectCTENames(stmt, ctes)
	seen := map[string]bool{}
	collectAllTables(stmt, ctes, seen, &res.Tables)
	return res
}

func collectCTENames(list ast.TokenList, names map[string]bool) {
	defs, _ := parseWith(innerNodes(list))
	for _, d := range defs {
		names[strings.ToUpper(d.name)] = true
	}
	for _, n := range list.GetTokens() {
		if l, ok := n.(ast.TokenList); ok {
			collectCTENames(l, names)
		}
	}
}

// collectAllTables walks every query level (all set-operator branches) for the
// table references following FROM/JOIN/….
func collectAllTables(list ast.TokenList, ctes, seen map[string]bool, out *[]*TableInfo) {
	sc := &queryScope{depth: maxResolveDepth} // no derived-table resolution needed
	sc.collect(innerNodes(list))
	for _, t := range sc.tables {
		if t.DatabaseSchema == "" && ctes[strings.ToUpper(t.Name)] {
			continue
		}
		key := strings.ToUpper(t.DatabaseSchema + "." + t.Name)
		if !seen[key] {
			seen[key] = true
			*out = append(*out, t)
		}
	}
	var walk func(nodes []ast.Node)
	walk = func(nodes []ast.Node) {
		for _, n := range nodes {
			if isSubQueryList(n) {
				collectAllTables(n.(ast.TokenList), ctes, seen, out)
				continue
			}
			if l, ok := n.(ast.TokenList); ok {
				walk(l.GetTokens())
			}
		}
	}
	walk(innerNodes(list))
}
