package parseutil

import "testing"

func TestExtractQueryLineage(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		cols     string
		tables   string
		compound bool
	}{
		{
			name: "PostgreSQL view definition",
			query: ` SELECT o.id AS order_id,
    o.customer_id,
    c.name AS customer_name
   FROM (orders o
     JOIN customer c ON ((c.id = o.customer_id)));`,
			cols:   "order_id<-orders.id,customer_id<-orders.customer_id,customer_name<-customer.name",
			tables: "orders o, customer c",
		},
		{
			name:  "unqualified columns of a single table, expressions",
			query: "SELECT prod_id AS i_id, syn_id, 'Y' syn_isproduct, upper(syn_name) AS n FROM caprodsynonym WHERE is_private = 'N'",
			// expressions have no origin (the select-list parser may also drop them)
			cols:   "i_id<-caprodsynonym.prod_id,syn_id<-caprodsynonym.syn_id",
			tables: "caprodsynonym",
		},
		{
			name:   "unqualified columns of two tables are unknown",
			query:  "select a_col, b_col from a, b where a.id = b.id",
			cols:   "a_col,b_col",
			tables: "a, b",
		},
		{
			name:   "star",
			query:  "select * from orders where total > 100",
			cols:   "*<-orders",
			tables: "orders",
		},
		{
			name:     "union: compound, tables of every branch",
			query:    "select id from a union all select id from b",
			cols:     "id{a.id|b.id}",
			tables:   "a, b",
			compound: true,
		},
		{
			name:     "union of the same table column keeps the origin",
			query:    "select id, name from a where k = 1 union all select id, upper(name) from a where k = 2",
			cols:     "id<-a.id,name",
			tables:   "a",
			compound: true,
		},
		{
			name:     "Oracle MINUS",
			query:    "select id from a minus select id from b",
			cols:     "id<-a.id",
			tables:   "a, b",
			compound: true,
		},
		{
			name:   "UNION in a derived table",
			query:  "select u.id from (select id from a union all select id from b) u",
			cols:   "id{a.id|b.id}",
			tables: "a, b",
		},
		{
			name:   "CTEs and subqueries: base tables only",
			query:  "with x as (select id from a) select x.id, (select max(v) from c) m from x join (select id from b) y on y.id = x.id",
			cols:   "id<-a.id,m",
			tables: "a, c, b",
		},
		{
			name:   "schema-qualified",
			query:  "select t.id from cade.orders t",
			cols:   "id<-orders.id",
			tables: "cade.orders t",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lin := ExtractQueryLineage(initExtractTable(t, tt.query))
			if got := renderCols(lin.Columns); got != tt.cols {
				t.Errorf("columns:\n got %q\nwant %q", got, tt.cols)
			}
			if got := renderTables(lin.Tables); got != tt.tables {
				t.Errorf("tables:\n got %q\nwant %q", got, tt.tables)
			}
			if lin.Compound != tt.compound {
				t.Errorf("compound = %v", lin.Compound)
			}
		})
	}
}
