package catalog

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
)

type graphFake struct {
	fake
	g *database.RelGraph
}

func (f graphFake) Relations() (*database.RelGraph, error) { return f.g, nil }

func pk(n, t string) *database.ColumnDesc {
	c := col(n, t)
	c.Key = "YES"
	return c
}

func TestRelations(t *testing.T) {
	cols := map[string][]*database.ColumnDesc{
		"CADE.CAPRODUCT":     {pk("PROD_ID", "NUMBER"), col("PROD_NAME", "VARCHAR2(100)")},
		"CADE.CAPRODSYNONYM": {pk("SYN_ID", "NUMBER"), col("PROD_ID", "NUMBER")},
		"CADE.DESYNONYM_500": {col("I_ID", "NUMBER"), col("SYN_ID", "NUMBER")},
		"CADE500.CAPRODUCT":  {pk("PROD_ID", "NUMBER")},
	}
	src := graphFake{fake: fake{def: "CADE",
		tables: map[string][]string{"CADE": {"CAPRODUCT", "CAPRODSYNONYM", "DESYNONYM_500"}, "CADE500": {"CAPRODUCT"}},
		cols:   cols}}
	src.g = database.BuildRelGraph(database.RelGraphInput{
		Schema: "CADE", Tables: src.tables["CADE"],
		Columns: func(t string) []*database.ColumnDesc { return cols["CADE."+t] },
		ForeignKeys: []*database.ForeignKey{{{
			{Table: "CAPRODSYNONYM", Name: "PROD_ID"}, {Table: "CAPRODUCT", Name: "PROD_ID"}}}},
		Views: []*database.ViewDef{{Name: "DESYNONYM_500", Uses: []string{"CAPRODSYNONYM"},
			Query: "SELECT prod_id AS i_id, syn_id FROM caprodsynonym WHERE is_private = 'N'"}},
	})
	opt := Options{Driver: dialect.DatabaseDriverOracle, Lower: true}

	res, err := Relations(src, "caprodsynonym", opt, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range res.Relations {
		got = append(got, r.Marker+" "+r.Insert+" "+r.Edge+" "+r.Condition+" ("+r.Note+")")
	}
	want := []string{
		"→ caproduct fk caproduct.prod_id = prod_id ()",
		// the view exposes the table's key: joinable from the table
		"◇ desynonym_500 uses desynonym_500.syn_id = syn_id (view reads this)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || res.Kind != "table" || res.Insert != "caprodsynonym" {
		t.Errorf("caprodsynonym (%s %s):\n%s", res.Kind, res.Insert, strings.Join(got, "\n"))
	}

	res, err = Relations(src, "DESYNONYM_500", opt, true)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, r := range res.Relations {
		got = append(got, r.Marker+" "+r.Insert+" "+r.Edge+" "+r.Condition+" ("+r.Note+")")
	}
	want = []string{
		"→ caprodsynonym view caprodsynonym.syn_id = syn_id (via caprodsynonym)",
		"→ caproduct view caproduct.prod_id = i_id (via caprodsynonym)",
		"◇ caprodsynonym uses  (reads)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || res.Kind != "view" || !strings.HasPrefix(res.Definition, "SELECT prod_id") {
		t.Errorf("desynonym_500 (%s):\n%s", res.Kind, strings.Join(got, "\n"))
	}

	if res, err := Relations(src, "cade500.caproduct", opt, true); err != nil || len(res.Relations) != 0 || res.Note == "" {
		t.Errorf("other schema: %+v %v", res, err)
	}
	if _, err := Relations(src, "nope", opt, true); err == nil {
		t.Error("unknown table: no error")
	}
}

func TestAlias(t *testing.T) {
	taken := map[string]bool{"o": true, "c": true}
	for in, want := range map[string]string{
		"order_line": "ol", "customer": "c2", "orders": "o2", "region": "r", "cade.caproduct": "c2",
		`"Mixed Case"`: "mc", "_x": "x", "123": "t", "as_of": "ao", "o_n": "on2",
	} {
		if got := Alias(in, taken); got != want {
			t.Errorf("Alias(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComposeJoin(t *testing.T) {
	steps := []JoinStep{
		{Insert: "customer", Cols: [][2]string{{"customer_id", "id"}}, AnchorRef: "o"},
		{Insert: "region", Cols: [][2]string{{"region_id", "id"}}},
		{Insert: "v_report"}, // no condition known
	}
	got := ComposeJoin(steps, map[string]bool{"o": true, "orders": true}, true, "  ")
	want := "customer c on c.id = o.customer_id\n  join region r on r.id = c.region_id\n  join v_report vr"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	got = ComposeJoin([]JoinStep{{Insert: "order_line", Cols: [][2]string{{"id", "order_id"}, {"v", "w"}}, AnchorRef: "orders"}}, nil, false, "")
	if want := "order_line ol ON ol.order_id = orders.id AND ol.w = orders.v"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
