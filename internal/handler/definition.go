package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
	"github.com/sourcegraph/jsonrpc2"
)

func (s *Server) handleDefinition(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DefinitionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	f, ok := s.files[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	return definition(params.TextDocument.URI, f.Text, params, s.worker.Cache(), s.viewFile)
}

// viewFiler writes a view's definition to a file, returning its path and text.
type viewFiler func(v *database.ViewDef) (path, text string, err error)

func definition(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache, views viewFiler) (lsp.Definition, error) {
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	parsed, err := parser.Parse(text)
	if err != nil {
		return nil, err
	}

	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	m := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeIdentifier},
	}
	var define ast.Node
	if currentVariable := nodeWalker.CurNodeBottomMatched(m); currentVariable != nil {
		define = aliasDefinition(parsed, pos, currentVariable.String())
	}

	if define == nil {
		if views == nil || dbCache == nil {
			return nil, nil
		}
		// a view (or a view's column): its definition, from the database
		v, column := viewTarget(parsed, pos, nodeWalker, dbCache)
		if v == nil {
			return nil, nil
		}
		path, body, err := views(v)
		if err != nil {
			return nil, err
		}
		line, from, to := viewColumnPosition(body, column)
		return []lsp.Location{{
			URI: "file://" + path,
			Range: lsp.Range{
				Start: lsp.Position{Line: line, Character: from},
				End:   lsp.Position{Line: line, Character: to},
			},
		}}, nil
	}

	res := []lsp.Location{
		{
			URI: url,
			Range: lsp.Range{
				Start: lsp.Position{
					Line:      define.Pos().Line,
					Character: define.Pos().Col,
				},
				End: lsp.Position{
					Line:      define.End().Line,
					Character: define.End().Col,
				},
			},
		},
	}

	return res, nil
}

// aliasDefinition: where a WITH query name visible at the position, or an
// alias, is defined.
func aliasDefinition(parsed ast.TokenList, pos token.Pos, name string) ast.Node {
	if define := parseutil.CTEDefinition(parsed, pos, name); define != nil {
		return define
	}
	for _, v := range parseutil.ExtractAliased(parsed) {
		alias, _ := v.(*ast.Aliased)
		if alias.AliasedName.String() == name {
			return alias.AliasedName
		}
	}
	return nil
}
