package database

import (
	"sort"
	"strings"
	"testing"
)

func tcol(table, name, typ, null, key string) *ColumnDesc {
	return &ColumnDesc{ColumnBase: ColumnBase{Schema: "s", Table: table, Name: name}, Type: typ, Null: null, Key: key}
}

func fkey(from, fromCol, to, toCol string) *ForeignKey {
	return &ForeignKey{{
		{Schema: "s", Table: from, Name: fromCol},
		{Schema: "s", Table: to, Name: toCol},
	}}
}

// testGraph: region ← customer ← orders → (maybe) employee; order_line → orders;
// views over them.
func testGraph(views []*ViewDef) *RelGraph {
	cols := map[string][]*ColumnDesc{
		"region":   {tcol("region", "id", "integer", "NO", "YES"), tcol("region", "name", "text", "YES", "NO")},
		"customer": {tcol("customer", "id", "integer", "NO", "YES"), tcol("customer", "name", "text", "YES", "NO"), tcol("customer", "region_id", "integer", "YES", "NO")},
		"employee": {tcol("employee", "employee_id", "integer", "NO", "YES"), tcol("employee", "name", "text", "YES", "NO")},
		"product":  {tcol("product", "id", "integer", "NO", "YES"), tcol("product", "code", "text", "NO", "NO")},
		"orders": {tcol("orders", "id", "integer", "NO", "YES"), tcol("orders", "customer_id", "integer", "NO", "NO"),
			tcol("orders", "employee_id", "integer", "YES", "NO"), tcol("orders", "total", "numeric", "YES", "NO")},
		"order_line": {tcol("order_line", "order_id", "integer", "NO", "YES"), tcol("order_line", "line_no", "integer", "NO", "YES"),
			tcol("order_line", "product_id", "text", "YES", "NO")}, // type differs from product.id: no guess
		"customer_detail": {tcol("customer_detail", "customer_id", "integer", "NO", "YES")},
		"v_orders": {tcol("v_orders", "order_id", "integer", "YES", "NO"), tcol("v_orders", "customer_id", "integer", "YES", "NO"),
			tcol("v_orders", "region_id", "integer", "YES", "NO"), tcol("v_orders", "total", "numeric", "YES", "NO")},
		"v_big": {tcol("v_big", "order_id", "integer", "YES", "NO"), tcol("v_big", "customer_id", "integer", "YES", "NO"),
			tcol("v_big", "region_id", "integer", "YES", "NO"), tcol("v_big", "total", "numeric", "YES", "NO")},
		"v_union":  {tcol("v_union", "id", "integer", "YES", "NO"), tcol("v_union", "customer_id", "integer", "YES", "NO")},
		"v_dunion": {tcol("v_dunion", "oid", "integer", "YES", "NO"), tcol("v_dunion", "cid", "integer", "YES", "NO")},
	}
	var tables []string
	for t := range cols {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	return BuildRelGraph(RelGraphInput{
		Schema:  "s",
		Tables:  tables,
		Columns: func(t string) []*ColumnDesc { return cols[t] },
		ForeignKeys: []*ForeignKey{
			fkey("customer", "region_id", "region", "id"),
			fkey("orders", "customer_id", "customer", "id"),
			fkey("order_line", "order_id", "orders", "id"),
			fkey("customer_detail", "customer_id", "customer", "id"),
			fkey("orders", "customer_id", "elsewhere", "id"), // not in the schema: dropped
		},
		Views: views,
	})
}

var testViews = []*ViewDef{
	{Name: "v_orders", Uses: []string{"orders", "customer"}, Query: ` SELECT o.id AS order_id,
    o.customer_id,
    c.region_id,
    o.total
   FROM (orders o
     JOIN customer c ON ((c.id = o.customer_id)));`},
	{Name: "v_big", Query: "select * from v_orders where total > 100"}, // dependencies from the query
	{Name: "v_union", Query: "select id, customer_id from orders union all select id, region_id from customer"},
	// UNION in a derived table: the first branch's columns are not the origin
	{Name: "v_dunion", Query: "select u.id as oid, u.customer_id as cid from (select id, customer_id from orders union all select id, region_id from customer) u"},
}

// render lists the edges of a relation: "→ other kind cols [notes]".
func render(g *RelGraph, name string) []string {
	var out []string
	for _, r := range g.Related(name, true) {
		s := r.Marker() + " " + r.Other + " " + string(r.Edge.Kind)
		var cs []string
		for _, c := range r.Cols {
			cs = append(cs, c.From+"="+c.To)
		}
		if len(cs) > 0 {
			s += " " + strings.Join(cs, ",")
		}
		if n := r.Note(); n != "" && r.Edge.Kind != EdgeUses {
			s += " [" + n + "]"
		}
		out = append(out, s)
	}
	return out
}

func check(t *testing.T, g *RelGraph, name string, want ...string) {
	t.Helper()
	got := render(g, name)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got:\n  %s\nwant:\n  %s", name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestRelGraphTables(t *testing.T) {
	g := testGraph(nil)
	if g.ViewsLoaded {
		t.Error("ViewsLoaded without views")
	}
	// until views are loaded they look like tables: their columns get guesses
	check(t, g, "orders",
		"→ customer fk customer_id=id",
		"← order_line fk id=order_id",
		"→ employee maybe employee_id=employee_id [maybe, nullable]",
		"← v_big maybe id=order_id [maybe]",
		"← v_orders maybe id=order_id [maybe]")
	check(t, g, "customer",
		"← customer_detail fk id=customer_id [1:1]",
		"← orders fk id=customer_id",
		"→ region fk region_id=id [nullable]",
		"← v_big maybe id=customer_id [maybe]",
		"← v_orders maybe id=customer_id [maybe]",
		"← v_union maybe id=customer_id [maybe]")
	// order_line.product_id is text, product.id integer: no guess
	check(t, g, "product")
	if k := g.Kind("V_ORDERS"); k != RelTable {
		t.Errorf("kind before views are loaded = %s", k)
	}
}

func TestRelGraphViews(t *testing.T) {
	g := testGraph(testViews)
	if !g.ViewsLoaded || g.Kind("v_orders") != RelView {
		t.Fatal("views not loaded")
	}
	check(t, g, "v_orders",
		"→ customer view customer_id=id [via orders]",
		"← order_line view order_id=order_id [via orders]",
		"→ orders view order_id=id [via orders]",
		"→ region view region_id=id [via customer, nullable]",
		"◇ customer uses",
		"◇ orders uses",
		"◇ v_big uses")
	// view on a view: resolved down to the base tables
	check(t, g, "v_big",
		"→ customer view customer_id=id [via orders]",
		"← order_line view order_id=order_id [via orders]",
		"→ orders view order_id=id [via orders]",
		"→ region view region_id=id [via customer, nullable]",
		"◇ v_orders uses")
	// UNION: no lineage and no guesses
	check(t, g, "v_union", "◇ customer uses", "◇ orders uses")
	check(t, g, "v_dunion", "◇ customer uses", "◇ orders uses")
	// view edges are listed for the view only; the table lists the views reading it
	check(t, g, "orders",
		"→ customer fk customer_id=id",
		"← order_line fk id=order_id",
		"→ employee maybe employee_id=employee_id [maybe, nullable]",
		"◇ v_dunion uses",
		"◇ v_orders uses",
		"◇ v_union uses")
	if cols := g.JoinVia("orders", "v_orders"); len(cols) != 1 || cols[0] != (ColPair{From: "id", To: "order_id"}) {
		t.Errorf("JoinVia = %v", cols)
	}
	if cols := g.JoinVia("customer", "v_orders"); cols != nil {
		t.Errorf("JoinVia(customer) = %v (the view doesn't expose customer.id)", cols)
	}
}

func TestRelGraphJoinKeys(t *testing.T) {
	g := testGraph(testViews)
	keys := func(m map[string]map[string][]*ForeignKey, t string) []string {
		var out []string
		for o, fks := range m[t] {
			for range fks {
				out = append(out, o)
			}
		}
		sort.Strings(out)
		return out
	}
	if got := strings.Join(keys(g.JoinKeys(true), "orders"), ","); got != "customer,employee,order_line" {
		t.Errorf("orders join keys: %s", got)
	}
	if got := strings.Join(keys(g.JoinKeys(false), "orders"), ","); got != "customer,order_line" {
		t.Errorf("orders join keys without guesses: %s", got)
	}
	if got := strings.Join(keys(g.JoinKeys(false), "v_orders"), ","); got != "customer,order_line,orders,region" {
		t.Errorf("v_orders join keys: %s", got)
	}
	// declared foreign keys keep their identity (join completion output unchanged)
	for _, fks := range g.JoinKeys(true)["customer"] {
		for _, fk := range fks {
			if e := g.EdgeOf(fk); e == nil {
				t.Errorf("no edge for %v", fk)
			}
		}
	}
}

func TestStemsAndTypes(t *testing.T) {
	if got := strings.Join(stems("categories"), ","); got != "categories,category" {
		t.Error(got)
	}
	if got := strings.Join(stems("addresses"), ","); got != "addresses,address" {
		t.Error(got)
	}
	if got := strings.Join(stems("status"), ","); got != "status,statu" {
		t.Error(got) // harmless: no column is called statu_id
	}
	if !sameTypeFamily("NUMBER(10)", "integer") || sameTypeFamily("VARCHAR2(10)", "NUMBER") || !sameTypeFamily("", "text") {
		t.Error("type families")
	}
}
