package parseutil

import (
	"fmt"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/token"
)

// at splits "…|…" into the text without the marker and the marker position.
func at(t *testing.T, s string) (string, token.Pos) {
	t.Helper()
	i := strings.Index(s, "|")
	if i < 0 {
		t.Fatalf("no cursor marker in %q", s)
	}
	before := s[:i]
	line := strings.Count(before, "\n")
	col := len(before) - (strings.LastIndex(before, "\n") + 1)
	return s[:i] + s[i+1:], token.Pos{Line: line, Col: col}
}

// renderCols: "name" or "name<-table.col" ("*<-table" for star columns).
func renderCols(cols []*SubQueryColumn) string {
	var parts []string
	for _, c := range cols {
		s := c.DisplayName()
		if c.NameOnly {
			s += "~" + c.ParentTable.Name // names only
		} else if c.ParentTable != nil {
			if c.ColumnName == "*" {
				s += "<-" + c.ParentTable.Name
			} else {
				s += "<-" + c.ParentTable.Name + "." + c.ColumnName
			}
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

func renderRels(infos []*SubQueryInfo) string {
	var parts []string
	for _, info := range infos {
		kind := "sub"
		if info.Kind == SubQueryCTE {
			kind = "cte"
			if info.CTE != info.Name {
				kind += ":" + info.CTE
			}
		}
		if info.Outer {
			kind += ",outer"
		}
		var cols []*SubQueryColumn
		for _, v := range info.Views {
			cols = append(cols, v.SubQueryColumns...)
		}
		parts = append(parts, fmt.Sprintf("%s(%s)[%s]", info.Name, kind, renderCols(cols)))
	}
	return strings.Join(parts, " ")
}

func renderTables(tables []*TableInfo) string {
	var parts []string
	for _, t := range tables {
		s := t.Name
		if t.DatabaseSchema != "" {
			s = t.DatabaseSchema + "." + s
		}
		if t.Alias != "" {
			s += " " + t.Alias
		}
		if t.Outer {
			s += " (outer)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func TestScope(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		tables string
		rels   string
	}{
		{
			name:   "plain tables",
			input:  "select | from a x join s.b on x.id = b.id",
			tables: "a x, s.b",
		},
		{
			name:  "derived table, aliased columns",
			input: "select t.| from (select ci.ID as city_id, ci.Name from world.city as ci) as t",
			rels:  "t(sub)[city_id<-city.ID,Name<-city.Name]",
		},
		{
			name:  "derived table: function, expression, cast and distinct columns",
			input: "select s.| from (select distinct count(*) as n, id + 1 nid, x::text as t, max(y) from batch) s",
			rels:  "s(sub)[n,nid,t]",
		},
		{
			name:  "derived star expands later against the cache",
			input: "select | from (select * from city) t",
			rels:  "t(sub)[*<-city]",
		},
		{
			name:  "nested derived keeps the origin column",
			input: "select | from (select it.id as cid from (select ci.id from city ci) it) ot",
			rels:  "ot(sub)[cid<-city.id]",
		},
		{
			name:  "two derived tables",
			input: "select | from (select id from a) x join (select name from b) y on true",
			rels:  "x(sub)[id<-a.id] y(sub)[name<-b.name]",
		},
		{
			name:  "inner FROM does not leak out",
			input: "select | from (select id from a) x where exists (select 1 from c)",
			rels:  "x(sub)[id<-a.id]",
		},
		{
			name:  "derived UNION: origin only where every branch agrees",
			input: "select | from (select a.id, a.name, 1 as n from a union all select a.id, b.name, 2 from a join b on true) u",
			rels:  "u(sub)[id<-a.id,name,n]",
		},
		{
			name:  "derived UNION: matched by position, not by name",
			input: "select | from (select id, code from a union select code, id from a) u",
			rels:  "u(sub)[id,code]",
		},
		{
			name:  "derived UNION: an unnamed expression keeps its position",
			input: "select | from (select a.x as c1, a.y as c2 from a union select upper(z), a.x from a) u",
			rels:  "u(sub)[c1,c2]",
		},
		{
			name:  "derived UNION: stars of the same table line up",
			input: "select | from (select *, a.id from a where k = 1 union select *, a.id from a where k = 2) u",
			rels:  "u(sub)[*<-a,id<-a.id]",
		},
		{
			name:  "derived UNION: stars of different tables give names only, later positions unknown",
			input: "select | from (select *, a.id from a union select *, b.id from b) u",
			rels:  "u(sub)[*~a,id]",
		},
		{
			name:  "derived EXCEPT/MINUS: the subtracted branch adds no rows",
			input: "select | from (select id from a except select id from b) u join (select id from a minus select id from b) v on true",
			rels:  "u(sub)[id<-a.id] v(sub)[id<-a.id]",
		},
		{
			name:  "derived INTERSECT: every branch must agree",
			input: "select | from (select id from a intersect select id from b) u",
			rels:  "u(sub)[id]",
		},
		{
			name:  "parenthesised branches",
			input: "select | from ((select id from a) union (select id from a where k = 1)) u",
			rels:  "u(sub)[id<-a.id]",
		},
		{
			name:  "CTE with UNION",
			input: "with x as (select id from a union select id from b) select x.| from x",
			rels:  "x(cte)[id]",
		},
		{
			name:  "CTE column list: positions, unnamed expressions included",
			input: "with x (c1, c2) as (select upper(n), t.id from t) select x.| from x",
			rels:  "x(cte)[c1,c2<-t.id]",
		},
		{
			name:  "CTE column list: positions unknown after a star",
			input: "with x (c1, c2, c3) as (select a.id, *, a.id from a join b on true) select x.| from x",
			rels:  "x(cte)[c1<-a.id,c2,c3]",
		},
		{
			name:   "cursor in a MINUS branch sees its own tables",
			input:  "select id from a minus select | from b",
			tables: "b",
		},
		{
			name:   "cursor inside a subquery sees its tables and the outer ones",
			input:  "select * from batch b where exists (select 1 from doc d where d.batch_id = b.|)",
			tables: "doc d, batch b (outer)",
		},
		{
			name:  "CTE reference",
			input: "with x as (select id, name from batch) select x.| from x",
			rels:  "x(cte)[id<-batch.id,name<-batch.name]",
		},
		{
			name:  "CTE reference with alias",
			input: "with x as (select id from batch) select | from x xx",
			rels:  "xx(cte:x)[id<-batch.id]",
		},
		{
			name:  "CTE column list",
			input: "with x (a, b) as (select id, name from batch) select | from x",
			rels:  "x(cte)[a<-batch.id,b<-batch.name]",
		},
		{
			name:  "CTE column list without space, recursive, materialized",
			input: "with recursive t(n) as materialized (select 1 union all select n + 1 from t) select | from t",
			rels:  "t(cte)[n]",
		},
		{
			name:  "CTE referring to an earlier CTE",
			input: "with x as (select id from batch), y as (select x.id as yid from x) select | from y",
			rels:  "y(cte)[yid<-batch.id]",
		},
		{
			name:  "CTE star of a CTE",
			input: "with x as (select id, name from batch), y as (select * from x) select | from y",
			rels:  "y(cte)[id<-batch.id,name<-batch.name]",
		},
		{
			name:   "CTE joined with a table",
			input:  "with x as (select id as bid from batch) select * from doc d join x on x.|",
			tables: "doc d",
			rels:   "x(cte)[bid<-batch.id]",
		},
		{
			name:  "CTE body tables do not leak into the main query",
			input: "with x as (select id from batch) select | from x",
			rels:  "x(cte)[id<-batch.id]",
		},
		{
			name:   "cursor in a CTE body sees its own FROM and earlier CTEs",
			input:  "with x as (select id from batch), y as (select | from x join doc d on true) select * from y",
			tables: "doc d",
			rels:   "x(cte)[id<-batch.id]",
		},
		{
			name:  "multi-line CTE",
			input: "with x as (\n  select id, name\n  from batch\n)\nselect x.|\nfrom x",
			rels:  "x(cte)[id<-batch.id,name<-batch.name]",
		},
		{
			name:   "union: first branch",
			input:  "select | from a union all select * from b",
			tables: "a",
		},
		{
			name:   "union: second branch",
			input:  "select * from a union select | from b",
			tables: "b",
		},
		{
			name:   "extract(… from …) is not a table",
			input:  "select extract(year from added_on) |, x from batch",
			tables: "batch",
		},
		{
			name:   "table function is not a table",
			input:  "select | from generate_series(1, 3) g, batch",
			tables: "batch",
		},
		{
			name:   "lateral",
			input:  "select | from batch b join lateral (select id from doc d where d.b = b.id) l on true",
			tables: "batch b",
			rels:   "l(sub)[id<-doc.id]",
		},
		{
			name:   "update and delete",
			input:  "update batch set name = | where id in (select batch_id from doc)",
			tables: "batch",
		},
		{
			name:   "full outer join",
			input:  "select | from a full outer join b on a.id = b.id",
			tables: "a, b",
		},
		{
			name:   "parenthesised join (PostgreSQL view definitions)",
			input:  "select | from (orders o join customer c on ((c.id = o.customer_id)))",
			tables: "orders o, customer c",
		},
		{
			name:   "nested parenthesised joins",
			input:  "select | from ((a join b on a.id = b.a_id) left join c on c.id = b.c_id) join d on true",
			tables: "a, b, c, d",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, pos := at(t, tt.input)
			parsed := initExtractTable(t, text)
			tables, err := ExtractTable(parsed, pos)
			if err != nil {
				t.Fatal(err)
			}
			if got := renderTables(tables); got != tt.tables {
				t.Errorf("tables:\n got %q\nwant %q", got, tt.tables)
			}
			rels, err := ExtractSubQueryViews(parsed, pos)
			if err != nil {
				t.Fatal(err)
			}
			if got := renderRels(rels); got != tt.rels {
				t.Errorf("relations:\n got %q\nwant %q", got, tt.rels)
			}
		})
	}
}

func TestExtractCTEs(t *testing.T) {
	text, pos := at(t, "with x as (select id from batch), y (a) as (select name from doc) select * from |")
	ctes, err := ExtractCTEs(initExtractTable(t, text), pos)
	if err != nil {
		t.Fatal(err)
	}
	want := "x(cte)[id<-batch.id] y(cte)[a<-doc.name]"
	if got := renderRels(ctes); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractLastTableScoped(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// the CTE body's join is another level
		{"with x as (select * from a join b on a.id = b.id) select * from x join c|", "c"},
		{"select * from a join b on a.id = b.id join c|", "c"},
		{"select * from a join b on a.id = b.id join |", ""},
	}
	for _, tt := range tests {
		text, pos := at(t, tt.input)
		got, err := ExtractLastTable(initExtractTable(t, text), pos)
		if err != nil {
			t.Fatal(err)
		}
		name := ""
		if got != nil {
			name = got.Name
		}
		if name != tt.want {
			t.Errorf("%q: got %q, want %q", tt.input, name, tt.want)
		}
	}
}
