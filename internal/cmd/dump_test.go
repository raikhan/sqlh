package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
)

// dumpEnv: a SQLite database, a config with connections "lite" and "lite2"
// (another file), no server (own runtime dir), cwd in a temp workspace.
type dumpEnv struct {
	t      *testing.T
	ws     string
	config string
}

func newDumpEnv(t *testing.T) *dumpEnv {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SQLH_RUNTIME_DIR", filepath.Join(base, "rt"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, db := range []string{"a.db", "b.db"} {
		conn, err := sql.Open("sqlite3", "file:"+filepath.Join(base, db))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec("create table t (id integer, name text); insert into t values (1, '" + db + "'), (2, 'x')"); err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	cfg := filepath.Join(base, "config.yml")
	yml := "connections:\n" +
		"  - alias: lite\n    driver: sqlite3\n    dataSourceName: file:" + filepath.Join(base, "a.db") + "\n" +
		"  - alias: lite2\n    driver: sqlite3\n    dataSourceName: file:" + filepath.Join(base, "b.db") + "\n"
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	return &dumpEnv{t: t, ws: ws, config: cfg}
}

func (e *dumpEnv) write(name, text string) string {
	e.t.Helper()
	p := filepath.Join(e.ws, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *dumpEnv) read(name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.ws, name))
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

// run runs `sqlh --config … dump ARGS…` in process; stdout is discarded.
func (e *dumpEnv) run(args ...string) error {
	e.t.Helper()
	app := &cli.App{
		Name:     "sqlh",
		Flags:    []cli.Flag{&cli.StringFlag{Name: "config", Aliases: []string{"c"}}},
		Commands: Commands(),
		Writer:   os.Stderr,
	}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	defer func() { os.Stdout = stdout; devnull.Close() }()
	argv := append([]string{"sqlh", "--config", e.config, "dump"}, args...)
	return app.Run(ReorderArgs(app, argv))
}

func TestDumpHeaderConnection(t *testing.T) {
	e := newDumpEnv(t)
	e.write("q/report.sql", "-- monthly\n-- sqlh: conn=lite2\n\nselect name from t order by id;\n")
	if err := e.run("q/report.sql"); err != nil {
		t.Fatal(err)
	}
	// cwd, <stem>.csv, the header's connection (b.db)
	if got := e.read("report.csv"); got != "name\nb.db\nx\n" {
		t.Errorf("report.csv = %q", got)
	}
	// never the results directory / latest.csv (only the history)
	if _, err := os.Stat(filepath.Join(e.ws, ".sqlh", "results")); !os.IsNotExist(err) {
		t.Error(".sqlh/results created by dump")
	}
	if b, err := os.ReadFile(filepath.Join(e.ws, ".sqlh", "history.jsonl")); err != nil || !strings.Contains(string(b), `"kind":"dump"`) {
		t.Errorf("history: %s %v", b, err)
	}

	// overwritten; -d DIR
	e.write("q/report.sql", "-- sqlh: conn=lite\nselect id from t where id = 2;\n")
	if err := e.run("-d", "out", "q/report.sql"); err != nil {
		t.Fatal(err)
	}
	if got := e.read("out/report.csv"); got != "id\n2\n" {
		t.Errorf("out/report.csv = %q", got)
	}
	if err := e.run("q/report.sql", "-d", "out"); err != nil { // flags after the file
		t.Fatal(err)
	}
}

func TestDumpNumbersResultSetsAndLine(t *testing.T) {
	e := newDumpEnv(t)
	e.write("two.sql", "-- sqlh: conn=lite\nselect 1 as a;\nupdate t set name = 'y' where id = 2;\nselect name from t where id = 2;\n")
	if err := e.run("two.sql"); err != nil {
		t.Fatal(err)
	}
	if got := e.read("two.csv"); got != "a\n1\n" {
		t.Errorf("two.csv = %q", got)
	}
	if got := e.read("two_2.csv"); got != "name\ny\n" {
		t.Errorf("two_2.csv = %q", got)
	}
	// --line: only that statement; -o: exact path
	if err := e.run("two.sql", "--line", "4", "-o", "x/one.csv"); err != nil {
		t.Fatal(err)
	}
	if got := e.read("x/one.csv"); got != "name\ny\n" {
		t.Errorf("one.csv = %q", got)
	}
}

func TestDumpConnectionRequired(t *testing.T) {
	e := newDumpEnv(t)
	e.write("plain.sql", "select 1;\n")
	err := e.run("plain.sql")
	if err == nil || !strings.Contains(err.Error(), "no connection") {
		t.Fatalf("want no-connection error, got %v", err)
	}
	// the workspace's remembered/first connection is NOT a fallback; --conn is
	if err := e.run("--conn", "lite2", "plain.sql"); err != nil {
		t.Fatal(err)
	}
	if got := e.read("plain.csv"); got != "1\n1\n" {
		t.Errorf("plain.csv = %q", got)
	}
	// --conn overrides a header (and its db=)
	e.write("h.sql", "-- sqlh: conn=lite db=ignored\nselect name from t where id = 1;\n")
	if err := e.run("--conn", "lite2", "h.sql"); err != nil {
		t.Fatal(err)
	}
	if got := e.read("h.csv"); got != "name\nb.db\n" {
		t.Errorf("h.csv = %q", got)
	}
	e.write("bad.sql", "-- sqlh: conn=nope\nselect 1;\n")
	if err := e.run("bad.sql"); err == nil || !strings.Contains(err.Error(), `unknown connection "nope" (header); configured: lite, lite2`) {
		t.Errorf("unknown alias: %v", err)
	}
}

func TestDumpSeveralFiles(t *testing.T) {
	e := newDumpEnv(t)
	e.write("a.sql", "-- sqlh: conn=lite\nselect 'a' as f;\n")
	e.write("b.sql", "select 'b' as f;\n") // no header: fails
	e.write("c.sql", "-- sqlh: conn=lite2\nselect 'c' as f;\n")
	err := e.run("-d", "out", "a.sql", "b.sql", "c.sql")
	if err == nil || err.Error() != "1 of 3 files failed" {
		t.Fatalf("got %v", err)
	}
	for f, want := range map[string]string{"out/a.csv": "f\na\n", "out/c.csv": "f\nc\n"} {
		if got := e.read(f); got != want {
			t.Errorf("%s = %q", f, got)
		}
	}
	for _, args := range [][]string{
		{"-o", "x.csv", "a.sql", "c.sql"},
		{"--line", "2", "a.sql", "c.sql"},
		{"-o", "x.csv", "-d", "out", "a.sql"},
		{},
	} {
		if err := e.run(args...); err == nil {
			t.Errorf("%v: want a usage error", args)
		}
	}
}

func TestDumpConnectionSchema(t *testing.T) {
	cfg := &config.Config{Connections: []*database.DBConfig{
		{Alias: "ora", Driver: "oracle", Schemas: []string{"CADE", "CADE500"}},
		{Alias: "lite", Driver: "sqlite3"},
	}}
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f := write("a.sql", "-- sqlh: conn=ora schema=cade500\nselect 1 from dual;\n")
	if c, err := dumpConnection(cfg, f, "", ""); err != nil || c.Schema != "CADE500" || c.CurrentSchema() != "CADE500" {
		t.Errorf("header schema: %+v %v", c, err)
	}
	if c, err := dumpConnection(cfg, f, "", "cade"); err != nil || c.CurrentSchema() != "CADE" {
		t.Errorf("--schema: %+v %v", c, err)
	}
	if cfg.Connections[0].Schema != "" {
		t.Error("the loaded configuration was mutated")
	}
	if _, err := dumpConnection(cfg, f, "", "other"); err == nil || !strings.Contains(err.Error(), "not in connection ora's schemas") {
		t.Errorf("schema outside the list: %v", err)
	}
	// another connection than the header's: the header's schema doesn't apply
	if c, err := dumpConnection(cfg, f, "lite", ""); err != nil || c.Schema != "" {
		t.Errorf("--conn: %+v %v", c, err)
	}
	if _, err := dumpConnection(cfg, write("b.sql", "-- sqlh: conn=lite schema=main\nselect 1;\n"), "", ""); err == nil {
		t.Error("schema on SQLite accepted")
	}
}
