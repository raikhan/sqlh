package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

func TestPanelSearchKinds(t *testing.T) {
	p := newTestPanel(t, &fakeBackend{}, nil)
	key(p, "2", "/")
	// the legend while the query is empty, not on the last line
	view := plain(p.View())
	for _, want := range []string{"─ Search ─", "#  indexes", "!  constraints", "^  triggers", "~  sequences", "other kinds"} {
		if !strings.Contains(view, want) {
			t.Errorf("legend lacks %q:\n%s", want, view)
		}
	}
	for _, c := range []struct{ q, want string }{{"#", "orders_customer_idx"}, {"!", "orders_pkey"}, {"^", "orders_audit"}} {
		key(p, "ctrl+u")
		typeText(p, c.q)
		if got := strings.Join(searchTexts(p), ","); got != c.want {
			t.Errorf("%s: %s", c.q, got)
		}
	}
	// a constraint goes to its table's Constraints
	key(p, "enter")
	if n := p.selected(); n == nil || n.kind != nObject || n.obj.Name != "orders_audit" {
		t.Errorf("trigger revealed: %+v", n)
	}
	// tab cycles the kinds, keeping the query
	key(p, "/")
	typeText(p, "ord")
	key(p, "tab")
	if p.query != ".ord" || p.skind != skColumn {
		t.Errorf("tab: %q %d", p.query, p.skind)
	}
	key(p, "shift+tab", "shift+tab")
	if p.query != "*ord" || p.skind != skAll {
		t.Errorf("shift+tab twice: %q %d", p.query, p.skind)
	}
}

func TestPanelStatementSearch(t *testing.T) {
	be := &fakeBackend{ctx: &control.ContextResult{Tables: []control.ScopeTable{
		{Name: "orders", Alias: "o", Ref: "o"}, {Name: "customer", Alias: "c", Ref: "c"}, {Name: "region", Alias: "r", Ref: "r"}}}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 1})
	key(p, "/")
	if got := strings.Join(searchTexts(p), ","); got != "orders o,customer c,region r" {
		t.Fatalf("statement's tables: %s", got)
	}
	typeText(p, "cus")
	key(p, "enter")
	if p.cur != tabStmt || p.selected() == nil || p.selected().label != "customer c" {
		t.Errorf("stays in Statement: tab %d %+v", p.cur, p.selected())
	}
}

func TestPanelRelationsNoLoop(t *testing.T) {
	be := &fakeBackend{ctx: &control.ContextResult{InJoin: true, Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 1})
	selectRow(t, p, "→ customer")
	key(p, "l")
	cust := p.selected()
	if !cust.expanded {
		t.Fatal("customer not open")
	}
	for i, n := range p.rows() {
		if n.label == "Relations" && n.parent != nil && n.parent.id == cust.id {
			p.tab().sel = i
		}
	}
	key(p, "l")
	cust = p.rows()[indexOfID(p.rows(), cust.id)]
	var under []string
	for _, n := range p.rows() {
		if n.kind == nRelation && n.parent != nil && n.parent.parent == cust {
			under = append(under, n.label)
		}
	}
	// orders (the anchor) is not offered again; region (the next hop) is
	if strings.Join(under, ",") != "→ region" {
		t.Errorf("customer's relations: %v", under)
	}
	// the chain still inserts from region
	selectRow(t, p, "→ region")
	key(p, "ctrl+y")
	if len(be.acts) != 1 || len(be.acts[0].Path) != 2 || be.acts[0].Anchor != "orders" {
		t.Errorf("chain: %+v", be.acts)
	}
	// and region, at the end of the chain, has nothing more to offer
	key(p, "l")
	for _, n := range p.rows() {
		if n.kind == nRelation && n.depth > 4 {
			t.Errorf("past region: %s", n.label)
		}
	}
}

func TestPanelMenuJoin(t *testing.T) {
	be := &fakeBackend{ctx: &control.ContextResult{Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 1})
	key(p, "2")
	selectRow(t, p, "customer")
	key(p, ".", "J")
	if len(be.acts) != 1 || be.acts[0].Mode != "join" {
		t.Fatalf("J: %+v", be.acts)
	}
	selectRow(t, p, "region") // not related to orders
	key(p, ".", "J")
	if len(be.acts) != 1 || !strings.Contains(p.msg, "relates to none") {
		t.Errorf("unrelated: %+v %q", be.acts, p.msg)
	}
	// j still moves in the menu
	key(p, ".", "j")
	if p.menu == nil || p.menu.sel != 1 {
		t.Errorf("j in the menu: %+v", p.menu)
	}
}

func indexOfID(rows []*node, id string) int {
	for i, r := range rows {
		if r.id == id {
			return i
		}
	}
	return -1
}

func (f *fakeBackend) RenameQuery(path, name string) (string, error) {
	dst := "/w/queries/" + name + ".sql"
	files := append([]string(nil), f.files...)
	for i, x := range files {
		if x == path {
			files[i] = dst
		}
	}
	f.files = files
	f.renamed = append(f.renamed, path+" → "+dst)
	return dst, nil
}

func (f *fakeBackend) DeleteQuery(path string) error {
	var keep []string
	for _, x := range f.files {
		if x != path {
			keep = append(keep, x)
		}
	}
	f.files = keep
	f.deleted = append(f.deleted, path)
	return nil
}

func TestPanelQueryRenameDelete(t *testing.T) {
	be := &fakeBackend{files: []string{"/w/queries/query_1.sql", "/w/queries/query_2.sql"}}
	p := newTestPanel(t, be, nil)
	key(p, "4")
	selectRow(t, p, "query_1.sql")
	key(p, "R")
	if p.prompt == nil || p.prompt.input.Value() != "query_1" {
		t.Fatalf("prompt: %+v", p.prompt)
	}
	key(p, "ctrl+u")
	typeText(p, "orders_report")
	key(p, "enter")
	if strings.Join(be.renamed, ",") != "/w/queries/query_1.sql → /w/queries/orders_report.sql" {
		t.Fatalf("renamed: %v", be.renamed)
	}
	if n := p.selected(); n == nil || n.label != "orders_report.sql" {
		t.Errorf("selected after rename: %+v", n)
	}
	key(p, "X")
	if p.confirm == nil {
		t.Fatal("no confirm")
	}
	key(p, "y")
	if strings.Join(be.deleted, ",") != "/w/queries/orders_report.sql" {
		t.Errorf("deleted: %v", be.deleted)
	}
	if got := strings.Join(labels(p.rows()), ","); got != "query_2.sql" {
		t.Errorf("still listed: %s", got)
	}
}

func (f *fakeBackend) EnsureFile(path, text string) (bool, error) {
	if f.texts == nil {
		f.texts = map[string]string{}
	}
	if _, ok := f.texts[path]; ok {
		return false, nil
	}
	f.texts[path] = text
	return true, nil
}

func TestPanelRunFileRecreated(t *testing.T) {
	now := time.Now()
	be := &fakeBackend{hist: &control.HistoryResult{Recent: []history.Entry{
		{ID: "1", SQL: "select *\nfrom orders", Time: now, Conn: "pg_local", Status: "ok", File: "/w/queries/gone.sql", Line: 3}}}}
	p := newTestPanel(t, be, nil)
	key(p, "5")
	for i, n := range p.rows() {
		if n.kind == nHistory {
			p.tab().sel = i
		}
	}
	key(p, "e")
	if be.texts["/w/queries/gone.sql"] != "-- sqlh: conn=pg_local\n\nselect *\nfrom orders\n" || strings.Join(be.open, ",") != "/w/queries/gone.sql" {
		t.Errorf("recreated %q, opened %v", be.texts["/w/queries/gone.sql"], be.open)
	}
	if !strings.Contains(p.msg, "recreated") {
		t.Errorf("msg %q", p.msg)
	}
}

func TestPanelHistoryByConnection(t *testing.T) {
	now := time.Now()
	be := &fakeBackend{hist: &control.HistoryResult{Recent: []history.Entry{
		{ID: "1", SQL: "select 1", Time: now, Conn: "pg_local", Status: "ok"},
		{ID: "2", SQL: "select 2", Time: now, Conn: "oracle_dev", Status: "ok"}}}}
	p := newTestPanel(t, be, nil)
	key(p, "5")
	view := plain(p.View())
	if !strings.Contains(view, "select 1") || strings.Contains(view, "select 2") || !strings.Contains(view, "recent runs on pg_local") {
		t.Errorf("filtered:\n%s", view)
	}
	key(p, "c")
	if view = plain(p.View()); !strings.Contains(view, "select 2") || !strings.Contains(view, "all connections") {
		t.Errorf("all:\n%s", view)
	}
}

func (f *fakeBackend) ConfigPath() (string, error) { return "/w/config.yml", nil }

func (f *fakeBackend) ReloadSettings() (*control.PanelSettings, error) {
	if f.reloadErr != nil {
		return nil, f.reloadErr
	}
	return f.reloaded, nil
}

func TestPanelReloadConfig(t *testing.T) {
	be := &fakeBackend{reloaded: &control.PanelSettings{Keys: testKeys(map[string]config.KeyList{"insert": {"i"}}, nil)}}
	p := newTestPanel(t, be, nil)
	key(p, "2")
	selectRow(t, p, "orders")
	key(p, "ctrl+r")
	if !strings.Contains(p.msg, "config reloaded") {
		t.Fatalf("msg %q", p.msg)
	}
	key(p, "i")
	if len(be.acts) != 1 {
		t.Errorf("the new key: %+v", be.acts)
	}
	// both in the palette
	key(p, "?")
	typeText(p, "config")
	view := plain(p.View())
	if !strings.Contains(view, "Edit the config file") || !strings.Contains(view, "Reload the config file") {
		t.Errorf("palette:\n%s", view)
	}
	key(p, "esc", "esc")
	be.reloadErr = errors.New("panel.keys: unknown action nope")
	key(p, "ctrl+r")
	if !strings.Contains(p.popup, "unknown action nope") {
		t.Errorf("popup %q", p.popup)
	}
	key(p, "x") // closes the popup
	key(p, "i") // the keys stay
	if len(be.acts) != 2 {
		t.Errorf("keys after a failed reload: %+v", be.acts)
	}
	for _, bad := range []string{"ctrl+R", "ctrl+:"} {
		_, err := config.Parse([]byte("panel:\n  keys:\n    reloadConfig: \"" + bad + "\"\n"))
		if err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func (f *fakeBackend) Chains(from []string, target string) (*control.ChainsResult, error) {
	cust := control.JoinHop{Table: "customer", Cols: [][2]string{{"customer_id", "id"}}}
	if target == "" {
		return &control.ChainsResult{Targets: []control.ChainTarget{
			{Table: "customer", Kind: "table", Joins: 1, Chains: 1, Via: []string{"orders", "customer"}},
			{Table: "order_line", Kind: "table", Joins: 1, Chains: 1, Via: []string{"orders", "order_line"}},
			{Table: "region", Kind: "table", Joins: 2, Chains: 1, Via: []string{"orders", "customer", "region"}},
		}}, nil
	}
	if target == "region" {
		return &control.ChainsResult{Chains: []control.Chain{{Anchor: "orders", Hops: []control.JoinHop{cust,
			{Table: "region", Cols: [][2]string{{"region_id", "id"}}}}}}}, nil
	}
	return &control.ChainsResult{}, nil
}

func TestPanelChainSearch(t *testing.T) {
	be := &fakeBackend{ctx: &control.ContextResult{InJoin: true, Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 1})
	key(p, "/")
	typeText(p, ":")
	if p.skind != skChain || strings.Join(searchTexts(p), ",") != "customer,order_line,region" {
		t.Fatalf("chains: %d %v", p.skind, searchTexts(p))
	}
	if view := plain(p.View()); !strings.Contains(view, "2 joins · orders → customer → region") {
		t.Errorf("detail:\n%s", view)
	}
	typeText(p, "reg")
	key(p, "enter")
	n := p.selected()
	if p.cur != tabStmt || n == nil || n.kind != nRelation || n.table != "region" || len(n.path) != 2 || n.anchor != "orders" {
		t.Fatalf("walked to %+v (tab %d)\n%s", n, p.cur, plain(p.View()))
	}
	// ctrl+y inserts the chain as a join
	key(p, "/")
	typeText(p, ":reg")
	key(p, "ctrl+y")
	if len(be.acts) != 1 || be.acts[0].Mode != "join" || be.acts[0].Anchor != "orders" || len(be.acts[0].Path) != 2 {
		t.Errorf("insert: %+v", be.acts)
	}
	// Objects keeps : for tables
	key(p, "2", "/")
	typeText(p, ":reg")
	if p.skind != skTable {
		t.Errorf("Objects : is %d", p.skind)
	}
}
