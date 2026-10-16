package parseutil

import (
	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/token"
)

type SyntaxPosition string

const (
	ColName        SyntaxPosition = "col_name"
	SelectExpr     SyntaxPosition = "select_expr"
	AliasName      SyntaxPosition = "alias_name"
	WhereCondition SyntaxPosition = "where_condition"
	CaseValue      SyntaxPosition = "case_value"
	TableReference SyntaxPosition = "table_reference"
	InsertColumn   SyntaxPosition = "insert_column"
	InsertValue    SyntaxPosition = "insert_value"
	JoinClause     SyntaxPosition = "join_clause"
	JoinOn         SyntaxPosition = "join_on"
	Unknown        SyntaxPosition = "unknown"
)

// CheckSyntaxPosition classifies the cursor position by the keyword before
// it. Inside parentheses only the keywords inside them count, so a subquery
// isn't taken for the clause around it (FROM (SELECT … | was a table
// position, WITH x AS (SEL| an alias); when nothing inside matches:
//   - an INSERT column list or a VALUES list: InsertColumn / InsertValue;
//   - a subquery (the parentheses start with SELECT/WITH): Unknown, like the
//     same position outside parentheses (keywords: WHERE, UNION, …);
//   - otherwise (an expression, or the start of the parentheses): the
//     position around them (WHERE (a = 1 → columns).
func CheckSyntaxPosition(nw *NodeWalker) SyntaxPosition {
	k, ok := nw.innermostParen()
	if !ok {
		return keywordPosition(nw)
	}
	if res := keywordPosition(&NodeWalker{Paths: nw.Paths[k+1:]}); res != Unknown {
		return res
	}
	switch {
	case isInsertValues(nw, k):
		return InsertValue
	case isInsertColumns(nw, k):
		return InsertColumn
	case isSubQueryList(nw.Paths[k].CurNode):
		return Unknown
	}
	// the position of the parentheses themselves (their level's keywords)
	return CheckSyntaxPosition(&NodeWalker{Paths: nw.Paths[:k+1]})
}

// AtParenStart reports whether the cursor is at the start of parentheses
// (nothing but the word being typed after "("), where a subquery may begin.
func AtParenStart(nw *NodeWalker) bool {
	k, ok := nw.innermostParen()
	if !ok || len(nw.Paths) != k+2 {
		return false
	}
	r := nw.Paths[k+1]
	lparen := genTokenMatcher([]token.Kind{token.LParen})
	if r.CurNodeIs(lparen) {
		return true
	}
	switch n := r.CurNode.(type) {
	case *ast.Identifier:
	case *ast.Item:
		if !isWhitespaceNode(n) && !n.Tok.MatchKind(token.SQLKeyword) {
			return false
		}
	default:
		return false
	}
	return r.PrevNodeIs(true, lparen)
}

// OnImplicitAlias reports whether the cursor is on a name following a table
// or column without AS (FROM t wh|): an alias as parsed, though it is often
// the next keyword being typed.
func OnImplicitAlias(nw *NodeWalker) bool {
	for i := len(nw.Paths) - 2; i >= 0; i-- {
		a, ok := nw.Paths[i].CurNode.(*ast.Aliased)
		if !ok {
			continue
		}
		return !a.IsAs && nw.Paths[i+1].CurNode == a.AliasedName
	}
	return false
}

// innermostParen is the level of the innermost parentheses holding the
// cursor; right after ")" is outside them.
func (nw *NodeWalker) innermostParen() (int, bool) {
	for i := len(nw.Paths) - 1; i >= 0; i-- {
		if _, ok := nw.Paths[i].CurNode.(*ast.Parenthesis); !ok || i+1 >= len(nw.Paths) {
			continue // not a parenthesis, or the cursor is not inside it
		}
		if i+1 < len(nw.Paths) && nw.Paths[i+1].CurNodeIs(genTokenMatcher([]token.Kind{token.RParen})) {
			continue
		}
		return i, true
	}
	return 0, false
}

func keywordPosition(nw *NodeWalker) SyntaxPosition {
	var res SyntaxPosition
	switch {
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		// UPDATE Statement
		"SET",
		// SELECT Statement
		"ORDER BY",
		"GROUP BY",
	})):
		res = ColName
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		// SELECT Statement
		"ALL",
		"DISTINCT",
		"DISTINCTROW",
		"SELECT",
	})) && !afterSetOperator(nw):
		res = SelectExpr
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		// Alias
		"AS",
	})):
		res = AliasName
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		// WHERE Clause
		"WHERE",
		"HAVING",
		// Operator
		"AND",
		"OR",
		"XOR",
	})):
		res = WhereCondition
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		// CASE Statement
		"CASE",
		"WHEN",
		"THEN",
		"ELSE",
	})):
		res = CaseValue
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		// SELECT Statement
		"FROM",
		// UPDATE Statement
		"UPDATE",
		// DELETE Statement
		"DELETE FROM",
		// INSERT Statement
		"INSERT INTO",
		// JOIN Clause
		"CROSS JOIN",
		// DESCRIBE Statement
		"DESCRIBE",
		"DESC",
		// TRUNCATE Statement
		"TRUNCATE",
	})):
		res = TableReference
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		"ON",
	})):
		res = getJoinOnCondition(nw)
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		"JOIN",
		"INNER JOIN",
		"OUTER JOIN",
		"LEFT JOIN",
		"RIGHT JOIN",
		"LEFT OUTER JOIN",
		"RIGHT OUTER JOIN",
		"FULL JOIN",
		"FULL OUTER JOIN",
		"NATURAL JOIN",
	})):
		res = getJoinCondition(nw)
	default:
		res = Unknown
	}
	return res
}

func getJoinCondition(nw *NodeWalker) SyntaxPosition {
	for _, n := range nw.Paths {
		if n.PeekNodeIs(true, genKeywordMatcher([]string{"ON"})) {
			return TableReference
		}
	}
	return JoinClause
}
func getJoinOnCondition(nw *NodeWalker) SyntaxPosition {
	switch {
	case nw.CurNodeIs(genTokenMatcher([]token.Kind{token.Period})):
		return ColName
	case nw.CurNodeIs(genTokenMatcher([]token.Kind{token.Whitespace})):
		if !nw.PrevNodesIs(true, astutil.NodeMatcher{
			ExpectTokens: []token.Kind{token.Eq}}) {
			return JoinOn
		}
	}
	return WhereCondition
}

func genKeywordMatcher(keywords []string) astutil.NodeMatcher {
	return astutil.NodeMatcher{
		ExpectKeyword: keywords,
	}
}

func genTokenMatcher(tokens []token.Kind) astutil.NodeMatcher {
	return astutil.NodeMatcher{
		ExpectTokens: tokens,
	}
}

// afterSetOperator: UNION ALL | / UNION DISTINCT | — the next SELECT is
// still to come (not SELECT ALL |).
func afterSetOperator(nw *NodeWalker) bool {
	for _, r := range nw.Paths {
		prev := prevSignificant(r, 2)
		if len(prev) == 2 && isKeyword(prev[0], "ALL", "DISTINCT") &&
			(setOperators[keywordOf(prev[1])] || isWord(prev[1], "MINUS")) {
			return true
		}
	}
	return false
}

// prevSignificant: up to n nodes before the current one of a level,
// nearest first, whitespace and comments skipped.
func prevSignificant(r *astutil.NodeReader, n int) []ast.Node {
	toks := r.Node.GetTokens()
	var out []ast.Node
	for i := r.Index - 2; i >= 0 && len(out) < n; i-- {
		if !isWhitespaceNode(toks[i]) {
			out = append(out, toks[i])
		}
	}
	return out
}

// isInsertColumns: the parentheses at level k are INSERT INTO t (…).
func isInsertColumns(nw *NodeWalker, k int) bool {
	prev := prevSignificant(nw.Paths[k], 2)
	return len(prev) == 2 && isKeyword(prev[1], "INSERT INTO", "INTO") &&
		(prev[0].Type() == ast.TypeIdentifier || prev[0].Type() == ast.TypeMemberIdentifier)
}

// isInsertValues: the parentheses at level k are a row of VALUES (…), (…).
func isInsertValues(nw *NodeWalker, k int) bool {
	r := nw.Paths[k]
	prev := prevSignificant(r, 1)
	if len(prev) == 0 {
		return false
	}
	if isKeyword(prev[0], "VALUES") {
		return true
	}
	if it, ok := prev[0].(*ast.Item); !ok || !it.Tok.MatchKind(token.Comma) {
		return false
	}
	// a later row: the list before it starts with VALUES
	toks := r.Node.GetTokens()
	for i := r.Index - 2; i >= 0; i-- {
		switch n := toks[i].(type) {
		case *ast.Parenthesis:
			continue
		case *ast.Item:
			if isKeyword(n, "VALUES") {
				return true
			}
			if n.Tok.MatchKind(token.Comma) || isWhitespaceNode(n) {
				continue
			}
		}
		return false
	}
	return false
}
