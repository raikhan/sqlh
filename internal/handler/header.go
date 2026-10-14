package handler

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/raikhan/sqlh/internal/header"
	"github.com/raikhan/sqlh/internal/lsp"
)

// stampHeader points the "-- sqlh: conn=… db=…" header of an open document
// at the current connection and database, inserting it if missing, so that
// `sqlh dump` runs the file where it last ran in the editor. The client
// applies the edit (the buffer becomes modified; the CLI reads it once
// saved); files that aren't open are never touched. Call with s.mu held.
func (s *Server) stampHeader(uri string) {
	f, ok := s.files[uri]
	if !ok || s.editor == nil || s.curDBCfg == nil {
		return
	}
	if s.curDBCfg.Alias == "" {
		log.Println("connection header: the connection has no alias to name it by")
		return
	}
	e, changed := header.Stamp(f.Text, s.curDBCfg.Alias, s.curDBName)
	if !changed {
		return
	}
	edit := lsp.WorkspaceEdit{DocumentChanges: []lsp.TextDocumentEdit{{
		TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
			Version:                int32(f.Version),
			TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: uri},
		},
		Edits: []lsp.TextEdit{{
			Range: lsp.Range{
				Start: lsp.Position{Line: e.Line},
				End:   lsp.Position{Line: e.Line, Character: e.OldLen},
			},
			NewText: e.Text,
		}},
	}}}
	conn := s.editor
	// a request to the client: not from the handler's goroutine (deadlock)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var res lsp.ApplyWorkspaceEditResult
		params := lsp.ApplyWorkspaceEditParams{Label: "sqlh: connection header", Edit: edit}
		if err := conn.Call(ctx, "workspace/applyEdit", params, &res); err != nil {
			log.Println("connection header:", err)
		} else if !res.Applied {
			log.Println("connection header not applied:", res.FailureReason)
		}
	}()
}

// openURI returns the URI of path if the editor has it open.
func (s *Server) openURI(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	for u := range s.files {
		if uriToPath(u) == abs {
			return u, true
		}
	}
	return "", false
}
