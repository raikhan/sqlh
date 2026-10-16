package handler

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

func TestEditPosition(t *testing.T) {
	cases := []struct {
		old, new   string
		line, char int
		ok         bool
	}{
		{"select ", "select x", 0, 8, true},
		{"select *\nfrom ", "select *\nfrom o", 1, 6, true},
		{"select *\nfrom orders", "select *\nfrom orders o", 1, 13, true},
		{"abc", "abc", 0, 0, false},
		{"select a\nfrom t", "select ab\nfrom t", 0, 9, true}, // typed b
		{"select ab\nfrom t", "select a\nfrom t", 0, 8, true}, // deleted b
		{"é", "éa", 0, 2, true},
		{"aé", "aéé", 0, 2, true}, // a repeated character: before it (ambiguous)
	}
	for _, c := range cases {
		l, ch, ok := editPosition(c.old, c.new)
		if l != c.line || ch != c.char || ok != c.ok {
			t.Errorf("%q → %q: %d:%d %v, want %d:%d %v", c.old, c.new, l, ch, ok, c.line, c.char, c.ok)
		}
	}
}

func TestPanelServer(t *testing.T) {
	d := newDumpServer(t, "select * from t\n")
	sock, _, err := control.Find(d.ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events := make(chan control.Event, 16)
	cl, err := control.DialEvents(ctx, sock, func(ev control.Event) { events <- ev })
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var sub control.SubscribeResult
	if err := cl.Call(ctx, control.MethodSubscribe, control.SubscribeParams{Pane: "w1:p2", EditorPane: "w1:p1"}, &sub); err != nil {
		t.Fatal(err)
	}
	wait := func(kind string) control.Event {
		t.Helper()
		for {
			select {
			case ev := <-events:
				if ev.Kind == kind {
					return ev
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s event", kind)
			}
		}
	}

	// the catalog
	var cat control.CatalogResult
	if err := d.call(control.MethodCatalog, nil, &cat); err != nil {
		t.Fatal(err)
	}
	if cat.Connection != "lite" || cat.Environment != "dev" || len(cat.Schemas) == 0 {
		t.Fatalf("catalog %+v", cat)
	}
	found := false
	for _, s := range cat.Schemas {
		for _, o := range s.Objects {
			found = found || o.Name == "t"
		}
	}
	if !found {
		t.Errorf("no table t in %+v", cat.Schemas)
	}

	// typing moves the cursor: an event with the edit's position
	if err := d.editor.conn.Notify(ctx, "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: d.uri, Version: 2},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "select * from t\nwhere \n"}},
	}); err != nil {
		t.Fatal(err)
	}
	ev := wait(control.EventCursor)
	if ev.Cursor == nil || ev.Cursor.File != d.file || ev.Cursor.Line != 2 || ev.Cursor.Col != 7 {
		t.Errorf("cursor event %+v", ev.Cursor)
	}

	// panels: registered, focus goes to this subscriber
	var panels []control.PanelInfo
	if err := d.call(control.MethodPanels, nil, &panels); err != nil || len(panels) != 1 || panels[0].Pane != "w1:p2" {
		t.Fatalf("panels %+v %v", panels, err)
	}
	var fr control.PanelFocusResult
	if err := d.call(control.MethodPanelFocus, control.PanelFocusParams{EditorPane: "w1:p1",
		Cursor: &control.Cursor{File: d.file, Line: 1, Col: 3}}, &fr); err != nil || !fr.Found || fr.Pane != "w1:p2" {
		t.Fatalf("focus %+v %v", fr, err)
	}
	if ev := wait(control.EventFocus); ev.Cursor == nil || ev.Cursor.Line != 1 {
		t.Errorf("focus event %+v", ev)
	}
	if err := d.call(control.MethodPanelFocus, control.PanelFocusParams{EditorPane: "w9:p9"}, &fr); err != nil || fr.Found {
		t.Errorf("focus of another editor: %+v %v", fr, err)
	}

	// pending actions: per editor pane, handed out once
	var ok string
	if err := d.call(control.MethodPanelAction, control.PanelActionParams{EditorPane: "w1:p1",
		Action: control.PanelAction{Kind: control.ActionTable, Table: "t"}}, &ok); err != nil {
		t.Fatal(err)
	}
	var take control.PanelTakeResult
	if err := d.call(control.MethodPanelTake, control.PanelTakeParams{EditorPane: "w9:p9"}, &take); err != nil || take.Found {
		t.Errorf("other pane took %+v %v", take, err)
	}
	if err := d.call(control.MethodPanelTake, control.PanelTakeParams{EditorPane: "w1:p1"}, &take); err != nil || !take.Found || take.Action.Table != "t" {
		t.Errorf("take %+v %v", take, err)
	}
	take = control.PanelTakeResult{}
	if err := d.call(control.MethodPanelTake, control.PanelTakeParams{EditorPane: "w1:p1"}, &take); err != nil || take.Found {
		t.Errorf("second take %+v %v", take, err)
	}

	// a connection switch: a connection event with a new generation
	gen := ev.Generation
	var st control.Status
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite2"}, &st); err != nil {
		t.Fatal(err)
	}
	if ev := wait(control.EventConnection); ev.Generation <= gen {
		t.Errorf("generation %d after %d", ev.Generation, gen)
	}

	// unsubscribed on disconnect
	cl.Close()
	for i := 0; i < 100; i++ {
		if err := d.call(control.MethodPanels, nil, &panels); err == nil && len(panels) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("panel still registered: %+v", panels)
}

func TestSample(t *testing.T) {
	d := newDumpServer(t, "select * from t\n")
	var res control.SampleResult
	if err := d.call(control.MethodSample, control.SampleParams{Table: "t"}, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || len(res.Columns) != 2 || res.Columns[1] != "name" || *res.Rows[0][1] != "a.db" || res.Truncated {
		t.Errorf("sample %+v", res)
	}
	res = control.SampleResult{}
	if err := d.call(control.MethodSample, control.SampleParams{Table: "t", Limit: 1}, &res); err != nil || len(res.Rows) != 1 || !res.Truncated {
		t.Errorf("limited %+v %v", res, err)
	}
	res = control.SampleResult{}
	if err := d.call(control.MethodSample, control.SampleParams{Table: "T", Column: "NAME"}, &res); err != nil || len(res.Rows) != 2 || *res.Rows[0][1] != "1" {
		t.Errorf("top values %+v %v", res, err)
	}
	for _, bad := range []control.SampleParams{{Table: "t; drop table t"}, {Table: "nope"}, {Table: "t", Column: "id; drop table t"}} {
		if err := d.call(control.MethodSample, bad, &res); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
	// sample: off
	d.cfg.Connections[0].Sample = "off"
	if err := d.call(control.MethodSample, control.SampleParams{Table: "t"}, &res); err == nil || !strings.Contains(err.Error(), "off") {
		t.Errorf("off: %v", err)
	}
}

func TestLimitSQL(t *testing.T) {
	for driver, want := range map[dialect.DatabaseDriver]string{
		dialect.DatabaseDriverOracle:     "SELECT * FROM t FETCH FIRST 5 ROWS ONLY",
		dialect.DatabaseDriverMssql:      "SELECT TOP 5 * FROM t",
		dialect.DatabaseDriverPostgreSQL: "SELECT * FROM t LIMIT 5",
	} {
		if got := limitSQL(driver, "*", "t", "", 5); got != want {
			t.Errorf("%s: %s", driver, got)
		}
	}
}

func TestHistoryAndReplace(t *testing.T) {
	const head = "-- sqlh: conn=lite\n"
	d := newDumpServer(t, head+"select id from t;\n\nselect name from t where id = 1;\n")
	d.run(control.RunParams{File: d.file, Line: 4})
	// edit the statement, run again: a second version
	before := d.edits()
	var msg string
	if err := d.call(control.MethodReplaceStatement, control.ReplaceParams{File: d.file, Line: 4, Col: 1,
		Text: "select name from t where id = 2"}, &msg); err != nil {
		t.Fatal(err)
	}
	if text := d.waitEdit(before); text != head+"select id from t;\n\nselect name from t where id = 2;\n" {
		t.Fatalf("replaced: %q", text)
	}
	d.run(control.RunParams{File: d.file, Line: 4})
	d.run(control.RunParams{File: d.file, Line: 2}) // another query

	var h control.HistoryResult
	if err := d.call(control.MethodHistory, control.HistoryParams{File: d.file, Line: 4, Col: 1}, &h); err != nil {
		t.Fatal(err)
	}
	if h.Statement != "select name from t where id = 2" || len(h.Versions) != 2 ||
		h.Versions[0].SQL != "select name from t where id = 2" || h.Versions[1].SQL != "select name from t where id = 1" {
		t.Fatalf("history %+v", h)
	}
	if len(h.Recent) != 3 || h.Recent[0].SQL != "select id from t" || h.Recent[0].Conn != "lite" || h.Recent[0].Result == "" {
		t.Errorf("recent %+v", h.Recent)
	}
	// back to the first version
	before = d.edits()
	if err := d.call(control.MethodReplaceStatement, control.ReplaceParams{File: d.file, Line: 4, Col: 5, Text: h.Versions[1].SQL}, &msg); err != nil {
		t.Fatal(err)
	}
	if text := d.waitEdit(before); text != head+"select id from t;\n\nselect name from t where id = 1;\n" {
		t.Errorf("restored: %q", text)
	}
}

func TestHeadlessGivesWay(t *testing.T) {
	rt, err := os.MkdirTemp("/tmp", "sqlh-rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(rt) })
	t.Setenv("SQLH_RUNTIME_DIR", rt)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	_ = os.MkdirAll(ws, 0o755)
	db := filepath.Join(base, "a.db")
	c, _ := sql.Open("sqlite3", "file:"+db)
	if _, err := c.Exec("create table t (id integer)"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	cfg := &config.Config{Connections: []*database.DBConfig{{Alias: "lite", Driver: "sqlite3", DataSourceName: "file:" + db}}}

	h, err := RunHeadless(cfg, "", ws)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	call := func(method string, result interface{}) error {
		sock, _, err := control.Find(ws)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cl, err := control.Dial(ctx, sock)
		if err != nil {
			return err
		}
		defer cl.Close()
		return cl.Call(ctx, method, nil, result)
	}
	var cat control.CatalogResult
	if err := call(control.MethodCatalog, &cat); err != nil || cat.Connection != "lite" {
		t.Fatalf("headless catalog %+v %v", cat, err)
	}
	if s := control.Servers(); len(s) != 1 || !s[0].Headless {
		t.Errorf("servers %+v", s)
	}
	// a second headless server doesn't take over
	if h2, err := RunHeadless(cfg, "", ws); err == nil {
		h2.Stop()
		t.Error("second headless server started")
	}
	// an editor's server does
	startTestServer(t, cfg, ws, "select 1\n")
	if !h.Released() {
		t.Error("headless server still serving")
	}
	if s := control.Servers(); len(s) != 1 || s[0].Headless {
		t.Errorf("servers after the editor's %+v", s)
	}
	if err := call(control.MethodCatalog, &cat); err != nil {
		t.Errorf("editor's server: %v", err)
	}
}
