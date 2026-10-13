package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
	"github.com/sourcegraph/jsonrpc2"
)

func (s *Server) handleReferences(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.ReferenceParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	f, ok := s.files[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	return references(params.TextDocument.URI, f.Text, params)
}

// references finds the uses of the identifier at the position:
//   - an alias (or a column, CTE name, …): its occurrences in the statement;
//   - a table: its occurrences in every statement of the document that
//     references it, plus the uses of the aliases it is given there.
func references(url, text string, params lsp.ReferenceParams) ([]lsp.Location, error) {
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	parsed, err := parser.Parse(text)
	if err != nil {
		return nil, err
	}

	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	m := astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeIdentifier}}
	cur, ok := nodeWalker.CurNodeBottomMatched(m).(*ast.Identifier)
	if !ok || cur == nil {
		return nil, nil
	}
	name := cur.NoQuoteString()

	var curStmt ast.TokenList
	stmts := parseutil.Statements(parsed)
	for _, stmt := range stmts {
		if astutil.IsEnclose(stmt, pos) {
			curStmt = stmt
			break
		}
	}
	if curStmt == nil {
		return nil, nil
	}

	isAlias, isTable := false, false
	for _, t := range parseutil.StatementTables(curStmt) {
		if strings.EqualFold(t.Alias, name) {
			isAlias = true
		}
		if strings.EqualFold(t.Name, name) {
			isTable = true
		}
	}

	var found []ast.Node
	if isTable && !isAlias {
		for _, stmt := range stmts {
			names := []string{}
			for _, t := range parseutil.StatementTables(stmt) {
				if !strings.EqualFold(t.Name, name) {
					continue
				}
				names = append(names, name)
				if t.Alias != "" {
					names = append(names, t.Alias)
				}
			}
			if len(names) > 0 {
				found = append(found, matchIdentifiers(stmt, names, params.Context.IncludeDeclaration)...)
			}
		}
	} else {
		found = matchIdentifiers(curStmt, []string{name}, params.Context.IncludeDeclaration)
	}

	sort.SliceStable(found, func(i, j int) bool {
		return token.ComparePos(found[i].Pos(), found[j].Pos()) < 0
	})
	res := make([]lsp.Location, 0, len(found))
	for _, n := range found {
		res = append(res, lsp.Location{
			URI: url,
			Range: lsp.Range{
				Start: lsp.Position{Line: n.Pos().Line, Character: n.Pos().Col},
				End:   lsp.Position{Line: n.End().Line, Character: n.End().Col},
			},
		})
	}
	return res, nil
}

// matchIdentifiers returns the identifiers of stmt named one of names
// (case-insensitively), leaving out alias declarations unless decl.
func matchIdentifiers(stmt ast.TokenList, names []string, decl bool) []ast.Node {
	skip := map[ast.Node]bool{}
	if !decl {
		for _, n := range parseutil.AliasDeclarations(stmt) {
			skip[n] = true
		}
	}
	res := []ast.Node{}
	for _, id := range parseutil.Identifiers(stmt) {
		if skip[id] {
			continue
		}
		for _, name := range names {
			if strings.EqualFold(id.NoQuoteString(), name) {
				res = append(res, id)
				break
			}
		}
	}
	return res
}
