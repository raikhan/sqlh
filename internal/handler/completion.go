package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/raikhan/sqlh/internal/jsoncomplete"
	"github.com/raikhan/sqlh/internal/sqlsplit"

	"github.com/raikhan/sqlh/internal/completer"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/sourcegraph/jsonrpc2"
)

func (s *Server) handleTextDocumentCompletion(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.CompletionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	f, ok := s.files[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	// JSON keys inside json/jsonb operators and SQL/JSON paths
	var d sqlsplit.Dialect
	if s.curDBCfg != nil {
		d = sqlsplit.DialectOf(string(s.curDBCfg.Driver))
	}
	line, char := params.Position.Line, params.Position.Character
	if jc, ok := jsoncomplete.Detect(f.Text, line, char, d); ok {
		return s.jsonCompletion(f.Text, params, jc, d)
	}
	if jsoncomplete.InString(f.Text, line, char, d) {
		return []lsp.CompletionItem{}, nil // inside an ordinary string literal
	}

	c := completer.NewCompleter(s.worker.Cache())
	c.LowercaseIdentifiers = s.getConfig().LowercaseIdentifiers
	c.HideMaybe = !s.getConfig().Relations.MaybeEnabled()
	if s.dbConn != nil {
		c.Driver = s.dbConn.Driver
	} else {
		c.Driver = ""
	}
	completionItems, err := c.Complete(f.Text, params, s.getConfig().LowercaseKeywords)
	if err != nil {
		return nil, err
	}
	return completionItems, nil
}
