package completer

import (
	"strings"
	"testing"
)

func TestCursorContext(t *testing.T) {
	tests := []struct {
		name      string
		text      string // | marks the cursor
		inJoin    bool
		tables    string // name alias, …
		focus     string
		needSpace bool
		indent    string
	}{
		{name: "after JOIN", text: "select *\n  from orders o\n  join |\n where o.total > 1;", inJoin: true, tables: "orders o", indent: "  "},
		{name: "after LEFT JOIN, two tables before", text: "select * from orders o join customer c on c.id = o.customer_id left join |", inJoin: true, tables: "orders o, customer c"},
		{name: "JOIN without a space", text: "select * from orders o join|", inJoin: true, tables: "orders o", needSpace: true},
		{name: "JOIN on the previous line", text: "select * from orders o join\n|", inJoin: true, tables: "orders o"},
		{name: "only tables before the cursor", text: "select * from orders o join | join region r on true", inJoin: true, tables: "orders o"},
		{name: "select list: all tables, not a join", text: "select | from orders o join customer c on c.id = o.customer_id", tables: "orders o, customer c"},
		{name: "cursor on an alias", text: "select o.id from orders o join customer c on c.id = |o.customer_id", tables: "orders o, customer c", focus: "orders", needSpace: false},
		{name: "cursor on a table name", text: "select 1 from orders o join cust|omer c on true", tables: "orders o, customer c", focus: "customer", needSpace: true},
		{name: "second statement", text: "select * from a;\nselect * from b x join |", inJoin: true, tables: "b x"},
		{name: "no statement", text: "|", tables: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := strings.Index(tt.text, "|")
			before := tt.text[:i]
			line := strings.Count(before, "\n")
			char := len(before) - (strings.LastIndex(before, "\n") + 1)
			sc := CursorContext(tt.text[:i]+tt.text[i+1:], line, char)
			var tables []string
			for _, tb := range sc.Tables {
				s := tb.Name
				if tb.Alias != "" {
					s += " " + tb.Alias
				}
				tables = append(tables, s)
			}
			focus := ""
			if sc.Focus != nil {
				focus = sc.Focus.Name
			}
			if sc.InJoin != tt.inJoin || strings.Join(tables, ", ") != tt.tables || focus != tt.focus ||
				sc.NeedSpace != tt.needSpace || sc.Indent != tt.indent {
				t.Errorf("got inJoin=%v tables=%q focus=%q needSpace=%v indent=%q", sc.InJoin, strings.Join(tables, ", "), focus, sc.NeedSpace, sc.Indent)
			}
		})
	}
}
