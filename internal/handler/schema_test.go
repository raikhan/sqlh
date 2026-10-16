package handler

import (
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
