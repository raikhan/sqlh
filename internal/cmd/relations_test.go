package cmd

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/control"
)

// fakeAPI serves canned relationships.
type fakeAPI struct {
	rels map[string]*control.RelationsResult
	all  []control.TableItem
}

func (f fakeAPI) schemas(bool) ([]control.SchemaItem, error)               { return nil, nil }
func (f fakeAPI) tables(string, bool) ([]control.TableItem, error)         { return f.all, nil }
func (f fakeAPI) columns(string) ([]control.ColumnItem, error)             { return nil, nil }
func (f fakeAPI) databases() ([]string, error)                             { return nil, nil }
func (f fakeAPI) context(string, int, int) (*control.ContextResult, error) { return nil, nil }
func (f fakeAPI) notify(string)                                            {}
func (f fakeAPI) close()                                                   {}
func (f fakeAPI) relations(t string) (*control.RelationsResult, error) {
	if r, ok := f.rels[t]; ok {
		return r, nil
	}
	return nil, errors.New("unknown table " + t)
}

var api = fakeAPI{
	rels: map[string]*control.RelationsResult{
		"orders": {Insert: "orders", Relations: []control.RelationItem{
			{Table: "customer", Insert: "customer", Kind: "table", Edge: "fk", Marker: "→", Out: true, Cols: [][2]string{{"customer_id", "id"}}},
			{Table: "order_line", Insert: "order_line", Kind: "table", Edge: "fk", Marker: "←", Cols: [][2]string{{"id", "order_id"}}},
			{Table: "v_orders", Insert: "v_orders", Kind: "view", Edge: "uses", Marker: "◇", Cols: [][2]string{{"id", "order_id"}}},
			{Table: "v_union", Insert: "v_union", Kind: "view", Edge: "uses", Marker: "◇"},
		}},
		"customer": {Insert: "customer", Relations: []control.RelationItem{
			{Table: "orders", Insert: "orders", Kind: "table", Edge: "fk", Marker: "←", Cols: [][2]string{{"id", "customer_id"}}},
			{Table: "region", Insert: "region", Kind: "table", Edge: "fk", Marker: "→", Out: true, Cols: [][2]string{{"region_id", "id"}}, Note: "nullable"},
			{Table: "v_union", Insert: "v_union", Kind: "view", Edge: "uses", Marker: "◇"},
		}},
	},
	all: []control.TableItem{
		{Name: "customer", Insert: "customer", Default: true}, {Name: "order_line", Insert: "order_line", Default: true},
		{Name: "orders", Insert: "orders", Default: true}, {Name: "product", Insert: "product", Default: true},
		{Name: "region", Insert: "region", Default: true},
	},
}

// display returns the visible part of picker lines (dim: "~name").
func display(lines []string) []string {
	var out []string
	for _, l := range lines {
		d := strings.SplitN(l, "\t", 3)[2]
		if strings.HasPrefix(d, ansiDim) {
			d = "~" + d
		}
		d = strings.NewReplacer(ansiDim, "", ansiReset, "").Replace(d)
		out = append(out, d)
	}
	return out
}

func TestNavLines(t *testing.T) {
	st := &navState{Ctx: control.ContextResult{InJoin: true, Insert: "join",
		Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}}
	lines, err := navLines(api, st)
	if err != nil {
		t.Fatal(err)
	}
	// related first, then the other tables, dimmed
	want := []string{"customer", "order_line", "v_orders", "v_union", "~orders", "~product", "~region"}
	if got := display(lines); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("root:\n%s", strings.Join(got, "\n"))
	}
	// the second field (preview, Ctrl-R) is the plain name
	if f := strings.Split(lines[4], "\t")[1]; f != "orders" {
		t.Errorf("name field %q", f)
	}

	// two tables: the focused one's relationships first; a table related to
	// both is told apart by the columns; a shared view dependency once
	st.Ctx.Tables = append(st.Ctx.Tables, control.ScopeTable{Name: "customer", Alias: "c", Ref: "c"})
	st.Ctx.Focus = "customer"
	st.Related = true
	both := api
	both.rels = map[string]*control.RelationsResult{"orders": api.rels["orders"], "customer": {Insert: "customer",
		Relations: append([]control.RelationItem{{Table: "order_line", Insert: "order_line", Kind: "table", Edge: "fk", Marker: "←",
			Cols: [][2]string{{"id", "customer_id"}}}}, api.rels["customer"].Relations...)}}
	lines, _ = navLines(both, st)
	want = []string{"order_line (c.id)", "region", "order_line (o.id)", "v_orders", "v_union"}
	if got := display(lines); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("focus customer:\n%s", strings.Join(got, "\n"))
	}
	item, _ := parseNavItem(strings.SplitN(lines[1], "\t", 2)[0])
	if item.Table != "region" || item.Ref != "c" || item.Anchor != "customer" {
		t.Errorf("item %+v", item)
	}
	withRegion := api
	withRegion.rels = map[string]*control.RelationsResult{"region": {Insert: "region", Kind: "table", ViewsLoaded: true}}
	text, err := previewText(withRegion, item)
	if err != nil || !strings.HasPrefix(text, "c → "+ansiCyan+"region"+ansiReset+".id = c.region_id  "+ansiDim+"nullable"+ansiReset+"\n\n") {
		t.Errorf("preview:\n%s (%v)", text, err)
	}

	// drilled into customer
	st.Path = []navItem{{Table: "customer", Ref: "o", Cols: [][2]string{{"customer_id", "id"}}}}
	if p := st.prompt(); p != "o › customer" {
		t.Errorf("prompt %q", p)
	}
	lines, _ = navLines(api, st)
	if got := display(lines); strings.Join(got, ",") != "orders,region,v_union" {
		t.Errorf("drilled:\n%s", strings.Join(got, "\n"))
	}
}

func TestDescribeText(t *testing.T) {
	rel := &control.RelationsResult{Insert: "orders", Kind: "table", ViewsLoaded: true, Relations: []control.RelationItem{
		{Insert: "customer", Edge: "fk", Marker: "→", Out: true, Condition: "customer.id = customer_id"},
		{Insert: "order_line", Edge: "fk", Marker: "←", Condition: "order_line.order_id = id", Note: "1:1"},
		{Insert: "v_orders", Edge: "uses", Marker: "◇"},
	}}
	cols := []control.ColumnItem{{Insert: "id", Type: "integer", Key: "YES"}, {Insert: "customer_id", Type: "integer"}}
	want := `orders table

Name         Type     PK
id           integer  PK
customer_id  integer

Relationships

→ customer.id = customer_id
← order_line.order_id = id  1:1

Used by views: v_orders
`
	if got := describeText(rel, cols); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// the preview colours the related table and the notes
	colored := textStyle{color: true}.describe(rel, cols)
	for _, part := range []string{"→ " + ansiCyan + "customer" + ansiReset + ".id = customer_id\n",
		"← " + ansiCyan + "order_line" + ansiReset + ".order_id = id  " + ansiDim + "1:1" + ansiReset + "\n"} {
		if !strings.Contains(colored, part) {
			t.Errorf("colored output lacks %q:\n%s", part, colored)
		}
	}
}

func TestChooseInsert(t *testing.T) {
	ctx := control.ContextResult{InJoin: true, Insert: "join", LowercaseKeywords: true, Indent: "  ",
		Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}
	customer := navItem{Table: "customer", Ref: "o", Cols: [][2]string{{"customer_id", "id"}}, Anchor: "orders"}
	region := navItem{Table: "region", Cols: [][2]string{{"region_id", "id"}}, Anchor: "customer"}
	product := navItem{Table: "product"}

	st := &navState{Ctx: ctx}
	for _, tt := range []struct {
		name string
		st   navState
		item navItem
		flip bool
		want string
	}{
		{"join", navState{Ctx: ctx}, customer, false, "customer c on c.id = o.customer_id"},
		{"ctrl-t: name", navState{Ctx: ctx}, customer, true, "customer"},
		{"chain", navState{Ctx: ctx, Path: []navItem{customer}}, region, false, "customer c on c.id = o.customer_id\n  join region r on r.id = c.region_id"},
		{"unrelated table", navState{Ctx: ctx}, product, false, "product"},
		{"chain from an unrelated table", navState{Ctx: ctx, Path: []navItem{product}}, region, false, "region"},
	} {
		if got := chooseInsert(&tt.st, tt.item, tt.flip); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}

	st.Ctx.Insert = "name"
	if got := chooseInsert(st, customer, false); got != "customer" {
		t.Errorf("insert: name → %q", got)
	}
	if got := chooseInsert(st, customer, true); got != "customer c on c.id = o.customer_id" {
		t.Errorf("insert: name, ctrl-t → %q", got)
	}
	st.Ctx = control.ContextResult{InJoin: false, NeedSpace: true, Tables: ctx.Tables}
	if got := chooseInsert(st, customer, false); got != " customer" {
		t.Errorf("not after JOIN → %q", got)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"~/Library/x.sql": home + "/Library/x.sql",
		"~":               home,
		"/abs/x.sql":      "/abs/x.sql",
		"~other/x.sql":    "~other/x.sql",
		"rel/~/x":         "rel/~/x",
	} {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// sysAPI has tables only in system schemas (PostgreSQL's postgres database).
type sysAPI struct{ fakeAPI }

func (s sysAPI) tables(_ string, all bool) ([]control.TableItem, error) {
	if !all {
		return nil, nil
	}
	return []control.TableItem{{Schema: "pg_catalog", Name: "pg_class", Insert: "pg_catalog.pg_class"}}, nil
}

func TestNavLinesSystemOnly(t *testing.T) {
	lines, err := navLines(sysAPI{}, &navState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := display(lines); strings.Join(got, ",") != "pg_catalog.pg_class" {
		t.Errorf("got %v", got)
	}
}
