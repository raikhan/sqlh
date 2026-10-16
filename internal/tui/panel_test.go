package tui

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/connedit"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

type fakeBackend struct {
	mu        sync.Mutex
	acts      []control.PanelAction
	copied    []string
	ctx       *control.ContextResult
	sampled   []string
	hist      *control.HistoryResult
	versions  map[string][]history.Entry
	opened    []string
	connected []string
	files     []string
	bodies    []string
	open      []string
	editor    string
	saved     []savedConn
	deleted   []string
	tested    []connedit.Fields
	conf      map[string]connedit.Fields
	texts     map[string]string
	renamed   []string
	reloaded  *control.PanelSettings
	reloadErr error
	current   map[string]string // run SQL → today's version
}

type savedConn struct {
	old, after string
	f          connedit.Fields
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
			{Kind: "constraint", Name: "orders_pkey", Insert: "orders_pkey", Table: "orders", Detail: "primary key (id)"},
			{Kind: "trigger", Name: "orders_audit", Insert: "orders_audit", Table: "orders"},
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
	return []control.Connection{
		{Alias: "pg_local", Driver: "postgresql", Current: true, Environment: "dev", Database: "shop", Schema: "sales",
			Fields: []control.Field{{Label: "Driver", Value: "postgresql"}, {Label: "Host", Value: "localhost"}, {Label: "Port", Value: "5432"}, {Label: "Password", Value: "••••••"}}},
		{Alias: "oracle_prod", Driver: "oracle", Environment: "prod", Group: "CADE", Database: "CADEPROD", Schema: "CADE"},
		{Alias: "oracle_dev", Driver: "oracle", Environment: "dev", Group: "CADE", Database: "CADEV1", Schema: "CADE"},
	}, nil
}
func (f *fakeBackend) AllColumns() ([]control.ColumnRef, error) {
	var out []control.ColumnRef
	for _, t := range []string{"customer", "orders", "order_line", "region"} {
		for _, c := range fakeCols[t] {
			out = append(out, control.ColumnRef{Schema: "sales", Table: t, TableInsert: t, Name: c.Name, Insert: c.Insert, Type: c.Type})
		}
	}
	return out, nil
}
func (f *fakeBackend) Versions(lineage string) ([]history.Entry, error) {
	return f.versions[lineage], nil
}
func (f *fakeBackend) Editor() string                { return f.editor }
func (f *fakeBackend) QueryFiles() ([]string, error) { return f.files, nil }
func (f *fakeBackend) NewQuery(body string) (string, error) {
	f.bodies = append(f.bodies, body)
	p := "/w/queries/query_2.sql"
	f.files = append([]string{p}, f.files...)
	return p, nil
}
func (f *fakeBackend) OpenFile(p string) error {
	f.open = append(f.open, p)
	f.editor = shortPath(p)
	return nil
}
func (f *fakeBackend) ConnectionFields(alias string) (connedit.Fields, error) {
	if c, ok := f.conf[alias]; ok {
		out := connedit.Fields{}
		for k, v := range c {
			out[k] = v
		}
		return out, nil
	}
	return nil, errors.New("no connection " + alias)
}
func (f *fakeBackend) SaveConnection(old string, c connedit.Fields, after string) error {
	f.saved = append(f.saved, savedConn{old, after, c})
	return nil
}
func (f *fakeBackend) DeleteConnection(alias string) error {
	f.deleted = append(f.deleted, alias)
	return nil
}
func (f *fakeBackend) TestConnection(c connedit.Fields) error {
	f.tested = append(f.tested, c)
	return nil
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
func (f *fakeBackend) Connect(a string) error { f.connected = append(f.connected, a); return nil }
func (f *fakeBackend) Refresh() error         { return nil }
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
	case popupDoneMsg: // keep the popup for the test to see
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
	return newPanelWith(t, be, Options{Icons: "ascii", Cursor: cur})
}

func newPanelWith(t *testing.T, be *fakeBackend, opt Options) *Panel {
	t.Helper()
	p := NewPanel(be, nil, opt)
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
		case "shift+tab":
			msg = tea.KeyMsg{Type: tea.KeyShiftTab}
		case "ctrl+y":
			msg = tea.KeyMsg{Type: tea.KeyCtrlY}
		case "ctrl+s":
			msg = tea.KeyMsg{Type: tea.KeyCtrlS}
		case "ctrl+t":
			msg = tea.KeyMsg{Type: tea.KeyCtrlT}
		case "ctrl+r":
			msg = tea.KeyMsg{Type: tea.KeyCtrlR}
		case "ctrl+u":
			msg = tea.KeyMsg{Type: tea.KeyCtrlU}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := p.Update(msg)
		drain(p, cmd)
	}
}

// typeText types into a search or form, rune by rune.
func typeText(p *Panel, s string) {
	for _, r := range s {
		_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
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
			p.tab().sel = i
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

func TestPanelTabs(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	if p.cur != tabStmt {
		t.Errorf("the panel starts on %d, want Statement", p.cur)
	}
	view := plain(p.View())
	for _, want := range []string{"pg_local › shop › sales", "dev", "1 Connections", "2 Objects", "3 Statement", "4 Queries", "5 History"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	key(p, "2")
	view = plain(p.View())
	for _, want := range []string{"Tables", "customer", "orders", "Views", "audit"} {
		if !strings.Contains(view, want) {
			t.Errorf("objects lack %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "orders_id_seq") {
		t.Errorf("Sequences open by default:\n%s", view)
	}
	app := newPanelWith(t, &fakeBackend{}, Options{Icons: "ascii", Standalone: true})
	if app.cur != tabObjects {
		t.Errorf("sqlh tui starts on %d, want Objects", app.cur)
	}
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + view)
	}
}

func TestPanelEnterNavigatesCtrlYInserts(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	key(p, "2")
	selectRow(t, p, "orders")
	key(p, "enter")
	if len(be.acts) != 0 {
		t.Fatalf("enter inserted: %+v", be.acts)
	}
	view := plain(p.View())
	for _, want := range []string{"Relations", "Indexes", "customer_id", "integer", "-> customer"} {
		if !strings.Contains(strings.ReplaceAll(view, "→", "->"), strings.ReplaceAll(want, "->", "->")) && !strings.Contains(view, strings.TrimPrefix(want, "-> ")) {
			t.Errorf("expanded orders lacks %q:\n%s", want, view)
		}
	}
	// Relations → customer → Relations → region: ctrl+y inserts with the path
	selectRow(t, p, "Relations")
	key(p, "l")
	if n := selectRow(t, p, "→ customer"); n.detail != "orders.customer_id" {
		t.Errorf("relation detail %q", n.detail)
	}
	key(p, "l")
	for _, n := range p.rows() {
		if n.label == "Relations" && n.parent != nil && n.parent.label == "→ customer" {
			p.tab().sel = indexOf(p.rows(), n)
			key(p, "l")
		}
	}
	var regionRow *node
	for _, n := range p.rows() {
		if n.label == "→ region" {
			regionRow = n
			p.tab().sel = indexOf(p.rows(), n)
		}
	}
	if regionRow == nil {
		t.Fatalf("no region under customer:\n%s", plain(p.View()))
	}
	if regionRow.detail != "customer.region_id  nullable" {
		t.Errorf("nested detail %q", regionRow.detail)
	}
	key(p, "ctrl+y")
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
	key(p, "2")
	selectRow(t, p, "customer")
	key(p, "l")
	selectRow(t, p, "name")
	key(p, "space") // marks name, moves to region_id
	key(p, "space") // marks region_id
	selectRow(t, p, "id")
	key(p, "ctrl+y")
	if len(be.acts) != 1 || strings.Join(be.acts[0].Columns, ",") != "name,region_id" || be.acts[0].Table != "customer" {
		t.Errorf("acts = %+v", be.acts)
	}
	key(p, "ctrl+y") // marks are cleared
	if strings.Join(be.acts[1].Columns, ",") != "id" {
		t.Errorf("second = %+v", be.acts[1])
	}
}

func TestPanelStatement(t *testing.T) {
	be := &fakeBackend{ctx: &control.ContextResult{InJoin: true, Insert: "join",
		Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 10})
	view := plain(p.View())
	for _, want := range []string{"q.sql:3 · after JOIN", "orders o", "Relations", "→ customer  o.customer_id", "← order_line  o.id"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	// one list of relations: under the table
	n := 0
	for _, r := range p.rows() {
		if r.label == "→ customer" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("customer listed %d times:\n%s", n, view)
	}
	selectRow(t, p, "→ customer")
	key(p, "ctrl+y")
	if len(be.acts) != 1 || be.acts[0].Anchor != "orders" || len(be.acts[0].Path) != 1 || be.acts[0].Path[0].Table != "customer" {
		t.Errorf("acts = %+v", be.acts)
	}
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + view)
	}
}

func searchTexts(p *Panel) []string {
	var out []string
	for _, m := range p.results {
		out = append(out, p.items[m.index].text)
	}
	return out
}

func TestPanelSearch(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	key(p, "2", "/")
	// tables (and views) only by default
	got := strings.Join(searchTexts(p), ",")
	if got != "audit.log,customer,order_line,orders,region,v_orders" {
		t.Errorf("empty query: %s", got)
	}
	typeText(p, "ol")
	if got := searchTexts(p); len(got) == 0 || got[0] != "order_line" {
		t.Errorf("ol: %v", got)
	}
	key(p, "enter") // goes to it
	if p.search || p.cur != tabObjects || p.selected() == nil || p.selected().label != "order_line" {
		t.Errorf("revealed tab %d %+v", p.cur, p.selected())
	}
	if len(be.acts) != 0 {
		t.Errorf("enter inserted: %+v", be.acts)
	}
	// ctrl+y inserts
	key(p, "/")
	typeText(p, "cust")
	key(p, "ctrl+y")
	if len(be.acts) != 1 || be.acts[0].Table != "customer" {
		t.Errorf("acts = %+v", be.acts)
	}
	// prefixes: columns, sequences, connections
	key(p, "/")
	typeText(p, ".region_id")
	if got := searchTexts(p); len(got) != 1 || got[0] != "region_id" {
		t.Errorf("columns: %v", got)
	}
	key(p, "enter")
	if n := p.selected(); n == nil || n.kind != nColumn || n.table != "customer" || n.col.Name != "region_id" {
		t.Errorf("column revealed: %+v", n)
	}
	key(p, "/")
	typeText(p, ".orders.tot")
	if got := searchTexts(p); len(got) == 0 || got[0] != "total" {
		t.Errorf("table.column: %v", got)
	}
	key(p, "esc", "/")
	typeText(p, "~")
	if got := strings.Join(searchTexts(p), ","); got != "orders_id_seq" {
		t.Errorf("sequences: %s", got)
	}
	key(p, "ctrl+u")
	typeText(p, "@prod")
	if got := searchTexts(p); len(got) == 0 || got[0] != "oracle_prod" {
		t.Errorf("connections: %v", got)
	}
	key(p, "enter")
	if p.cur != tabConns || p.selected().label != "oracle_prod" {
		t.Errorf("connection revealed: tab %d %+v", p.cur, p.selected())
	}
	// the tab's own kind: connections in Connections
	key(p, "/")
	if got := strings.Join(searchTexts(p), ","); got != "oracle_dev,oracle_prod,pg_local" {
		t.Errorf("in Connections: %s", got)
	}
}

func TestPanelPreviewAndDDL(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	key(p, "2")
	selectRow(t, p, "Views")
	key(p, "l")
	selectRow(t, p, "v_orders")
	view := plain(p.View())
	if !strings.Contains(view, "Definition") || !strings.Contains(view, "select id, customer_id from orders") {
		t.Errorf("view preview:\n%s", view)
	}
	key(p, "tab")
	if view = plain(p.View()); !strings.Contains(view, "CREATE TABLE v_orders") {
		t.Errorf("DDL tab:\n%s", view)
	}
}

func TestPanelMenu(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	key(p, "2")
	selectRow(t, p, "orders")
	key(p, ".")
	if !strings.Contains(plain(p.View()), "insert SELECT * FROM orders") {
		t.Errorf("menu:\n%s", plain(p.View()))
	}
	key(p, "n")
	if len(be.acts) != 1 || be.acts[0].Mode != "name" {
		t.Errorf("acts = %+v", be.acts)
	}
}

func TestPanelConnections(t *testing.T) {
	be := &fakeBackend{conf: map[string]connedit.Fields{
		"pg_local":    {"alias": "pg_local", "driver": "postgresql", "host": "localhost", "port": "5432", "passwd": "x"},
		"oracle_prod": {"alias": "oracle_prod", "driver": "oracle", "group": "CADE", "environment": "prod"},
	}}
	p := newTestPanel(t, be, nil)
	key(p, "1")
	view := plain(p.View())
	if !regexp.MustCompile(`pg_local +● shop +sales +dev`).MatchString(view) || !strings.Contains(view, "CADE  2") {
		t.Errorf("connections:\n%s", view)
	}
	// a table: the header's columns over the rows'
	var header, row string
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "connection") && strings.Contains(l, "database") {
			header = l
		}
		if strings.Contains(l, "● shop") {
			row = l
		}
	}
	col := func(s, sub string) int { return len([]rune(s[:strings.Index(s, sub)])) }
	if header == "" || col(header, "database") != col(row, "shop") || col(header, "schema") != col(row, "sales") ||
		col(header, "connection") != col(row, "pg_local") || col(header, "env") != col(row, "dev") {
		t.Errorf("not aligned:\n%s\n%s", header, row)
	}
	if strings.Contains(view, "oracle_prod") {
		t.Errorf("the group is open:\n%s", view)
	}
	selectRow(t, p, "pg_local")
	view = plain(p.View())
	for _, want := range []string{"Host", "localhost", "Port", "5432", "Password", "••••••", "● current"} {
		if !strings.Contains(view, want) {
			t.Errorf("describe lacks %q:\n%s", want, view)
		}
	}
	selectRow(t, p, "CADE")
	key(p, "enter")
	selectRow(t, p, "oracle_prod")
	key(p, "enter")
	if strings.Join(be.connected, ",") != "oracle_prod" || p.cur != tabObjects {
		t.Errorf("connect: %v tab %d", be.connected, p.cur)
	}

	// edit
	key(p, "1")
	selectRow(t, p, "pg_local")
	key(p, "E")
	if p.form == nil || p.form.value("host") != "localhost" {
		t.Fatalf("form %+v", p.form)
	}
	if view := plain(p.View()); !strings.Contains(view, "Edit pg_local") || strings.Contains(view, "passwd: x") {
		t.Errorf("form:\n%s", view)
	}
	for p.form.focused().key != "host" {
		key(p, "down")
	}
	key(p, "ctrl+u")
	typeText(p, "db.example")
	key(p, "ctrl+t")
	if len(be.tested) != 1 || be.tested[0]["host"] != "db.example" || !strings.Contains(plain(p.View()), "connected") {
		t.Errorf("test: %v\n%s", be.tested, plain(p.View()))
	}
	key(p, "ctrl+s")
	if p.form != nil || len(be.saved) != 1 || be.saved[0].old != "pg_local" || be.saved[0].f["host"] != "db.example" || be.saved[0].f["passwd"] != "x" {
		t.Errorf("saved %+v", be.saved)
	}
	// environment is a choice
	key(p, "1")
	selectRow(t, p, "pg_local")
	key(p, "E")
	for p.form.focused().key != "environment" {
		key(p, "down")
	}
	key(p, "right", "right")
	if v := p.form.value("environment"); v != "prod" {
		t.Errorf("environment %q", v)
	}
	key(p, "esc")
	if p.form != nil {
		t.Error("esc kept the form")
	}

	// duplicate: a copy after the original
	selectRow(t, p, "CADE")
	selectRow(t, p, "oracle_prod")
	key(p, "D")
	if p.form == nil || p.form.value("alias") != "oracle_prod_copy" || p.form.value("group") != "CADE" {
		t.Fatalf("duplicate form %+v", p.form)
	}
	key(p, "ctrl+s")
	if s := be.saved[len(be.saved)-1]; s.old != "" || s.after != "oracle_prod" || s.f["alias"] != "oracle_prod_copy" {
		t.Errorf("duplicate saved %+v", s)
	}
	// delete: asks first; not the current connection
	selectRow(t, p, "oracle_prod")
	key(p, "X")
	if !strings.Contains(plain(p.View()), "Delete connection oracle_prod") {
		t.Errorf("no question:\n%s", plain(p.View()))
	}
	key(p, "y")
	if strings.Join(be.deleted, ",") != "oracle_prod" {
		t.Errorf("deleted %v", be.deleted)
	}
	selectRow(t, p, "pg_local")
	key(p, "X")
	if p.confirm != nil || len(be.deleted) != 1 {
		t.Error("deleting the current connection was offered")
	}
	// new: in the selected one's group
	selectRow(t, p, "oracle_dev")
	key(p, "C")
	if p.form == nil || p.form.value("group") != "CADE" || p.form.value("driver") != "postgresql" || p.form.old != "" {
		t.Errorf("new form %+v", p.form)
	}
}

func TestPanelSampleOneRow(t *testing.T) {
	be := &fakeBackend{}
	p := newTestPanel(t, be, nil)
	key(p, "2")
	selectRow(t, p, "customer")
	key(p, "tab", "tab") // Sample (auto on dev: loads once the selection rests)
	view := plain(p.View())
	if !strings.Contains(view, "row 1 of 2") || !strings.Contains(view, "acme") || strings.Contains(view, "globex") {
		t.Errorf("sample:\n%s", view)
	}
	key(p, "]")
	view = plain(p.View())
	if !strings.Contains(view, "row 2 of 2") || !strings.Contains(view, "globex") || !strings.Contains(view, "NULL") {
		t.Errorf("next row:\n%s", view)
	}
	// a column: most common values on s
	key(p, "l")
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
	v3 := history.Entry{ID: "3", SQL: "select *\nfrom orders o\nwhere o.total > 100", Time: now, Conn: "pg_local", Status: "ok", Rows: 3, Affected: -1, Lineage: "1"}
	v1 := history.Entry{ID: "1", SQL: "select *\nfrom orders o", Time: now.Add(-time.Hour), Conn: "pg_local", Status: "ok", Rows: 50, Affected: -1, Result: "/w/r.csv", Lineage: "1"}
	be := &fakeBackend{hist: &control.HistoryResult{
		Statement: v3.SQL,
		Versions:  []history.Entry{v3, v1},
		Recent: []history.Entry{
			{ID: "9", SQL: "select 1", Time: now, Conn: "pg_local", Status: "error", Error: "boom", Affected: -1, Lineage: "9"},
			v3, v1,
		},
	}, versions: map[string][]history.Entry{"1": {v3, v1}, "9": {{ID: "9", SQL: "select 1", Time: now, Lineage: "9"}}}}
	p := newTestPanel(t, be, &control.Cursor{File: "/w/q.sql", Line: 3, Col: 1})
	key(p, "5")
	view := plain(p.View())
	if !strings.Contains(view, "This statement") || !strings.Contains(view, "= current") || strings.Count(view, "where o.total > 100") != 2 {
		t.Fatalf("history:\n%s", view)
	}
	// a query (top level) expands to its versions on enter
	for i, n := range p.rows() {
		if n.parent == nil && n.kind == nHistory && n.run.ID == "3" {
			p.tab().sel = i
		}
	}
	key(p, "enter")
	if n := p.selected(); !n.expanded || len(n.children) != 2 {
		t.Fatalf("versions: %+v", n)
	}
	// the older version: diff against the statement, replace from the menu
	var older *node
	for _, n := range p.rows() {
		if n.kind == nHistory && n.run.ID == "1" && n.version {
			older = n
		}
	}
	p.tab().sel = indexOf(p.rows(), older)
	if view = plain(p.View()); !strings.Contains(view, "─ Query │ Diff") || strings.Contains(view, "- where o.total") {
		t.Errorf("query tab:\n%s", view)
	}
	key(p, "tab")
	view = plain(p.View())
	if !strings.Contains(view, "- where o.total > 100") || !strings.Contains(view, "against the statement at the cursor") {
		t.Errorf("diff:\n%s", view)
	}
	key(p, "enter")
	if len(be.acts) != 0 {
		t.Errorf("enter replaced: %+v", be.acts)
	}
	key(p, ".", "r")
	if len(be.acts) != 1 || be.acts[0].Kind != control.ActionReplace || be.acts[0].Text != "select *\nfrom orders o" {
		t.Errorf("acts %+v", be.acts)
	}
	key(p, "o")
	if len(be.opened) != 1 || be.opened[0] != "/w/r.csv" {
		t.Errorf("opened %v", be.opened)
	}
}

func TestTUIStandalone(t *testing.T) {
	be := &fakeBackend{files: []string{"/w/queries/query_1.sql"}}
	p := newPanelWith(t, be, Options{Icons: "ascii", Standalone: true})
	if view := plain(p.View()); !strings.Contains(view, "no editor") {
		t.Fatalf("tui:\n%s", view)
	}
	// no editor: ctrl+y copies and says so
	selectRow(t, p, "orders")
	key(p, "ctrl+y")
	if strings.Join(be.copied, ",") != "orders" || len(be.acts) != 0 || !strings.Contains(plain(p.View()), "copied orders") {
		t.Errorf("copied %v acts %v\n%s", be.copied, be.acts, plain(p.View()))
	}
	key(p, "4")
	if view := plain(p.View()); !strings.Contains(view, "query_1.sql") {
		t.Fatalf("queries:\n%s", view)
	}
	selectRow(t, p, "query_1.sql")
	key(p, "enter")
	if strings.Join(be.open, ",") != "/w/queries/query_1.sql" {
		t.Errorf("open %v", be.open)
	}
	key(p, "2")
	selectRow(t, p, "orders")
	key(p, "n")
	if len(be.bodies) != 1 || be.bodies[0] != "SELECT *\nFROM orders\n" || be.open[len(be.open)-1] != "/w/queries/query_2.sql" {
		t.Errorf("new query %v %v", be.bodies, be.open)
	}
}

func TestPanelKeysConfigurable(t *testing.T) {
	be := &fakeBackend{}
	p := newPanelWith(t, be, Options{Icons: "ascii", Keys: testKeys(map[string]config.KeyList{"insert": {"i"}, "tab2": {"o"}}, nil)})
	key(p, "o")
	if p.cur != tabObjects {
		t.Fatalf("tab2 on o: %d", p.cur)
	}
	selectRow(t, p, "orders")
	key(p, "i")
	if len(be.acts) != 1 || be.acts[0].Table != "orders" {
		t.Errorf("insert on i: %+v", be.acts)
	}
	key(p, "ctrl+y")
	if len(be.acts) != 1 {
		t.Error("ctrl+y still inserts")
	}
	p.msg = ""
	if !strings.Contains(plain(p.View()), "i insert") {
		t.Errorf("footer:\n%s", plain(p.View()))
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

func TestLineDiff(t *testing.T) {
	got := strings.Join(lineDiff("a\nb\nc", "a\nc\nd"), "|")
	if got != "  a|- b|  c|+ d" {
		t.Errorf("got %s", got)
	}
	if got := strings.Join(lineDiff("x > 50", "x > 1"), "|"); got != "- x > 50|+ x > 1" {
		t.Errorf("changed line: %s", got)
	}
}

// testKeys: the keymaps of a panel.keys.
func testKeys(all map[string]config.KeyList, scopes map[string]map[string]config.KeyList) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for scope, km := range (config.PanelKeys{All: all, Scopes: scopes}).Keymaps() {
		out[scope] = km
	}
	return out
}

func (b *fakeBackend) FileText(path string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.texts[path]; ok {
		return t, nil
	}
	return "", fmt.Errorf("no such file %s", path)
}

func (b *fakeBackend) CurrentVersion(file, sql string) (*control.CurrentVersionResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.current[sql]; ok {
		return &control.CurrentVersionResult{Statement: v, Line: 3}, nil
	}
	return &control.CurrentVersionResult{}, nil
}
