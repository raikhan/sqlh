package database

import (
	"strings"
	"testing"
)

func TestViewWrites(t *testing.T) {
	cases := []struct {
		w       *ViewWrites
		summary string
		insert  bool
		merge   bool
	}{
		{nil, "", true, true},
		{&ViewWrites{}, "read-only", false, false},
		{&ViewWrites{true, true, true}, "", true, true},
		{&ViewWrites{false, true, true}, "no INSERT", false, true},
		{&ViewWrites{false, true, false}, "only UPDATE", false, true},
	}
	for _, c := range cases {
		if got := c.w.Summary(); got != c.summary {
			t.Errorf("%+v: summary %q, want %q", c.w, got, c.summary)
		}
		if c.w.Allows("insert") != c.insert || c.w.Allows("MERGE") != c.merge {
			t.Errorf("%+v: allows", c.w)
		}
	}
}

func TestCreateStatement(t *testing.T) {
	cases := []struct {
		v    *ViewDef
		want string
	}{
		{&ViewDef{Schema: "s", Name: "v", Query: " SELECT a.id\n   FROM a;"}, "CREATE OR REPLACE VIEW s.v AS\n SELECT a.id\n   FROM a;\n"},
		{&ViewDef{Schema: "s", Name: "mv", Materialized: true, Query: "SELECT 1 x\n"}, "CREATE MATERIALIZED VIEW s.mv AS\nSELECT 1 x;\n"},
		{&ViewDef{Name: "v", DDL: "CREATE VIEW v(a) AS SELECT 1"}, "CREATE VIEW v(a) AS SELECT 1;\n"},
		{&ViewDef{Name: "v"}, "CREATE OR REPLACE VIEW v AS\n  -- the query is not available;\n"},
	}
	for _, c := range cases {
		if got := c.v.CreateStatement(); got != c.want {
			t.Errorf("got %q\nwant %q", got, c.want)
		}
	}
}

func TestTableDocWrites(t *testing.T) {
	views := []*ViewDef{
		{Name: "v_orders", Uses: []string{"orders"}, Query: "SELECT o.id AS order_id FROM orders o", Writes: &ViewWrites{}},
		{Name: "v_big", Uses: []string{"orders"}, Query: "SELECT o.id AS order_id FROM orders o", Writes: &ViewWrites{Insert: true, Update: true, Delete: true}},
	}
	dc := &DBCache{Relations: testGraph(views)}
	if doc := dc.TableDoc("v_orders", nil); !strings.HasPrefix(doc, "`v_orders` view (read-only)\n") {
		t.Errorf("read-only view:\n%s", doc)
	}
	if doc := dc.TableDoc("v_big", nil); !strings.HasPrefix(doc, "`v_big` view\n") {
		t.Errorf("updatable view:\n%s", doc)
	}
}
