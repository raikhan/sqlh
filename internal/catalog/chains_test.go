package catalog

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
)

func TestChains(t *testing.T) {
	tables := []string{"a", "b", "c", "d", "e", "f"}
	cols := map[string][]*database.ColumnDesc{
		"s.a": {pk("id", "integer")},
		"s.b": {pk("id", "integer"), col("a_id", "integer"), col("e_id", "integer")},
		"s.c": {pk("id", "integer"), col("b_id", "integer"), col("d_id", "integer")},
		"s.d": {pk("id", "integer"), col("a_id", "integer")},
		"s.e": {pk("id", "integer"), col("b_id", "integer")},
		"s.f": {pk("id", "integer"), col("c_id", "integer")},
	}
	fk := func(from, fcol, to string) *database.ForeignKey {
		return &database.ForeignKey{{{Table: from, Name: fcol}, {Table: to, Name: "id"}}}
	}
	src := graphFake{fake: fake{def: "s", tables: map[string][]string{"s": tables}, cols: cols}}
	src.g = database.BuildRelGraph(database.RelGraphInput{
		Schema: "s", Tables: tables,
		Columns: func(t string) []*database.ColumnDesc { return cols["s."+t] },
		ForeignKeys: []*database.ForeignKey{fk("b", "a_id", "a"), fk("c", "b_id", "b"), fk("d", "a_id", "a"),
			fk("c", "d_id", "d"), fk("e", "b_id", "b"), fk("b", "e_id", "e"), fk("f", "c_id", "c")},
	})
	opt := Options{Driver: dialect.DatabaseDriverPostgreSQL, Lower: true}

	res, err := Chains(src, []string{"a"}, "", 4, 5, opt, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tg := range res.Targets {
		got = append(got, tg.Table+":"+string(rune('0'+tg.Joins))+":"+string(rune('0'+tg.Chains))+":"+strings.Join(tg.Via, ">"))
	}
	// nearest first; c by two shortest chains; e by two relationships with b (each way)
	if strings.Join(got, " ") != "b:1:1:a>b d:1:1:a>d c:2:2:a>b>c e:2:2:a>b>e f:3:2:a>b>c>f" {
		t.Errorf("targets: %s", strings.Join(got, " "))
	}
	res, _ = Chains(src, []string{"a"}, "c", 4, 5, opt, false)
	var chains []string
	for _, c := range res.Chains {
		s := c.Anchor
		for _, h := range c.Hops {
			s += ">" + h.Table + "(" + h.Cols[0][0] + "=" + h.Cols[0][1] + ")"
		}
		chains = append(chains, s)
	}
	if strings.Join(chains, " ") != "a>b(id=a_id)>c(id=b_id) a>d(id=a_id)>c(id=d_id)" {
		t.Errorf("chains to c: %v", chains)
	}
	// the depth cap; a statement table is neither a target nor on the way
	res, _ = Chains(src, []string{"a"}, "", 1, 5, opt, false)
	if len(res.Targets) != 2 {
		t.Errorf("depth 1: %+v", res.Targets)
	}
	res, _ = Chains(src, []string{"a", "c"}, "", 4, 5, opt, false)
	got = nil
	for _, tg := range res.Targets {
		got = append(got, tg.Table+":"+strings.Join(tg.Via, ">"))
	}
	if strings.Join(got, " ") != "b:a>b d:a>d f:c>f e:a>b>e" {
		t.Errorf("from a and c: %s", strings.Join(got, " "))
	}
}
