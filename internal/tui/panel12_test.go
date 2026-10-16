package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/connedit"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

func labels(rows []*node) []string {
	var out []string
	for _, n := range rows {
		out = append(out, n.label)
	}
	return out
}

func TestPanelCollapseAll(t *testing.T) {
	be := &fakeBackend{ctx: &control.ContextResult{Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}}
	cur := &control.Cursor{File: "/w/q.sql", Line: 3, Col: 10}
	p := newTestPanel(t, be, cur)
	if len(p.rows()) < 3 {
		t.Fatalf("statement open: %v", labels(p.rows()))
	}
	key(p, "H")
	if got := strings.Join(labels(p.rows()), ","); got != "orders o" {
		t.Fatalf("collapsed: %s", got)
	}
	// a table joining later arrives collapsed too
	be.ctx = &control.ContextResult{Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}, {Name: "customer", Alias: "c", Ref: "c"}}}
	_, cmd := p.Update(eventMsg{control.Event{Kind: control.EventCursor, Cursor: cur}})
	drain(p, cmd)
	if got := strings.Join(labels(p.rows()), ","); got != "orders o,customer c" {
		t.Fatalf("new table: %s", got)
	}
	key(p, "L")
	view := plain(p.View())
	if !strings.Contains(view, "→ customer  o.customer_id") || strings.Count(view, "Relations  2") != 2 {
		t.Fatalf("expand all:\n%s", view)
	}
	// Objects: down to the groups; no expand all there
	key(p, "2")
	key(p, "H")
	for _, n := range p.rows() {
		if n.expanded || n.depth > 0 {
			t.Fatalf("objects not collapsed: %v", labels(p.rows()))
		}
	}
	key(p, "L")
	if !strings.Contains(p.msg, "not in this tab") {
		t.Errorf("L in Objects: %q", p.msg)
	}
	// z: the selected row to the top
	key(p, "l") // open Tables
	key(p, "j", "j", "z")
	p.View()
	if tb := p.tab(); tb.top != tb.sel || tb.sel != 2 {
		t.Errorf("z: top %d sel %d", tb.top, tb.sel)
	}

	// panel.statement: collapsed
	p = newPanelWith(t, &fakeBackend{ctx: be.ctx}, Options{Icons: "ascii", Cursor: cur, StatementCollapsed: true})
	if got := strings.Join(labels(p.rows()), ","); got != "orders o,customer c" {
		t.Errorf("starts collapsed: %s", got)
	}
}

func TestPanelPalette(t *testing.T) {
	be := &fakeBackend{conf: map[string]connedit.Fields{"pg_local": {"alias": "pg_local", "driver": "postgresql"}}}
	p := newTestPanel(t, be, nil)
	key(p, "1")
	selectRow(t, p, "pg_local")
	key(p, "?")
	if p.pal == nil {
		t.Fatal("no palette")
	}
	view := plain(p.View())
	for _, want := range []string{"Connections · keys", "── Connections ──", "New connection", "Duplicate the connection", "── Navigation ──"} {
		if !strings.Contains(view, want) {
			t.Errorf("palette lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Sample a row") || strings.Contains(view, "Insert at") {
		t.Errorf("palette has other tabs' actions:\n%s", view)
	}
	typeText(p, "dupl")
	if e := p.pal.entries[p.pal.rows[p.pal.cursor].entry]; e.action != "duplicateConnection" {
		t.Errorf("dupl → %s", e.action)
	}
	key(p, "enter")
	if p.pal != nil || p.form == nil || !strings.Contains(p.form.title, "Duplicate of pg_local") {
		t.Fatalf("enter didn't run it: %+v", p.form)
	}
	key(p, "esc")
	// @ filters on keys; esc clears, then closes
	key(p, "?")
	typeText(p, "@X")
	if len(p.pal.rows) != 1 || p.pal.entries[p.pal.rows[0].entry].action != "deleteConnection" {
		t.Errorf("@X: %+v", p.pal.rows)
	}
	key(p, "esc")
	if p.pal == nil || p.pal.query != "" {
		t.Fatal("esc should clear the filter first")
	}
	key(p, "esc")
	if p.pal != nil {
		t.Fatal("esc should close")
	}
	// another tab, other actions
	key(p, "2", "?")
	view = plain(p.View())
	if !strings.Contains(view, "Sample a row") || strings.Contains(view, "New connection") {
		t.Errorf("objects palette:\n%s", view)
	}
	key(p, "?")
	if p.pal != nil {
		t.Error("? should close it")
	}
}

func TestPanelKeysPerTab(t *testing.T) {
	be := &fakeBackend{conf: map[string]connedit.Fields{"pg_local": {"alias": "pg_local", "driver": "postgresql"}}}
	keys := testKeys(nil, map[string]map[string]config.KeyList{
		"connections": {"duplicateConnection": {"d"}},
		"objects":     {"sample": {"d"}},
	})
	p := newPanelWith(t, be, Options{Icons: "ascii", Keys: keys})
	key(p, "1")
	selectRow(t, p, "pg_local")
	key(p, "d")
	if p.form == nil || !strings.Contains(p.form.title, "Duplicate") {
		t.Fatalf("d in Connections: %+v", p.form)
	}
	key(p, "esc", "2")
	selectRow(t, p, "orders")
	key(p, "d")
	if strings.Join(be.sampled, ",") != "orders" {
		t.Errorf("d in Objects: %v", be.sampled)
	}

	for _, c := range []struct{ yaml, err string }{
		{"panel:\n  keys:\n    connections:\n      duplicateConnection: E\n", `key "E" is on both`},
		{"panel:\n  keys:\n    tables:\n      up: k\n", "unknown section"},
		{"panel:\n  keys:\n    connections:\n      sample: x\n", "unknown action sample"},
		{"panel:\n  keys:\n    objects:\n      sample: d\n    connections:\n      duplicateConnection: d\n", ""},
		{"panel:\n  statement: shut\n", "panel.statement"},
		{"panel:\n  syntax:\n    keyword: blue\n", "not a colour"},
		{"panel:\n  syntax:\n    kw: \"#ffffff\"\n", "unknown class"},
	} {
		_, err := config.Parse([]byte(c.yaml + "connections:\n  - alias: a\n    driver: sqlite3\n    dataSourceName: file:a.db\n"))
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%q: %v", c.yaml, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%q: %v, want %q", c.yaml, err, c.err)
		}
	}
}

func TestPanelPreviewTabs(t *testing.T) {
	file := "/w/queries/query_1.sql"
	now := time.Now()
	be := &fakeBackend{files: []string{file},
		texts: map[string]string{file: "-- sqlh: conn=pg_local\n\nselect *\nfrom orders o\n"},
		hist: &control.HistoryResult{Recent: []history.Entry{
			{ID: "7", SQL: "select *\nfrom orders", Time: now, Conn: "pg_local", Status: "ok", Rows: 3, Affected: -1, File: file, Line: 3},
		}},
		current: map[string]string{"select *\nfrom orders": "select *\nfrom orders o"},
	}
	p := newPanelWith(t, be, Options{Icons: "ascii", Workspace: "/w"})
	// a table: three tabs; the chosen one stays for tables
	key(p, "2")
	selectRow(t, p, "orders")
	if view := plain(p.View()); !strings.Contains(view, "Describe │ DDL │ Sample") {
		t.Fatalf("table tabs:\n%s", view)
	}
	key(p, "tab")
	selectRow(t, p, "customer")
	if view := plain(p.View()); !strings.Contains(view, "CREATE TABLE customer") {
		t.Errorf("DDL not kept:\n%s", view)
	}
	// a column: no DDL
	key(p, "l")
	selectRow(t, p, "name")
	if view := plain(p.View()); !strings.Contains(view, "─ Describe │ Sample ─") {
		t.Errorf("column tabs:\n%s", view)
	}
	// a query file: its text and last run
	key(p, "4")
	selectRow(t, p, "query_1.sql")
	view := plain(p.View())
	for _, want := range []string{"─ Query ─", "queries/query_1.sql", "last run:", "3 rows", "-- sqlh: conn=pg_local", "from orders o"} {
		if !strings.Contains(view, want) {
			t.Errorf("file preview lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "DDL") {
		t.Errorf("file has DDL:\n%s", view)
	}
	// a run: Query, then Diff against the file now
	key(p, "5")
	for i, n := range p.rows() {
		if n.kind == nHistory && n.run.ID == "7" {
			p.tab().sel = i
		}
	}
	drain(p, p.previewCmd())
	if view := plain(p.View()); !strings.Contains(view, "─ Query │ Diff") || !strings.Contains(view, "from orders") {
		t.Errorf("run:\n%s", view)
	}
	key(p, "tab")
	view = plain(p.View())
	for _, want := range []string{"query_1.sql:3 now", "- from orders o", "+ from orders"} {
		if !strings.Contains(view, want) {
			t.Errorf("diff lacks %q:\n%s", want, view)
		}
	}
}

func TestSQLHighlight(t *testing.T) {
	sql := "-- sqlh: conn=pg\nSELECT aep.model_id, count(*) AS n, x::text FROM cade.ai_extraction_process aep " +
		"JOIN t2 ON aep.id = $1 WHERE d > '2020' AND v = :name\nCREATE TABLE t (id integer primary key, name varchar(20))"
	got := map[string]string{}
	for _, tk := range lexSQL(sql) {
		if tk.kind != 's' && tk.class != sqlPlain {
			got[tk.text] = sqlClassNames[tk.class]
		}
	}
	want := map[string]string{"-- sqlh: conn=pg": "comment", "SELECT": "keyword", "count": "function", "AS": "keyword",
		"n": "alias", "text": "type", "FROM": "keyword", "aep": "alias", "JOIN": "keyword", "$1": "parameter",
		"'2020'": "string", ":name": "parameter", "integer": "type", "varchar": "type", "20": "number", "key": "keyword"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	for _, plainWord := range []string{"model_id", "ai_extraction_process", "t2", "cade", "id", "x"} {
		if c, ok := got[plainWord]; ok {
			t.Errorf("%s classed %s", plainWord, c)
		}
	}
	// a block comment and a $$ body over lines; wrapping at spaces
	toks := lexSQL("/* a\nb */ select $$x\ny$$")
	if toks[0].class != sqlComment || toks[0].text != "/* a\nb */" || toks[len(toks)-1].class != sqlString {
		t.Errorf("multi-line tokens: %+v", toks)
	}
	lines := newSyntax(nil).sqlLines("select alpha, beta from gamma", 14)
	if len(lines) != 3 || plain(lines[0]) != "select alpha," || plain(lines[2]) != "gamma" {
		t.Errorf("wrap: %q", lines)
	}
}
