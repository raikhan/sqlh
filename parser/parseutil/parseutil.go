package parseutil

import (
	"fmt"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/token"
)

type TableInfo struct {
	DatabaseSchema  string
	Name            string
	Alias           string
	SubQueryColumns []*SubQueryColumn
	// Outer: visible from an enclosing query level (correlated reference).
	Outer bool
}

func (ti *TableInfo) isMatchTableName(name string) bool {
	if ti.Name == name {
		return true
	}
	if ti.Alias == name {
		return true
	}
	return false
}

type SubQueryKind int

const (
	// SubQueryDerived: a subquery in FROM/JOIN, (SELECT …) alias.
	SubQueryDerived SubQueryKind = iota
	// SubQueryCTE: a WITH query, or a reference to one in FROM/JOIN.
	SubQueryCTE
)

type SubQueryInfo struct {
	// Name the relation is referenced by (alias, or the CTE name).
	Name  string
	Views []*SubQueryView
	Kind  SubQueryKind
	// CTE: the name of the WITH query (Kind == SubQueryCTE).
	CTE string
	// Outer: visible from an enclosing query level (correlated reference).
	Outer bool
}

type SubQueryView struct {
	SubQueryColumns []*SubQueryColumn
}

type SubQueryColumn struct {
	ParentTable *TableInfo
	ParentName  string
	ColumnName  string
	AliasName   string
}

func (sc *SubQueryColumn) DisplayName() string {
	name := sc.ColumnName
	if sc.AliasName != "" {
		name = sc.AliasName
	}
	return name
}

func extractFocusedStatement(parsed ast.TokenList, pos token.Pos) (ast.TokenList, error) {
	nodeWalker := NewNodeWalker(parsed, pos)
	matcher := astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeStatement}}
	if !nodeWalker.CurNodeIs(matcher) {
		return nil, fmt.Errorf("not found statement, Node: %q, Position: (%d, %d)", parsed.String(), pos.Line, pos.Col)
	}
	stmt := nodeWalker.CurNodeTopMatched(matcher).(ast.TokenList)
	return stmt, nil
}

func encloseIsSubQuery(stmt ast.TokenList, pos token.Pos) bool {
	nodeWalker := NewNodeWalker(stmt, pos)
	matcher := astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeParenthesis}}
	if !nodeWalker.CurNodeIs(matcher) {
		return false
	}
	parenthesis := nodeWalker.CurNodeBottomMatched(matcher)
	tokenList, ok := parenthesis.(ast.TokenList)
	if !ok {
		return false
	}
	return isSubQuery(tokenList)
}

func isSubQuery(tokenList ast.TokenList) bool {
	reader := astutil.NewNodeReader(tokenList)
	if !reader.NextNode(false) {
		return false
	}
	if !reader.NextNode(false) {
		return false
	}
	if !reader.CurNodeIs(astutil.NodeMatcher{ExpectKeyword: []string{"SELECT"}}) {
		return false
	}
	return true
}

// ExtractSubQueryViews returns the derived tables and CTE references visible
// at pos: those of its own query level first, then (Outer) those of the
// enclosing levels.
func ExtractSubQueryViews(parsed ast.TokenList, pos token.Pos) ([]*SubQueryInfo, error) {
	scopes, err := scopeAt(parsed, pos)
	if err != nil {
		return nil, err
	}
	var res []*SubQueryInfo
	for i := len(scopes) - 1; i >= 0; i-- {
		for _, r := range scopes[i].rels {
			cp := *r
			cp.Outer = i != len(scopes)-1
			res = append(res, &cp)
		}
	}
	return res, nil
}

// ExtractTable returns the database tables visible at pos: those of its own
// query level first, then (Outer) those of the enclosing levels. Derived
// tables and CTE references are not included (see ExtractSubQueryViews).
func ExtractTable(parsed ast.TokenList, pos token.Pos) ([]*TableInfo, error) {
	scopes, err := scopeAt(parsed, pos)
	if err != nil {
		return nil, err
	}
	res := []*TableInfo{}
	seen := map[string]bool{}
	for i := len(scopes) - 1; i >= 0; i-- {
		for _, t := range scopes[i].tables {
			key := t.DatabaseSchema + "\t" + t.Name + "\t" + t.Alias
			if seen[key] {
				continue
			}
			seen[key] = true
			cp := *t
			cp.Outer = i != len(scopes)-1
			res = append(res, &cp)
		}
	}
	return res, nil
}

// ExtractPrevTables returns the database tables of pos's query level that are
// referenced before pos.
func ExtractPrevTables(parsed ast.TokenList, pos token.Pos) ([]*TableInfo, error) {
	scopes, levels, err := scopeAndLevelsAt(parsed, pos)
	if err != nil {
		return nil, err
	}
	sc := scopes[len(scopes)-1]
	cur := &queryScope{parent: sc.parent, ctes: sc.ctes, depth: sc.depth}
	nodes := innerNodes(levels[len(levels)-1])
	_, start := parseWith(nodes)
	cur.collect(nodesBefore(branch(nodes[start:], &pos), pos))
	return cur.tables, nil
}

// nodesBefore returns the nodes starting before pos (recursively trimmed).
func nodesBefore(nodes []ast.Node, pos token.Pos) []ast.Node {
	var res []ast.Node
	for _, n := range nodes {
		if token.ComparePos(n.Pos(), pos) > 0 {
			break
		}
		res = append(res, n)
	}
	return res
}

// ExtractLastTable returns the table joined last before pos in pos's query
// level, unless its join condition is already written.
func ExtractLastTable(parsed ast.TokenList, pos token.Pos) (*TableInfo, error) {
	scopes, err := scopeAt(parsed, pos)
	if err != nil {
		return nil, err
	}
	var res *TableInfo
	for _, j := range scopes[len(scopes)-1].joined {
		p := j.node.Pos()
		if token.ComparePos(p, pos) > 0 {
			continue
		}
		if j.hasOn {
			continue
		}
		res = j.table // nil for derived tables and CTEs: no foreign keys
	}
	return res, nil
}
