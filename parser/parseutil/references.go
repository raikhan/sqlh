package parseutil

import (
	"strings"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
)

// Statements returns the top-level statements of parsed.
func Statements(parsed ast.TokenList) []ast.TokenList {
	res := []ast.TokenList{}
	for _, n := range parsed.GetTokens() {
		if s, ok := n.(*ast.Statement); ok {
			res = append(res, s)
		}
	}
	return res
}

// Identifiers returns every identifier node in list (members of qualified
// names included, e.g. both "p" and "id" of "p.id").
func Identifiers(list ast.TokenList) []*ast.Identifier {
	matcher := astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeIdentifier}}
	res := []*ast.Identifier{}
	for _, n := range parsePrefix(astutil.NewNodeReader(list), matcher, parseIdentifier) {
		if id, ok := n.(*ast.Identifier); ok {
			res = append(res, id)
		}
	}
	return res
}

// StatementTables returns the database tables referenced in stmt (FROM/JOIN
// of every query level, CTE bodies included; CTE names excluded), one entry
// per reference, so a table joined twice under two aliases appears twice.
func StatementTables(stmt ast.TokenList) []*TableInfo {
	ctes := map[string]bool{}
	collectCTENames(stmt, ctes)
	res := []*TableInfo{}
	var levels func(list ast.TokenList)
	levels = func(list ast.TokenList) {
		sc := &queryScope{depth: maxResolveDepth}
		sc.collect(innerNodes(list))
		for _, t := range sc.tables {
			if t.DatabaseSchema == "" && ctes[strings.ToUpper(t.Name)] {
				continue
			}
			res = append(res, t)
		}
		var walk func(nodes []ast.Node)
		walk = func(nodes []ast.Node) {
			for _, n := range nodes {
				if isSubQueryList(n) {
					levels(n.(ast.TokenList))
					continue
				}
				if l, ok := n.(ast.TokenList); ok {
					walk(l.GetTokens())
				}
			}
		}
		walk(innerNodes(list))
	}
	levels(stmt)
	return res
}

// AliasDeclarations returns the alias name nodes (the "p" of "tbl p" / "tbl AS
// p") declared in list.
func AliasDeclarations(list ast.TokenList) []ast.Node {
	res := []ast.Node{}
	for _, n := range ExtractAliased(list) {
		if a, ok := n.(*ast.Aliased); ok && a.AliasedName != nil {
			res = append(res, a.AliasedName)
		}
	}
	return res
}
