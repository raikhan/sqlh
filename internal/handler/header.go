package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/raikhan/sqlh/internal/header"
	"github.com/raikhan/sqlh/internal/lsp"
)

const (
	// CommandUseHeader switches the editor to the connection (and database)
	// in a document's header. Argument: the document URI.
	CommandUseHeader = "sqlh.useHeader"
	// CommandStampHeader points a document's header at the editor's
	// connection without running anything. Argument: the document URI.
	CommandStampHeader = "sqlh.stampHeader"
)

// headerTarget is where a document's header points, or where the editor is:
// a connection alias, a database and a schema override ("" = the
// connection's own).
type headerTarget struct{ conn, db, schema string }

func (t headerTarget) String() string {
	var sw []string
	if t.db != "" {
		sw = append(sw, "database "+t.db)
	}
	if t.schema != "" {
		sw = append(sw, "schema "+t.schema)
	}
	if len(sw) > 0 {
		return t.conn + " (" + strings.Join(sw, ", ") + ")"
	}
	return t.conn
}

// headerState is what header diagnostics are checked against, snapshot
// under s.mu (they are published from a timer).
type headerState struct {
	editor  headerTarget // conn "" when not connected or unnamed
	aliases []string     // configured connection aliases
	// schemas: a connection's default schema, which a header needn't name
	schemas map[string]string
}

func (s *Server) headerState() headerState {
	st := headerState{schemas: map[string]string{}}
	if cfg := s.getConfig(); cfg != nil {
		for _, c := range cfg.Connections {
			if c.Alias != "" {
				st.aliases = append(st.aliases, c.Alias)
				st.schemas[c.Alias] = c.CurrentSchema()
			}
		}
	}
	if s.dbConn != nil && s.curDBCfg != nil {
		st.editor = headerTarget{s.curDBCfg.Alias, s.curDBName, s.curSchema}
		if s.defaultSchema != "" {
			st.schemas[s.curDBCfg.Alias] = s.defaultSchema
		}
	}
	return st
}

// target is where a header points; a schema= naming the connection's
// default schema counts as none.
func (st headerState) target(h header.Header) headerTarget {
	t := headerTarget{h.Conn, h.DB, h.Schema}
	if d := st.schemas[h.Conn]; t.schema != "" && strings.EqualFold(t.schema, d) {
		t.schema = ""
	}
	return t
}

// headerDiagnostics flags a header naming a connection that isn't
// configured (warning) or another target than the editor's (information:
// running the file from the editor would move the header; `sqlh dump` from
// the terminal still uses it).
func headerDiagnostics(text string, st headerState) []lsp.Diagnostic {
	h := header.Parse(text)
	if h.Line < 0 || h.Conn == "" {
		return nil
	}
	start, end := h.Span()
	d := lsp.Diagnostic{
		Range: lsp.Range{
			Start: lsp.Position{Line: h.Line, Character: start},
			End:   lsp.Position{Line: h.Line, Character: end},
		},
		Source: &diagnosticSource,
	}
	file := st.target(h)
	switch {
	case !contains(st.aliases, h.Conn):
		d.Severity = lsp.SeverityWarning
		d.Message = fmt.Sprintf("unknown connection %q; configured: %s", h.Conn, strings.Join(st.aliases, ", "))
	case st.editor.conn != "" && file != st.editor:
		d.Severity = lsp.SeverityInformation
		d.Message = fmt.Sprintf("this file targets %s, the editor is on %s; running it here moves the header (code action: switch the editor)", file, st.editor)
	default:
		return nil
	}
	return []lsp.Diagnostic{d}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// headerActions: the code actions for a document whose header differs from
// the editor's connection; first when the cursor is on the header.
func (s *Server) headerActions(uri string, rng lsp.Range) (actions []lsp.Command, onHeader bool) {
	f, ok := s.files[uri]
	if !ok {
		return nil, false
	}
	h := header.Parse(f.Text)
	if h.Line < 0 || h.Conn == "" {
		return nil, false
	}
	st := s.headerState()
	file := st.target(h)
	if st.editor.conn == "" || file == st.editor {
		return nil, false
	}
	if contains(st.aliases, h.Conn) {
		actions = append(actions, lsp.Command{
			Title: "Switch the editor to " + file.String() + " (the file's header)", Command: CommandUseHeader,
			Arguments: []interface{}{uri},
		})
	}
	actions = append(actions, lsp.Command{
		Title: "Point the file's header at " + st.editor.String(), Command: CommandStampHeader,
		Arguments: []interface{}{uri},
	})
	return actions, rng.Start.Line <= h.Line && h.Line <= rng.End.Line
}

// useHeader switches the editor to the connection, database and schema of a
// document's header.
func (s *Server) useHeader(ctx context.Context, uri string) (string, error) {
	f, ok := s.files[uri]
	if !ok {
		return "", fmt.Errorf("document not open: %s", uri)
	}
	h := header.Parse(f.Text)
	if h.Line < 0 || h.Conn == "" {
		return "", errors.New("the file has no connection header")
	}
	var msgs []string
	if s.dbConn == nil || s.curDBCfg == nil || s.curDBCfg.Alias != h.Conn {
		m, err := s.switchTo(ctx, h.Conn) // also back to its own database
		if err != nil {
			return "", err
		}
		msgs = append(msgs, m)
	}
	if h.DB != s.curDBName {
		var m string
		var err error
		if h.DB == "" {
			m, err = s.switchTo(ctx, h.Conn) // reconnect to the connection's own database
		} else {
			m, err = s.useDatabase(ctx, h.DB)
		}
		if err != nil {
			return "", err
		}
		msgs = append(msgs, m)
	}
	if want := s.headerState().target(h); want.schema != s.curSchema {
		target := want.schema
		if target == "" {
			target = s.defaultSchema // back to the connection's own
		}
		m, err := s.useSchema(ctx, target)
		if err != nil {
			return "", err
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return "already on " + s.headerState().target(h).String(), nil
	}
	return strings.Join(msgs, "; "), nil
}

// relintOpen re-checks every open document; s.mu must be held (relintAll
// takes it).
func (s *Server) relintOpen() {
	for uri := range s.files {
		s.scheduleLint(uri)
	}
}

// stampHeader points the "-- sqlh: conn=… db=… schema=…" header of an open
// document at the current connection, database and schema, inserting it if missing, so that
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
	e, changed := header.Stamp(f.Text, s.curDBCfg.Alias, s.curDBName, s.curSchema)
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
		if samePath(uriToPath(u), abs) {
			return u, true
		}
	}
	return "", false
}
