package cmd

import (
	"testing"

	"github.com/raikhan/sqlh/internal/control"
)

func TestPanelText(t *testing.T) {
	join := &control.ContextResult{InJoin: true, Insert: "join", NeedSpace: false,
		Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}
	from := &control.ContextResult{Insert: "join", Tables: []control.ScopeTable{{Name: "orders", Alias: "o", Ref: "o"}}}
	cases := []struct {
		name string
		a    control.PanelAction
		ctx  *control.ContextResult
		want string
		copy bool
	}{
		{"name outside a join", control.PanelAction{Kind: control.ActionTable, Table: "customer"}, from, "customer", false},
		{"related table after JOIN", control.PanelAction{Kind: control.ActionTable, Table: "customer"}, join,
			"customer c ON c.id = o.customer_id", false},
		{"forced name after JOIN", control.PanelAction{Kind: control.ActionTable, Table: "customer", Mode: "name"}, join, "customer", false},
		{"unrelated after JOIN", control.PanelAction{Kind: control.ActionTable, Table: "product"}, join, "product", false},
		{"chain from the statement", control.PanelAction{Kind: control.ActionTable, Table: "region", Anchor: "orders",
			Path: []control.JoinHop{{Table: "customer", Cols: [][2]string{{"customer_id", "id"}}}, {Table: "region", Cols: [][2]string{{"region_id", "id"}}}}},
			join, "customer c ON c.id = o.customer_id\nJOIN region r ON r.id = c.region_id", false},
		{"cursor on a table name copies", control.PanelAction{Kind: control.ActionTable, Table: "customer"},
			&control.ContextResult{Focus: "orders", Tables: from.Tables}, "customer", true},
		{"columns with the alias", control.PanelAction{Kind: control.ActionColumns, Table: "orders", Columns: []string{"id", "total"}}, from, "o.id, o.total", false},
		{"columns of another table", control.PanelAction{Kind: control.ActionColumns, Table: "region", Columns: []string{"name"}}, from, "name", false},
		{"columns, single unaliased table", control.PanelAction{Kind: control.ActionColumns, Table: "orders", Columns: []string{"id"}},
			&control.ContextResult{Tables: []control.ScopeTable{{Name: "orders", Ref: "orders"}}}, "id", false},
		{"text, needs a space", control.PanelAction{Kind: control.ActionText, Text: "orders_id_seq"}, &control.ContextResult{NeedSpace: true}, " orders_id_seq", false},
	}
	for _, c := range cases {
		got, cp := panelText(api, c.a, c.ctx)
		if got != c.want || cp != c.copy {
			t.Errorf("%s: got %q (copy %v), want %q (copy %v)", c.name, got, cp, c.want, c.copy)
		}
	}
}

func TestSameTable(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"orders", "ORDERS", true}, {"sales.orders", "orders", true}, {`"Orders"`, "orders", true},
		{"a.orders", "b.orders", false}, {"orders", "order_line", false},
	} {
		if got := sameTable(c.a, c.b); got != c.want {
			t.Errorf("sameTable(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestFindAncestor(t *testing.T) {
	ps := " 1 0 launchd\n 100 1 /opt/homebrew/bin/hx\n 200 100 nu\n 300 200 sqlh\n"
	if got := findAncestor(ps, 200, func(c string) bool { return c == "hx" || len(c) > 3 && c[len(c)-3:] == "/hx" }); got != 100 {
		t.Errorf("got %d", got)
	}
	if got := findAncestor(ps, 1, func(string) bool { return false }); got != 0 {
		t.Errorf("got %d", got)
	}
}

func TestShellLine(t *testing.T) {
	if got := shellLine([]string{"/bin/sqlh", "panel", "it's"}); got != `'/bin/sqlh' 'panel' 'it'"'"'s'` {
		t.Errorf("got %s", got)
	}
}
