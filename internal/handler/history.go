package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// controlHistory: the versions of the statement at a cursor and the
// workspace's recent runs. s.mu must be held (live buffers).
func (s *Server) controlHistory(p control.HistoryParams) (*control.HistoryResult, error) {
	res := &control.HistoryResult{Versions: []history.Entry{}, Recent: []history.Entry{}}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if p.Lineage != "" {
		entries, err := history.Find(s.workspace, history.Query{Lineage: p.Lineage})
		if err != nil {
			return nil, err
		}
		for _, v := range history.Versions(entries) {
			res.Versions = append(res.Versions, v...)
		}
		return res, nil
	}
	if p.File != "" && p.Line > 0 {
		if st, _, err := s.statementAt(p.File, p.Line, p.Col); err == nil {
			res.Statement = strings.TrimSpace(st.SQL)
			abs, _ := filepath.Abs(p.File)
			entries, err := history.Find(s.workspace, history.Query{File: abs, Like: res.Statement})
			if err != nil {
				return nil, err
			}
			for _, v := range history.Versions(entries) {
				res.Versions = append(res.Versions, v...)
			}
		}
	}
	recent, err := history.Find(s.workspace, history.Query{Limit: limit})
	if err != nil {
		return nil, err
	}
	res.Recent = append(res.Recent, recent...)
	return res, nil
}

// statementAt finds the statement at a cursor (1-based) in a file's live
// buffer.
func (s *Server) statementAt(file string, line, col int) (sqlsplit.Statement, string, error) {
	d := sqlsplit.PostgreSQL
	if s.curDBCfg != nil {
		d = sqlsplit.DialectOf(string(s.curDBCfg.Driver))
	}
	_, text, err := s.documentText(file)
	if err != nil {
		return sqlsplit.Statement{}, "", err
	}
	st, ok := sqlsplit.AtCursor(text, sqlsplit.Split(text, d), sqlsplit.Position{Line: line - 1, Char: max(col-1, 0)})
	if !ok {
		return sqlsplit.Statement{}, text, fmt.Errorf("no statement at %s:%d", filepath.Base(file), line)
	}
	return st, text, nil
}

// replaceStatement replaces the statement at a cursor in the editor's
// buffer with text (the terminator is kept). s.mu must be held.
func (s *Server) replaceStatement(p control.ReplaceParams) (string, error) {
	uri, ok := s.openURI(p.File)
	if !ok || s.editor == nil {
		return "", errors.New("the file is not open in the editor")
	}
	st, text, err := s.statementAt(p.File, p.Line, p.Col)
	if err != nil {
		return "", err
	}
	start := offsetOf(text, st.Start)
	end := start + len(st.SQL)
	if start < 0 || end > len(text) || text[start:end] != st.SQL {
		// the statement's text isn't verbatim at Start: replace through the
		// terminator and put one back
		end = offsetOf(text, st.End)
		if st.Terminated && !strings.HasSuffix(strings.TrimSpace(p.Text), ";") {
			p.Text += ";"
		}
	}
	if start < 0 || end < start || end > len(text) {
		return "", errors.New("cannot locate the statement")
	}
	edit := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{uri: {{
		Range:   lsp.Range{Start: positionOf(text, start), End: positionOf(text, end)},
		NewText: strings.TrimSpace(p.Text),
	}}}}
	conn := s.editor
	// a request to the client: not from the handler's goroutine (deadlock);
	// Helix applies it once the `:insert-output` that asked returns
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var res lsp.ApplyWorkspaceEditResult
		params := lsp.ApplyWorkspaceEditParams{Label: "sqlh: statement from history", Edit: edit}
		if err := conn.Call(ctx, "workspace/applyEdit", params, &res); err != nil {
			log.Println("replace statement:", err)
		} else if !res.Applied {
			log.Println("replace statement not applied:", res.FailureReason)
		}
	}()
	return fmt.Sprintf("replaced the statement at line %d", st.Start.Line+1), nil
}

// offsetOf converts a 0-based line/character (UTF-16 units, as sqlsplit
// counts) position to a byte offset; -1 if outside the text.
func offsetOf(text string, p sqlsplit.Position) int {
	off := 0
	for l := 0; l < p.Line; l++ {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			return -1
		}
		off += i + 1
	}
	for c := 0; c < p.Char && off < len(text) && text[off] != '\n'; {
		r, size := utf8.DecodeRuneInString(text[off:])
		off += size
		c++
		if r > 0xFFFF {
			c++
		}
	}
	return off
}

// positionOf converts a byte offset to an LSP position (UTF-16 units).
func positionOf(text string, off int) lsp.Position {
	before := text[:off]
	line := strings.Count(before, "\n")
	lineStart := strings.LastIndexByte(before, '\n') + 1
	char := 0
	for _, r := range before[lineStart:] {
		if r >= 0x10000 {
			char += 2
		} else {
			char++
		}
	}
	return lsp.Position{Line: line, Character: char}
}
