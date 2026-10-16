package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/connedit"
	"github.com/raikhan/sqlh/internal/herdr"
	"github.com/raikhan/sqlh/internal/herdr/herdrtest"
)

func TestNextQueryNumber(t *testing.T) {
	dir := t.TempDir()
	if n := nextQueryNumber(dir); n != 1 {
		t.Errorf("empty: %d", n)
	}
	if n := nextQueryNumber(filepath.Join(dir, "missing")); n != 1 {
		t.Errorf("missing: %d", n)
	}
	for _, f := range []string{"query_3.sql", "query_x.sql", "report.sql", "query_10.sql.bak", "Query_20.sql"} {
		_ = os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	if n := nextQueryNumber(dir); n != 4 {
		t.Errorf("after query_3: %d", n)
	}
}

func TestHelixQuote(t *testing.T) {
	if got := helixQuote("/w/it's.sql"); got != "'/w/it''s.sql'" {
		t.Errorf("got %s", got)
	}
}

func TestSplitEditorEnv(t *testing.T) {
	hd, fake := herdrtest.Start(t, func(method string, _ map[string]any) (any, *herdr.Error) {
		if method == "pane.split" {
			return map[string]any{"pane": map[string]any{"pane_id": "w1:p9"}}, nil
		}
		return nil, nil
	})
	pane, err := splitEditor(hd, "w1:p1", "right", "/w", []string{"hx", "/w/q.sql"}, "w1:p1")
	if err != nil || pane != "w1:p9" {
		t.Fatalf("pane %q err %v", pane, err)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 || reqs[0].Method != "pane.split" || reqs[1].Method != "pane.swap" {
		t.Fatalf("requests %+v", reqs)
	}
	env, _ := reqs[0].Params["env"].(map[string]any)
	if env["HELIX_ORIGIN_PANE_ID"] != "w1:p1" || env["HELIX_HELPER_ARGV"] != `["hx","/w/q.sql"]` {
		t.Errorf("env %v", env)
	}
}

func TestEditConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	orig := "# mine\nconnections:\n  - alias: a\n    driver: sqlite3\n    dataSourceName: file:" + filepath.Join(dir, "a.db") + "\n\nresults:\n  previewLimit: 5\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	fields := connedit.Fields{"alias": "b", "driver": "sqlite3", "dataSourceName": "file:" + filepath.Join(dir, "b.db"), "group": "G"}
	if err := editConfigFile(path, func(f *connedit.File) error { return f.Set("", fields, "a") }); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(got), "# mine\n") || !strings.Contains(string(got), "  - alias: b\n    group: G\n    driver: sqlite3\n") {
		t.Errorf("after adding:\n%s", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	// an invalid result is not written
	bad := connedit.Fields{"alias": "c", "driver": "nosuch"}
	if err := editConfigFile(path, func(f *connedit.File) error { return f.Set("", bad, "") }); err == nil {
		t.Error("an invalid driver was written")
	}
	if after, _ := os.ReadFile(path); string(after) != string(got) {
		t.Error("the file changed on a refused edit")
	}
	if err := editConfigFile(path, func(f *connedit.File) error { return f.Delete("a") }); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if strings.Contains(string(got), "alias: a\n") || !strings.Contains(string(got), "results:\n  previewLimit: 5\n") {
		t.Errorf("after deleting:\n%s", got)
	}
	// a test connection
	if err := testConnection(fields); err != nil {
		t.Errorf("test sqlite: %v", err)
	}
	if err := testConnection(connedit.Fields{"alias": "x", "driver": "sqlite3", "dataSourceName": "file:/nonexistent/dir/x.db?mode=ro"}); err == nil {
		t.Error("a missing database connected")
	}
}

func TestRenameDeleteQuery(t *testing.T) {
	dir := t.TempDir()
	b := &panelBackend{ws: dir}
	q := filepath.Join(dir, "queries")
	_ = os.MkdirAll(q, 0o755)
	a, other := filepath.Join(q, "query_1.sql"), filepath.Join(q, "query_2.sql")
	_ = os.WriteFile(a, []byte("select 1"), 0o644)
	_ = os.WriteFile(other, []byte("select 2"), 0o644)
	dst, err := b.RenameQuery(a, "reports/orders")
	if err != nil || dst != filepath.Join(q, "reports", "orders.sql") {
		t.Fatalf("rename: %s %v", dst, err)
	}
	if text, _ := os.ReadFile(dst); string(text) != "select 1" {
		t.Errorf("moved text %q", text)
	}
	if _, err := b.RenameQuery(other, "reports/orders.sql"); err == nil {
		t.Error("renamed over an existing file")
	}
	if err := b.DeleteQuery(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("not deleted")
	}
}
