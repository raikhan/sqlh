package tui

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

type fakeBackend struct {
	mu      sync.Mutex
	acts    []control.PanelAction
	copied  []string
	ctx     *control.ContextResult
	schemas []string
	sampled []string
	hist    *control.HistoryResult
	opened  []string
}

var fakeCatalog = &control.CatalogResult{
	Connection: "pg_local", Driver: "postgresql", Environment: "dev", Schema: "sales", ViewsLoaded: true, ObjectsLoaded: true,
	SamplePolicy: "auto",
	Schemas: []control.CatalogSchema{
		{Name: "sales", Default: true, Objects: []control.CatalogObject{
			{Kind: "table", Name: "customer", Insert: "customer", Columns: 3},
			{Kind: "table", Name: "order_line", Insert: "order_line", Columns: 3},
			{Kind: "table", Name: "orders", Insert: "orders", Columns: 3},
			{Kind: "table", Name: "region", Insert: "region", Columns: 2},
			{Kind: "view", Name: "v_orders", Insert: "v_orders", Columns: 2},
			{Kind: "sequence", Name: "orders_id_seq", Insert: "orders_id_seq"},
			{Kind: "index", Name: "orders_customer_idx", Insert: "orders_customer_idx", Table: "orders", Detail: "(customer_id)"},
		}},
		{Name: "audit", Objects: []control.CatalogObject{
			{Name: "log", Insert: "audit.log", Columns: 4},
		}},
	},
}

var fakeCols = map[string][]control.ColumnItem{
	"customer":   {{Name: "id", Insert: "id", Type: "integer", Key: "YES"}, {Name: "name", Insert: "name", Type: "text"}, {Name: "region_id", Insert: "region_id", Type: "integer"}},
	"orders":     {{Name: "id", Insert: "id", Type: "integer", Key: "YES"}, {Name: "customer_id", Insert: "customer_id", Type: "integer"}, {Name: "total", Insert: "total", Type: "numeric"}},
	"order_line": {{Name: "id", Insert: "id", Type: "integer", Key: "YES"}, {Name: "order_id", Insert: "order_id", Type: "integer"}, {Name: "qty", Insert: "qty", Type: "integer"}},
	"region":     {{Name: "id", Insert: "id", Type: "integer", Key: "YES"}, {Name: "name", Insert: "name", Type: "text"}},
	"v_orders":   {{Name: "id", Insert: "id", Type: "integer"}, {Name: "customer_id", Insert: "customer_id", Type: "integer"}},
}

var fakeRels = map[string]*control.RelationsResult{
	"orders": {Table: "orders", Insert: "orders", Kind: "table", ViewsLoaded: true, Relations: []control.RelationItem{
		{Table: "customer", Insert: "customer", Kind: "table", Edge: "fk", Marker: "→", Out: true, Cols: [][2]string{{"customer_id", "id"}}, Condition: "customer.id = customer_id"},
		{Table: "order_line", Insert: "order_line", Kind: "table", Edge: "fk", Marker: "←", Cols: [][2]string{{"id", "order_id"}}, Condition: "order_line.order_id = id"},
		{Table: "v_orders", Insert: "v_orders", Kind: "view", Edge: "uses", Marker: "◇"},
	}},
	"customer": {Table: "customer", Insert: "customer", Kind: "table", ViewsLoaded: true, Relations: []control.RelationItem{
		{Table: "orders", Insert: "orders", Kind: "table", Edge: "fk", Marker: "←", Cols: [][2]string{{"id", "customer_id"}}, Condition: "orders.customer_id = id"},
		{Table: "region", Insert: "region", Kind: "table", Edge: "fk", Marker: "→", Out: true, Cols: [][2]string{{"region_id", "id"}}, Condition: "region.id = region_id", Note: "nullable"},
	}},
	"region":     {Table: "region", Insert: "region", Kind: "table", ViewsLoaded: true, Relations: []control.RelationItem{}},
	"order_line": {Table: "order_line", Insert: "order_line", Kind: "table", ViewsLoaded: true, Relations: []control.RelationItem{}},
	"v_orders":   {Table: "v_orders", Insert: "v_orders", Kind: "view", ViewsLoaded: true, Definition: "select id, customer_id from orders", Relations: []control.RelationItem{}},
}

func (f *fakeBackend) Catalog() (*control.CatalogResult, error) { return fakeCatalog, nil }
func (f *fakeBackend) Connections() ([]control.Connection, error) {
	return []control.Connection{{Alias: "pg_local", Driver: "postgresql", Current: true, Environment: "dev"}, {Alias: "oracle_prod", Driver: "oracle", Environment: "prod"}}, nil
}
func (f *fakeBackend) Columns(t string) ([]control.ColumnItem, error) {
	if c, ok := fakeCols[t]; ok {
		return c, nil
	}
	return nil, errors.New("unknown table " + t)
}
func (f *fakeBackend) Relations(t string) (*control.RelationsResult, error) {
	if r, ok := fakeRels[t]; ok {
		return r, nil
	}
	return nil, errors.New("unknown table " + t)
}
func (f *fakeBackend) Context(control.Cursor) (*control.ContextResult, error) {
	if f.ctx == nil {
		return &control.ContextResult{}, nil
	}
	return f.ctx, nil
}
func (f *fakeBackend) DDL(t string) (*control.DDLResult, error) {
	return &control.DDLResult{Name: t, Kind: "table", DDL: "CREATE TABLE " + t + " (\n  id integer\n);"}, nil
}
func (f *fakeBackend) Sample(t, col string) (*control.SampleResult, error) {
	f.mu.Lock()
	f.sampled = append(f.sampled, sampleKey(t, col))
	f.mu.Unlock()
	if col != "" {
		a, n1, b, n2 := "north", "2", "south", "1"
		return &control.SampleResult{Table: t, Columns: []string{col, "n"}, Rows: [][]*string{{&a, &n1}, {&b, &n2}}}, nil
	}
	one, acme, two, globex := "1", "acme", "2", "globex"
	return &control.SampleResult{Table: t, Columns: []string{"id", "name", "region_id"},
		Rows: [][]*string{{&one, &acme, &one}, {&two, &globex, nil}}}, nil
}
func (f *fakeBackend) History(cur *control.Cursor) (*control.HistoryResult, error) {
	if f.hist != nil {
		return f.hist, nil
	}
	return &control.HistoryResult{}, nil
}
func (f *fakeBackend) Open(file string) error { f.opened = append(f.opened, file); return nil }
func (f *fakeBackend) Connect(string) error   { return nil }
func (f *fakeBackend) UseSchema(s string) error {
	f.schemas = append(f.schemas, s)
	return nil
}
func (f *fakeBackend) Refresh() error { return nil }
func (f *fakeBackend) Act(a control.PanelAction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, a)
	return nil
}
func (f *fakeBackend) Copy(t string) error { f.copied = append(f.copied, t); return nil }
func (f *fakeBackend) BackToEditor() error { return nil }
func (f *fakeBackend) EditorAlive() bool   { return true }

// drain runs a command and everything it leads to, synchronously.
func drain(p *Panel, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	switch m := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range m {
			drain(p, c)
		}
	case reloadMsg:
		p.reloadAt = p.reloadAt.Add(-1e12)
		_, next := p.Update(m)
		drain(p, next)
	default:
		if _, ok := msg.(tea.QuitMsg); ok {
			return
		}
		_, next := p.Update(m)
		drain(p, next)
	}
}

func newTestPanel(t *testing.T, be *fakeBackend, cur *control.Cursor) *Panel {
	t.Helper()
	p := NewPanel(be, nil, Options{Icons: "ascii", Cursor: cur})
	p.noTick = true
	p.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
	drain(p, p.Init())
	return p
}

func key(p *Panel, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := p.Update(msg)
		drain(p, cmd)
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

// selectRow moves the selection to the row whose label is label.
func selectRow(t *testing.T, p *Panel, label string) *node {
	t.Helper()
	for i, n := range p.rows() {
		if n.label == label {
			p.sel = i
			drain(p, p.previewCmd())
			return n
		}
	}
	var labels []string
	for _, n := range p.rows() {
		labels = append(labels, n.label)
	}
	t.Fatalf("no row %q in %q", label, labels)
	return nil
}

func TestPanelTree(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	view := plain(p.View())
	for _, want := range []string{"pg_local › sales", "dev", "sales", "Tables", "customer", "orders", "Views", "audit", "Connections"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + view)
	}
}

func TestPanelExpandAndInsert(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	selectRow(t, p, "orders")
	key(p, "l")
	view := plain(p.View())
	for _, want := range []string{"Relations", "Indexes", "customer_id", "integer"} {
		if !strings.Contains(view, want) {
			t.Errorf("expanded orders lacks %q:\n%s", want, view)
		}
	}
	// Relations → customer → Relations → region: Enter inserts with the path
	selectRow(t, p, "Relations")
	key(p, "l")
	selectRow(t, p, "→ customer")
	key(p, "l")
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + plain(p.View()))
	}
	// customer's own relations, under the relation node
	var regionRow *node
	for _, n := range p.rows() {
		if n.label == "Relations" && n.parent != nil && n.parent.label == "→ customer" {
			p.sel = indexOf(p.rows(), n)
			key(p, "l")
		}
	}
	for _, n := range p.rows() {
		if n.label == "→ region" {
			regionRow = n
			p.sel = indexOf(p.rows(), n)
		}
	}
	if regionRow == nil {
		t.Fatalf("no region under customer:\n%s", plain(p.View()))
	}
	key(p, "enter")
	if len(be.acts) != 1 {
		t.Fatalf("acts = %+v", be.acts)
	}
	a := be.acts[0]
	if a.Kind != control.ActionTable || a.Table != "region" || a.Anchor != "orders" || len(a.Path) != 2 ||
		a.Path[0].Table != "customer" || a.Path[1].Table != "region" {
		t.Errorf("action = %+v", a)
	}
}

func indexOf(rows []*node, n *node) int {
	for i, r := range rows {
		if r == n {
			return i
		}
	}
	return -1
}

func TestPanelColumnsMarked(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	selectRow(t, p, "customer")
	key(p, "l")
	selectRow(t, p, "name")
	key(p, "space") // marks name, moves to region_id
	key(p, "space") // marks region_id
	selectRow(t, p, "id")
	key(p, "enter")
	if len(be.acts) != 1 || strings.Join(be.acts[0].Columns, ",") != "name,region_id" || be.acts[0].Table != "customer" {
		t.Errorf("acts = %+v", be.acts)
	}
	// marks are cleared
	key(p, "enter")
	if strings.Join(be.acts[1].Columns, ",") != "id" {
		t.Errorf("second = %+v", be.acts[1])
	}
}

func TestPanelStatement(t *testing.T) {
	be := &fakeBackend{ctx: &control.ContextResult{InJoin: true, Insert: "join",
		Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 10})
	view := plain(p.View())
	for _, want := range []string{"In this statement", "q.sql:3 · after JOIN", "orders o", "→ customer", "o.customer_id", "← order_line"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	selectRow(t, p, "→ customer")
	key(p, "enter")
	if len(be.acts) != 1 || be.acts[0].Anchor != "orders" || len(be.acts[0].Path) != 1 || be.acts[0].Path[0].Table != "customer" {
		t.Errorf("acts = %+v", be.acts)
	}
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + view)
	}
}

func TestPanelSearch(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	key(p, "/", "o", "l")
	view := plain(p.View())
	if !strings.Contains(view, "/ ol") || !strings.Contains(view, "order_line") {
		t.Errorf("search view:\n%s", view)
	}
	if got := p.items[p.results[0].index].text; got != "order_line" {
		t.Errorf("first result %q", got)
	}
	key(p, "enter")
	if len(be.acts) != 1 || be.acts[0].Table != "order_line" {
		t.Errorf("acts = %+v", be.acts)
	}
	// tab reveals in the tree
	key(p, "/", "l", "o", "g", "tab")
	if n := p.selected(); n == nil || n.label != "log" || n.schema != "audit" {
		t.Errorf("revealed %+v", n)
	}
}

func TestPanelPreviewAndDDL(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	selectRow(t, p, "Views")
	key(p, "l")
	selectRow(t, p, "v_orders")
	view := plain(p.View())
	if !strings.Contains(view, "Definition") || !strings.Contains(view, "select id, customer_id from orders") {
		t.Errorf("view preview:\n%s", view)
	}
	key(p, "tab")
	view = plain(p.View())
	if !strings.Contains(view, "CREATE TABLE v_orders") {
		t.Errorf("DDL tab:\n%s", view)
	}
}

func TestPanelMenuAndUseSchema(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	selectRow(t, p, "orders")
	key(p, ".")
	if !strings.Contains(plain(p.View()), "insert SELECT * FROM orders") {
		t.Errorf("menu:\n%s", plain(p.View()))
	}
	key(p, "n")
	if len(be.acts) != 1 || be.acts[0].Mode != "name" {
		t.Errorf("acts = %+v", be.acts)
	}
	selectRow(t, p, "audit")
	key(p, "u")
	if strings.Join(be.schemas, ",") != "audit" {
		t.Errorf("schemas = %v", be.schemas)
	}
}

func TestFuzzy(t *testing.T) {
	cases := []struct {
		pat  string
		best string
		cand []string
	}{
		{"ol", "order_line", []string{"orders", "order_line", "v_orders", "log"}},
		{"cust", "customer", []string{"v_customer_orders", "customer", "customer_region"}},
		{"cr", "customer_region", []string{"customer", "customer_region", "carrier"}},
	}
	for _, c := range cases {
		m := fuzzyRank(c.pat, c.cand)
		if len(m) == 0 || c.cand[m[0].index] != c.best {
			var got []string
			for _, x := range m {
				got = append(got, c.cand[x.index])
			}
			t.Errorf("%q: %v", c.pat, got)
		}
	}
	if _, _, ok := fuzzyScore("xz", "orders"); ok {
		t.Error("xz matched orders")
	}
}

func TestPanelSample(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	selectRow(t, p, "customer")
	key(p, "tab", "tab") // Sample (auto on dev: loads once the selection rests)
	view := plain(p.View())
	if !strings.Contains(view, "acme · globex") || !strings.Contains(view, "NULL×1") {
		t.Errorf("sample:\n%s", view)
	}
	key(p, "v")
	view = plain(p.View())
	if !strings.Contains(view, "row 1/2") || !strings.Contains(view, "acme") {
		t.Errorf("record view:\n%s", view)
	}
	key(p, "n")
	if !strings.Contains(plain(p.View()), "globex") {
		t.Errorf("next row:\n%s", plain(p.View()))
	}
	// a column: most common values on s
	key(p, "v", "l")
	selectRow(t, p, "region_id")
	key(p, "s")
	if view := plain(p.View()); !strings.Contains(view, "most common values") || !strings.Contains(view, "north") {
		t.Errorf("top values:\n%s", view)
	}
	// views are not sampled automatically
	be.sampled = nil
	selectRow(t, p, "Views")
	key(p, "l")
	selectRow(t, p, "v_orders")
	if len(be.sampled) != 0 || !strings.Contains(plain(p.View()), "views on request") {
		t.Errorf("view sampled %v:\n%s", be.sampled, plain(p.View()))
	}
}

func TestPanelHistory(t *testing.T) {
	now := time.Now()
	be := &fakeBackend{hist: &control.HistoryResult{
		Statement: "select *\nfrom orders o\nwhere o.total > 100",
		Versions: []history.Entry{
			{ID: "3", SQL: "select *\nfrom orders o\nwhere o.total > 100", Time: now, Conn: "pg_local", Status: "ok", Rows: 3, Affected: -1, Lineage: "1"},
			{ID: "1", SQL: "select *\nfrom orders o", Time: now.Add(-time.Hour), Conn: "pg_local", Status: "ok", Rows: 50, Affected: -1, Result: "/w/r.csv", Lineage: "1"},
		},
		Recent: []history.Entry{{ID: "9", SQL: "select 1", Time: now, Conn: "pg_local", Status: "error", Error: "boom", Affected: -1}},
	}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 1})
	view := plain(p.View())
	if !strings.Contains(view, "History") || !strings.Contains(view, "This statement") || !strings.Contains(view, "= current") {
		t.Fatalf("history:\n%s", view)
	}
	var older *node
	for _, n := range p.rows() {
		if n.kind == nHistory && n.run.ID == "1" {
			older = n
		}
	}
	p.sel = indexOf(p.rows(), older)
	view = plain(p.View())
	if !strings.Contains(view, "Enter replaces the statement:") || !strings.Contains(view, "- where o.total > 100") {
		t.Errorf("diff:\n%s", view)
	}
	key(p, "enter")
	if len(be.acts) != 1 || be.acts[0].Kind != control.ActionReplace || be.acts[0].Text != "select *\nfrom orders o" {
		t.Errorf("acts %+v", be.acts)
	}
	key(p, "o")
	if len(be.opened) != 1 || be.opened[0] != "/w/r.csv" {
		t.Errorf("opened %v", be.opened)
	}
	// the current version: nothing to replace
	for _, n := range p.rows() {
		if n.kind == nHistory && n.run.ID == "3" {
			p.sel = indexOf(p.rows(), n)
		}
	}
	key(p, "enter")
	if len(be.acts) != 1 {
		t.Errorf("replaced with the current text: %+v", be.acts)
	}
}

func TestLineDiff(t *testing.T) {
	got := strings.Join(lineDiff("a\nb\nc", "a\nc\nd"), "|")
	if got != "  a|- b|  c|+ d" {
		t.Errorf("got %s", got)
	}
	if got := strings.Join(lineDiff("x > 50", "x > 1"), "|"); got != "- x > 50|+ x > 1" {
		t.Errorf("changed line: %s", got)
	}
}

type fakeWorkspace struct {
	fakeBackend
	files  []string
	bodies []string
	open   []string
	editor string
}

func (f *fakeWorkspace) QueryFiles() ([]string, error) { return f.files, nil }
func (f *fakeWorkspace) NewQuery(body string) (string, error) {
	f.bodies = append(f.bodies, body)
	p := "/w/queries/query_2.sql"
	f.files = append([]string{p}, f.files...)
	return p, nil
}
func (f *fakeWorkspace) OpenFile(p string) error {
	f.open = append(f.open, p)
	f.editor = shortPath(p)
	return nil
}
func (f *fakeWorkspace) Editor() string { return f.editor }

func TestTUIStandalone(t *testing.T) {
	be := &fakeWorkspace{files: []string{"/w/queries/query_1.sql"}}
	p := NewPanel(be, nil, Options{Icons: "ascii", Standalone: true})
	p.noTick = true
	p.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
	drain(p, p.Init())
	view := plain(p.View())
	if !strings.Contains(view, "Query files") || !strings.Contains(view, "query_1.sql") || !strings.Contains(view, "no editor") {
		t.Fatalf("tui:\n%s", view)
	}
	selectRow(t, p, "query_1.sql")
	key(p, "enter")
	if strings.Join(be.open, ",") != "/w/queries/query_1.sql" {
		t.Errorf("open %v", be.open)
	}
	selectRow(t, p, "orders")
	key(p, "n")
	if len(be.bodies) != 1 || be.bodies[0] != "SELECT *\nFROM orders\n" || be.open[len(be.open)-1] != "/w/queries/query_2.sql" {
		t.Errorf("new query %v %v", be.bodies, be.open)
	}
}
