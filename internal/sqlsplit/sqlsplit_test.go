package sqlsplit

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func sqls(st []Statement) []string {
	out := []string{}
	for _, s := range st {
		out = append(out, s.SQL)
	}
	return out
}

func TestSplit(t *testing.T) {
	tests := []struct {
		name string
		d    Dialect
		in   string
		want []string
	}{
		{"basic", Generic, "select 1;\nselect 2;", []string{"select 1", "select 2"}},
		{"unterminated last", Generic, "select 1;\nselect 2", []string{"select 1", "select 2"}},
		{"leading comments kept out", Generic, "-- c1\nselect 1; -- trailing\n/* b */ select 2;", []string{"select 1", "select 2"}},
		{"semicolon in string", Generic, "select ';' from t; select 'a''b;' from t;", []string{"select ';' from t", "select 'a''b;' from t"}},
		{"semicolon in comment", Generic, "select 1 -- not; end\nfrom t;", []string{"select 1 -- not; end\nfrom t"}},
		{"semicolon in quoted ident", Generic, `select "a;b" from t;`, []string{`select "a;b" from t`}},
		{"empty statements", Generic, ";;select 1;;", []string{"select 1"}},
		{
			"oracle plsql block with slash", Oracle,
			"select 1 from dual;\nbegin\n  dbms_output.put_line('x');\n  null;\nend;\n/\nselect 2 from dual;\n",
			[]string{"select 1 from dual", "begin\n  dbms_output.put_line('x');\n  null;\nend;", "select 2 from dual"},
		},
		{
			"oracle create or replace procedure", Oracle,
			"create or replace procedure p as\nbegin\n  null;\nend;\n/\n",
			[]string{"create or replace procedure p as\nbegin\n  null;\nend;"},
		},
		{"oracle plsql unterminated", Oracle, "declare x number;\nbegin x := 1; end;", []string{"declare x number;\nbegin x := 1; end;"}},
		{"oracle slash terminates sql", Oracle, "select 1 from dual\n/\nselect 2 from dual", []string{"select 1 from dual", "select 2 from dual"}},
		{"oracle division is not slash line", Oracle, "select 4\n/ 2 from dual;", []string{"select 4\n/ 2 from dual"}},
		{"oracle q-quote", Oracle, "select q'[it's; fine]' from dual; select 2 from dual;", []string{"select q'[it's; fine]' from dual", "select 2 from dual"}},
		{"oracle create table is sql", Oracle, "create table t (a number);\ninsert into t values (1);", []string{"create table t (a number)", "insert into t values (1)"}},
		{
			"pg dollar quoting", PostgreSQL,
			"create function f() returns int as $$ begin return 1; end; $$ language plpgsql;\nselect $1;",
			[]string{"create function f() returns int as $$ begin return 1; end; $$ language plpgsql", "select $1"},
		},
		{"pg tagged dollar quote", PostgreSQL, "do $body$ begin perform 1; end $body$; select 2;", []string{"do $body$ begin perform 1; end $body$", "select 2"}},
		{
			"sqlite trigger", SQLite,
			"create trigger tr after insert on t begin update t set a = 1; insert into u values (1); end;\nselect 1;",
			[]string{"create trigger tr after insert on t begin update t set a = 1; insert into u values (1); end", "select 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqls(Split(tt.in, tt.d))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestPositions(t *testing.T) {
	text := "-- header\nselect 1;\n\nselect\n  2;"
	st := Split(text, Generic)
	if len(st) != 2 {
		t.Fatalf("got %d statements", len(st))
	}
	if st[0].Start != (Position{1, 0}) || st[0].End != (Position{1, 9}) {
		t.Errorf("stmt0 span %v..%v", st[0].Start, st[0].End)
	}
	if st[1].Start != (Position{3, 0}) || st[1].End != (Position{4, 4}) {
		t.Errorf("stmt1 span %v..%v", st[1].Start, st[1].End)
	}
	if st[0].PLSQL {
		t.Error("unexpected PLSQL")
	}
}

func TestAtCursor(t *testing.T) {
	text := "select 1;\n\n-- about two\nselect 2;\nselect 3; \n\n\nselect 4;"
	//       line0      1  2            3          4          5 6 7
	st := Split(text, Generic)
	cases := []struct {
		cur  Position
		want string
	}{
		{Position{0, 3}, "select 1"},
		{Position{0, 9}, "select 1"},  // right after ';'
		{Position{1, 0}, "select 1"},  // blank line after stmt 1
		{Position{2, 3}, "select 2"},  // comment attached to stmt 2
		{Position{3, 0}, "select 2"},  //
		{Position{4, 10}, "select 3"}, // trailing space on the same line
		{Position{6, 0}, "select 3"},  // blank lines: previous
		{Position{7, 2}, "select 4"},
	}
	for _, c := range cases {
		got, ok := AtCursor(text, st, c.cur)
		if !ok || got.SQL != c.want {
			t.Errorf("cursor %v: got %q want %q", c.cur, got.SQL, c.want)
		}
	}
}

func TestInLines(t *testing.T) {
	text := "select 1;\n\n-- about two\nselect 2; select 3;\nselect\n  4;\n\n\nselect 5;"
	//       line0      1  2            3                   4      5    6 7 8
	st := Split(text, Generic)
	cases := []struct {
		from, to int
		want     string
	}{
		{0, 0, "select 1"},                                         // one line: the statement at the cursor
		{3, 3, "select 2"},                                         // one line, two statements: still the cursor's
		{0, 3, "select 1|select 2|select 3"},                       // a selection: all its statements
		{3, 4, "select 2|select 3|select\n  4"},                    // partly selected statements count
		{5, 8, "select\n  4|select 5"},                             //
		{6, 7, "select\n  4"},                                      // only blank lines: the cursor's (previous)
		{0, 99, "select 1|select 2|select 3|select\n  4|select 5"}, // whole file
	}
	for _, c := range cases {
		got, ok := InLines(text, st, c.from, c.to, Position{Line: c.from})
		var sqls []string
		for _, s := range got {
			sqls = append(sqls, s.SQL)
		}
		if !ok || strings.Join(sqls, "|") != c.want {
			t.Errorf("lines %d-%d: got %q want %q", c.from, c.to, strings.Join(sqls, "|"), c.want)
		}
	}
}

func TestFormatHazards(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string // "line:what"
	}{
		{"plain select", "select a, b from t where x = '4,4''-mdi';", nil},
		{"pg transaction begin", "begin;\nupdate t set a = 1;\ncommit;", nil},
		{"begin transaction", "begin transaction;\nselect 1;\ncommit;", nil},
		{"slash in string/comment", "select '/' from t; -- a / b\n/* \n/\n*/ select 1;", nil},
		{"pg trigger without begin", "create trigger trg after insert on t for each row execute function f();", nil},
		{"q quote in comment", "-- q'[x]'\nselect 1;", nil},
		{"slash line", "select 1 from dual\n/\n", []string{"1:'/' terminator line"}},
		{"q quote", "select 1;\nselect q'[a]' from dual;", []string{"1:Oracle q'...' string"}},
		{"plsql", "select 1;\n\nbegin\n  null;\nend;", []string{"2:PL/SQL block"}},
		{"create procedure", "create or replace procedure p as begin null; end;", []string{"0:PL/SQL block"}},
		{"pg dollar", "do $$ begin perform 1; end $$;", []string{"0:dollar-quoted body ($$)"}},
		{"sqlite trigger", "create trigger trg after insert on t begin update u set a = 1; end;", []string{"0:trigger body"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, h := range FormatHazards(tt.in) {
				got = append(got, fmt.Sprintf("%d:%s", h.Line, h.What))
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}
