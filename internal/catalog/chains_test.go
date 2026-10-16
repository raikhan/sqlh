package catalog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/control"
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

	res, err := Chains(src, []string{"a"}, "", 4, 10, opt, false)
	if err != nil {
		t.Fatal(err)
	}
	targets := func(res *control.ChainsResult) string {
		var got []string
		for _, tg := range res.Targets {
			s := fmt.Sprintf("%s:%d:%d", tg.Table, tg.Joins, tg.Chains)
			if tg.More {
				s += "+"
			}
			got = append(got, s)
		}
		return strings.Join(got, " ")
	}
	// nearest first; every simple chain counted (e: two relationships with
	// b, each way, by two routes to b)
	if got := targets(res); got != "b:1:2 d:1:2 c:2:2 e:2:4 f:3:2" {
		t.Errorf("targets: %s", got)
	}
	res, _ = Chains(src, []string{"a"}, "c", 4, 10, opt, false)
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
	// longer ones too, shortest first: to b, directly and around through c
	res, _ = Chains(src, []string{"a"}, "b", 4, 10, opt, false)
	if len(res.Chains) != 2 || len(res.Chains[0].Hops) != 1 || len(res.Chains[1].Hops) != 3 {
		t.Errorf("chains to b: %+v", res.Chains)
	}
	// the limit
	res, _ = Chains(src, []string{"a"}, "e", 4, 3, opt, false)
	if len(res.Chains) != 3 || !res.More {
		t.Errorf("limit: %d %v", len(res.Chains), res.More)
	}
	res, _ = Chains(src, []string{"a"}, "", 4, 3, opt, false)
	if got := targets(res); got != "b:1:2 d:1:2 c:2:2 e:2:3+ f:3:2" {
		t.Errorf("counted to the limit: %s", got)
	}
	// the depth cap; a statement table is neither a target nor on the way
	res, _ = Chains(src, []string{"a"}, "", 1, 10, opt, false)
	if got := targets(res); got != "b:1:1 d:1:1" {
		t.Errorf("depth 1: %s", got)
	}
	res, _ = Chains(src, []string{"a", "c"}, "", 4, 10, opt, false)
	if got := targets(res); got != "b:1:2 d:1:2 f:1:1 e:2:4" {
		t.Errorf("from a and c: %s", got)
	}
}
