package sqlsplit

import (
	"reflect"
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
