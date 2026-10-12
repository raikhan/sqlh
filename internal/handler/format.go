package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/raikhan/sqlh/internal/formatter"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/sourcegraph/jsonrpc2"
)

func (s *Server) handleTextDocumentFormatting(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DocumentFormattingParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	f, ok := s.files[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	// The formatter's lexer does not understand some dialect constructs and
	// would corrupt them; leave such documents untouched and say why.
	if hz := sqlsplit.FormatHazards(f.Text); len(hz) > 0 {
		msg := fmt.Sprintf("format skipped: %s on line %d", hz[0].What, hz[0].Line+1)
		if len(hz) > 1 {
			msg += fmt.Sprintf(" (+%d more)", len(hz)-1)
		}
		_ = lsp.NewMessenger(conn).ShowWarning(ctx, msg+"; the formatter would corrupt it")
		return nil, nil
	}

	textEdits, err := formatter.Format(f.Text, params, s.getConfig())
	if err != nil {
		return nil, err
	}
	if len(textEdits) > 0 {
		return textEdits, nil
	}
	return nil, nil
}

func (s *Server) handleTextDocumentRangeFormatting(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DocumentRangeFormattingParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	_, ok := s.files[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	textEdits := []lsp.TextEdit{}
	if len(textEdits) > 0 {
		return textEdits, nil
	}
	return nil, nil
}
