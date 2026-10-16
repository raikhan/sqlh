package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
	"github.com/sourcegraph/jsonrpc2"
)

// handleDefinition is called without s.mu (see Handle): a table's
// definition is fetched from the database, which must not block the CLI.
func (s *Server) handleDefinition(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}
	var params lsp.DefinitionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.trackActiveDocument(req)
	f, ok := s.files[params.TextDocument.URI]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}
	cache := s.worker.Cache()
	var d sqlsplit.Dialect
	deps := defDeps{cache: cache, conn: connAlias(s.curDBCfg)}
	if s.curDBCfg != nil {
		deps.driver = s.curDBCfg.Driver
		d = sqlsplit.DialectOf(string(deps.driver))
	}
	local, target, err := definitionLocal(params.TextDocument.URI, f.Text, params, cache, d)
	if err != nil || local != nil || target == nil {
		s.mu.Unlock()
		return local, err
	}
	if target.view == nil {
		if repo, err := s.newDBRepository(ctx); err == nil {
			deps.repo = repo
		}
	}
	key := deps.conn + "\t" + s.targetKey()
	_, deps.skipMetadata = s.noMetadataDDL.Load(key)
	deps.failed = func() { s.noMetadataDDL.Store(key, true) }
	workspace := s.workspace
	s.mu.Unlock()

	def, err := fetchDefinition(ctx, target, deps)
	if err != nil || def == nil {
		return nil, err
	}
	path, text, err := definitionFile(workspace, deps.conn, def)
	if err != nil {
		return nil, err
	}
	return []lsp.Location{definitionLocation(path, text, def, target.column)}, nil
}

// definitionLocal: the definition within the document (an alias, a WITH
// query name), else the table or view to show the definition of.
func definitionLocal(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache, d sqlsplit.Dialect) (lsp.Definition, *defTarget, error) {
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	parsed, err := parser.Parse(text)
	if err != nil {
		return nil, nil, err
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
		// a table or view (or one of its columns): its definition, from the database
		return nil, objectTarget(text, params.Position, parsed, pos, nodeWalker, dbCache, d), nil
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
	return res, nil, nil
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
