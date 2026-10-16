package parseutil

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/token"
)

func TestCheckSyntaxPosition(t *testing.T) {
	tests := []struct {
		name string
		text string
		pos  token.Pos
		want SyntaxPosition
	}{
		{
			name: "insert column",
			text: "insert into city (ID, Name, CountryCode) VALUES (123, 'aaa', '2020')",
			pos: token.Pos{
				Line: 0,
				Col:  57,
			},
			want: InsertValue,
		},
		{
			name: "on lparen",
			text: "insert into city (ID, Name, CountryCode) VALUES (",
			pos: token.Pos{
				Line: 0,
				Col:  49,
			},
			want: InsertValue,
		},
		{
			name: "with space first param",
			text: "insert into city (ID, Name, CountryCode) VALUES (   ",
			pos: token.Pos{
				Line: 0,
				Col:  52,
			},
			want: InsertValue,
		},
		{
			name: "second param",
			text: "insert into city (ID, Name, CountryCode) VALUES (123, ",
			pos: token.Pos{
				Line: 0,
				Col:  54,
			},
			want: InsertValue,
		},
		{
			name: "white space with second param",
			text: "insert into city (ID, Name, CountryCode) VALUES (123,   ",
			pos: token.Pos{
				Line: 0,
				Col:  56,
			},
			want: InsertValue,
		},
		{
			name: "third param",
			text: "insert into city (ID, Name, CountryCode) VALUES (123, 'aaa', '2020' ",
			pos: token.Pos{
				Line: 0,
				Col:  68,
			},
			want: InsertValue,
		},
		{
			name: "white space with third param",
			text: "insert into city (ID, Name, CountryCode) VALUES (123, 'aaa', '2020'   ",
			pos: token.Pos{
				Line: 0,
				Col:  70,
			},
			want: InsertValue,
		},
		{
			name: "join tables",
			text: "select CountryCode from city join ",
			pos: token.Pos{
				Line: 0,
				Col:  34,
			},
			want: JoinClause,
		},
		{
			name: "join filtered tables",
			text: "select CountryCode from city join co",
			pos: token.Pos{
				Line: 0,
				Col:  36,
			},
			want: JoinClause,
		},
		{
			name: "left join tables",
			text: "select CountryCode from city left join ",
			pos: token.Pos{
				Line: 0,
				Col:  39,
			},
			want: JoinClause,
		},
		{
			name: "left outer join tables",
			text: "select CountryCode from city left outer join ",
			pos: token.Pos{
				Line: 0,
				Col:  45,
			},
			want: JoinClause,
		},
		{
			name: "join on columns",
			text: "select * from city left join country on ",
			pos: token.Pos{
				Line: 0,
				Col:  40,
			},
			want: JoinOn,
		},
		{
			name: "join on filtered columns",
			text: "select * from city left join country on co",
			pos: token.Pos{
				Line: 0,
				Col:  42,
			},
			want: WhereCondition,
		},
		{
			name: "join on table<Period>",
			text: "select * from city left join country on country.",
			pos: token.Pos{
				Line: 0,
				Col:  48,
			},
			want: ColName,
		},
		{
			name: "join on <Eq>",
			text: "select * from city left join country on country.Code =",
			pos: token.Pos{
				Line: 0,
				Col:  54,
			},
			want: WhereCondition,
		},
		{
			name: "join on <Eq><WhiteSpace>",
			text: "select * from city left join country on country.Code = ",
			pos: token.Pos{
				Line: 0,
				Col:  55,
			},
			want: WhereCondition,
		},
		{
			name: "join on ref tables filtered",
			text: "select * from city left join country on country.Code = ci",
			pos: token.Pos{
				Line: 0,
				Col:  57,
			},
			want: WhereCondition,
		},
		{
			name: "join on ref table<Period>",
			text: "select * from city left join country on country.Code = city.",
			pos: token.Pos{
				Line: 0,
				Col:  60,
			},
			want: ColName,
		},
		{
			name: "join alias snippet",
			text: "select * from city c left join country c1 on c1.Code",
			pos: token.Pos{
				Line: 0,
				Col:  39,
			},
			want: TableReference,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parser.Parse(tt.text)
			if err != nil {
				t.Errorf("parse error, %v", err)
				return
			}

			nodeWalker := NewNodeWalker(parsed, tt.pos)
			if got := CheckSyntaxPosition(nodeWalker); got != tt.want {
				t.Errorf("unmatch syntax position got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyntaxPositionInParentheses(t *testing.T) {
	tests := []struct {
		input string // | marks the cursor
		want  SyntaxPosition
		start bool // AtParenStart
		alias bool // OnImplicitAlias
	}{
		// a subquery is analysed on its own, not as part of the clause around it
		{"SELECT f.id FROM (SELECT bd.id FROM batch_document bd uni|", Unknown, false, false},
		{"SELECT f.id FROM (SELECT bd.id FROM batch_document bd UNION |", Unknown, false, false},
		{"SELECT f.id FROM (SELECT bd.id FROM batch_document bd uni|) f", Unknown, false, false},
		{"SELECT f.id FROM (SELECT bd.id FROM |) f", TableReference, false, false},
		{"SELECT f.id FROM (SELECT bd.| FROM batch_document bd) f", SelectExpr, false, false},
		{"SELECT * FROM a WHERE x IN (SELECT y FROM |", TableReference, false, false},
		{"SELECT (SELECT max(x) FROM |", TableReference, false, false},
		{"WITH f AS (SELECT 1 FROM t wh|", TableReference, false, true},
		// the start of parentheses: the clause around them, a subquery may begin
		{"SELECT f.id FROM (sel|", TableReference, true, false},
		{"SELECT f.id FROM (|", TableReference, true, false},
		{"WITH f AS (sel|", AliasName, true, false},
		{"SELECT * FROM a JOIN (|", JoinClause, true, false},
		// expressions in parentheses: the clause around them
		{"SELECT * FROM batch b WHERE (b.id = 1 AND |", WhereCondition, false, false},
		{"SELECT * FROM batch b WHERE (b.|", WhereCondition, false, false},
		{"SELECT coalesce(b.|", SelectExpr, false, false},
		// only real INSERT lists are insert positions
		{"INSERT INTO batch (|", InsertColumn, true, false},
		{"INSERT INTO batch (id, |", InsertColumn, false, false},
		{"INSERT INTO batch (id) VALUES (|", InsertValue, true, false},
		{"INSERT INTO batch (id) VALUES (1), (|", InsertValue, true, false},
		// UNION ALL |: the next SELECT, not SELECT ALL |
		{"SELECT 1 FROM t UNION ALL sel|", Unknown, false, false},
		{"SELECT ALL |", SelectExpr, false, false},
		// right after ")" is outside the parentheses
		{"SELECT f.id FROM (SELECT bd.id FROM batch_document bd)|", TableReference, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			i := strings.Index(tt.input, "|")
			parsed, err := parser.Parse(strings.Replace(tt.input, "|", "", 1))
			if err != nil {
				t.Fatal(err)
			}
			nw := NewNodeWalker(parsed, token.Pos{Line: 0, Col: i})
			if got := CheckSyntaxPosition(nw); got != tt.want {
				t.Errorf("position %s, want %s", got, tt.want)
			}
			if got := AtParenStart(nw); got != tt.start {
				t.Errorf("AtParenStart %v", got)
			}
			if got := OnImplicitAlias(nw); got != tt.alias {
				t.Errorf("OnImplicitAlias %v", got)
			}
		})
	}
}
