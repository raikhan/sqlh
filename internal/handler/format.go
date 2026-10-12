package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/raikhan/sqlh/internal/formatter"
	"github.com/raikhan/sqlh/internal/lsp"
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

	// Only verified, content-preserving output is applied; otherwise the
	// document is left untouched and the reason shown in the status line.
	formatted, err := formatter.FormatVerified(f.Text, params, s.getConfig())
	if err != nil {
		var se *formatter.SkipError
		if errors.As(err, &se) {
			_ = lsp.NewMessenger(conn).ShowWarning(ctx, se.Error())
			return nil, nil
		}
		return nil, err
	}
	if formatted == f.Text {
		return nil, nil
	}
	// a single whole-document edit: Helix diffs it, so the cursor stays put
	return []lsp.TextEdit{{Range: formatter.FullRange(f.Text), NewText: formatted}}, nil
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
