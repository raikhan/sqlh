package handler

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

// newSchemaServer: a server on PostgreSQL (SQLH_TEST_PG) whose connection
// lists two schemas of that database.
func newSchemaServer(t *testing.T, text string) (*dumpServer, string, string) {
	t.Helper()
	dsn := os.Getenv("SQLH_TEST_PG")
	if dsn == "" {
		t.Skip("SQLH_TEST_PG not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var a, b string
	if err := db.QueryRow(`SELECT min(nspname), max(nspname) FROM pg_namespace
		WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'`).Scan(&a, &b); err != nil || a == b {
		t.Skipf("needs two user schemas: %v", err)
	}

	rt, err := os.MkdirTemp("/tmp", "sqlh-rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(rt) })
	t.Setenv("SQLH_RUNTIME_DIR", rt)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []*database.DBConfig{
		{Alias: "pg", Driver: "postgresql", DataSourceName: dsn, Schemas: []string{a, b}},
	}}
	return startTestServer(t, cfg, ws, text), a, b
}

func (d *dumpServer) currentSchemaInSession() string {
	d.t.Helper()
	res := d.run(control.RunParams{SQL: "select '<' || current_schema() || '>' as s", Table: true})
	_, rest, _ := strings.Cut(res.Table, "<")
	got, _, _ := strings.Cut(rest, ">")
	return got
}

func TestSwitchSchema(t *testing.T) {
	d, a, b := newSchemaServer(t, "select 1;\n")

	var st control.Status
	if err := d.call(control.MethodStatus, nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Schema != a {
		t.Fatalf("starts on %q, want the first of the list %q", st.Schema, a)
	}
	if got := d.currentSchemaInSession(); got != a {
		t.Fatalf("session on %q", got)
	}
	var choices []control.SchemaItem
	if err := d.call(control.MethodSchemaChoices, nil, &choices); err != nil {
		t.Fatal(err)
	}
	if len(choices) != 2 || choices[0].Name != a || !choices[0].Default || choices[1].Default {
		t.Fatalf("choices %+v", choices)
	}

	// switch from the buffer: session, cache and header follow
	edits := d.edits()
	var msg string
	if err := d.call(control.MethodUseSchema, control.DatabaseParams{Name: b, File: d.file}, &msg); err != nil {
		t.Fatal(err)
	}
	if got := d.waitEdit(edits); !strings.HasPrefix(got, "-- sqlh: conn=pg schema="+b+"\n") {
		t.Fatalf("header after the switch: %q", got)
	}
	if got := d.currentSchemaInSession(); got != b {
		t.Errorf("session on %q after switching to %q", got, b)
	}
	if err := d.call(control.MethodStatus, nil, &st); err != nil || st.Schema != b {
		t.Errorf("status schema %q %v", st.Schema, err)
	}

	// not in the list: refused, nothing changes
	if err := d.call(control.MethodUseSchema, control.DatabaseParams{Name: "nope"}, &msg); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Errorf("unknown schema: %v", err)
	}
	if got := d.currentSchemaInSession(); got != b {
		t.Errorf("session moved to %q", got)
	}

	// another connection's view: the editor back on the default, the header
	// still on b → hint; using the header switches back to b
	if err := d.call(control.MethodUseSchema, control.DatabaseParams{Name: a}, &msg); err != nil {
		t.Fatal(err)
	}
	ds := d.waitDiags("schema mismatch", func(ds []lsp.Diagnostic) bool { return headerHint(ds) != nil })
	if h := headerHint(ds); !strings.Contains(h.Message, "pg (schema "+b+")") {
		t.Errorf("hint %q", h.Message)
	}
	d.execute(lsp.Command{Command: CommandUseHeader, Arguments: []interface{}{d.uri}})
	d.waitDiags("after useHeader", func(ds []lsp.Diagnostic) bool { return headerHint(ds) == nil })
	if got := d.currentSchemaInSession(); got != b {
		t.Errorf("useHeader: session on %q", got)
	}

	// back to the default from the buffer: schema= dropped from the header
	edits = d.edits()
	if err := d.call(control.MethodUseSchema, control.DatabaseParams{Name: a, File: d.file}, &msg); err != nil {
		t.Fatal(err)
	}
	if got := d.waitEdit(edits); !strings.HasPrefix(got, "-- sqlh: conn=pg\n") {
		t.Fatalf("header back on the default: %q", got)
	}
}

func TestTableDefinitionFromDatabase(t *testing.T) {
	d, a, _ := newSchemaServer(t, "select 1;\n")
	var table string
	db, err := sql.Open("pgx", os.Getenv("SQLH_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT min(table_name) FROM information_schema.tables WHERE table_schema = $1 AND table_type = 'BASE TABLE'`, a).Scan(&table); err != nil {
		t.Skip("no table in", a)
	}
	text := "select * from " + a + "." + table + ";\n"
	d.editor.mu.Lock()
	d.editor.text[d.uri] = text
	d.editor.version[d.uri]++
	v := d.editor.version[d.uri]
	d.editor.mu.Unlock()
	_ = d.editor.conn.Notify(context.Background(), "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: d.uri, Version: v},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: text}},
	})
	d.waitSynced()

	var locs []lsp.Location
	params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: d.uri},
		Position:     lsp.Position{Line: 0, Character: len("select * from "+a+".") + 1},
	}}
	if err := d.editor.conn.Call(context.Background(), "textDocument/definition", params, &locs); err != nil {
		t.Fatal(err)
	}
	if len(locs) != 1 {
		t.Fatalf("locations %+v", locs)
	}
	body := readFile(t, strings.TrimPrefix(locs[0].URI, "file://"))
	if !strings.Contains(body, "(table) in pg: a read-only copy of its definition (pg_catalog)") || !strings.Contains(body, "CREATE TABLE ") {
		t.Errorf("file:\n%s", body)
	}

	// sqlh ddl through the server: the same
	var res control.DDLResult
	if err := d.call(control.MethodDDL, control.DDLParams{Table: a + "." + table}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Kind != "table" || res.Source != "pg_catalog" || !strings.HasPrefix(res.DDL, "CREATE TABLE ") {
		t.Errorf("ddl %+v", res)
	}
	if err := d.call(control.MethodDDL, control.DDLParams{Table: "no_such_table_x"}, &res); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("unknown table: %v", err)
	}
}
