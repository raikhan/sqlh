package exec

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		"create table t (id integer primary key, name text, note text)",
		`insert into t (name, note) values ('a', 'plain'), ('b', 'has, comma'), ('c', 'line1
line2 "quoted"'), ('d', null), ('e', '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func stmt(sql string) sqlsplit.Statement {
	return sqlsplit.Split(sql, sqlsplit.SQLite)[0]
}

func readCSV(t *testing.T, p string) [][]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestReturnsRows(t *testing.T) {
	cases := map[string]bool{
		"select 1":                               true,
		"-- c\n/* d */ SELECT 1":                 true,
		"with x as (select 1) select * from x":   true,
		"values (1)":                             true,
		"pragma table_info(t)":                   true,
		"pragma foreign_keys = on":               false,
		"insert into t values (1)":               false,
		"insert into t values (1) returning id":  true,
		"update t set a = 1":                     false,
		"delete from t where id = 1 returning *": true,
		"create table x (a int)":                 false,
		"begin":                                  false,
		"explain select 1":                       true,
	}
	for q, want := range cases {
		if got := ReturnsRows(sqlsplit.Statement{SQL: q}); got != want {
			t.Errorf("%q: got %v want %v", q, got, want)
		}
	}
	if ReturnsRows(sqlsplit.Statement{SQL: "begin null; end;", PLSQL: true}) {
		t.Error("PL/SQL must not return rows")
	}
}

func TestValueFormatter(t *testing.T) {
	utc := func(s string) time.Time { v, _ := time.Parse("2006-01-02 15:04:05.999999999", s); return v }
	plus10 := time.FixedZone("+10:00", 10*3600)
	pg := ValueFormatter{Dialect: sqlsplit.PostgreSQL}
	ora := ValueFormatter{Dialect: sqlsplit.Oracle, Null: "NULL"}
	cases := []struct {
		f      ValueFormatter
		v      any
		dbType string
		want   string
	}{
		{pg, nil, "TEXT", ""},
		{ora, nil, "NCHAR", "NULL"},
		{pg, "12345678901234567890", "NUMERIC", "12345678901234567890"},
		{pg, int64(42), "INT4", "42"},
		{pg, 1.5, "FLOAT8", "1.5"},
		{pg, 1e20, "FLOAT8", "100000000000000000000"},
		{pg, true, "BOOL", "true"},
		{pg, []byte(`{"a":1}`), "JSONB", `{"a":1}`},
		{pg, []byte{0xde, 0xad}, "BYTEA", "0xdead"},
		{ora, []byte("AB"), "RAW", "0x4142"},   // binary type even if valid UTF-8
		{pg, []byte{0xff, 0xfe}, "", "0xfffe"}, // invalid UTF-8
		{pg, utc("2026-04-01 00:00:00"), "DATE", "2026-04-01"},
		{ValueFormatter{Dialect: sqlsplit.SQLite}, utc("2026-04-01 10:11:12"), "DATE", "2026-04-01 10:11:12"}, // keep time if present
		{ora, utc("2026-04-01 00:00:00"), "DATE", "2026-04-01 00:00:00"},                                      // Oracle DATE has a time
		{pg, utc("2026-04-01 10:11:12.123456"), "TIMESTAMP", "2026-04-01 10:11:12.123456"},
		{ora, utc("2026-04-01 10:11:12"), "TimeStampDTY", "2026-04-01 10:11:12"},
		{ora, time.Date(2026, 4, 1, 10, 11, 12, 5e8, plus10), "TimeStampTZ_DTY", "2026-04-01 10:11:12.5+10:00"},
		{pg, time.Date(2026, 4, 1, 8, 0, 0, 0, plus10), "TIMESTAMPTZ", "2026-04-01 08:00:00+10:00"},
	}
	for _, c := range cases {
		if got := c.f.Format(c.v, c.dbType); got != c.want {
			t.Errorf("Format(%#v, %q) = %q, want %q", c.v, c.dbType, got, c.want)
		}
	}
}

func TestRunCSVPreviewAndExport(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	q := stmt("select name, note from t order by id")

	// preview: limit 2 of 5 → truncated
	p1 := filepath.Join(dir, "p.csv")
	out, err := Run(ctx, db, q, func() (Sink, error) { return NewCSVFile(p1), nil }, Options{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != KindResultSet || out.Rows != 2 || !out.Truncated || out.Columns != 2 {
		t.Errorf("preview outcome %+v", out)
	}
	if got := readCSV(t, p1); len(got) != 3 || !reflect.DeepEqual(got[0], []string{"name", "note"}) {
		t.Errorf("preview csv %q", got)
	}

	// limit equal to the row count → not truncated
	out, err = Run(ctx, db, q, func() (Sink, error) { return &Table{}, nil }, Options{Limit: 5})
	if err != nil || out.Truncated || out.Rows != 5 {
		t.Errorf("exact limit: %+v %v", out, err)
	}

	// export: all rows, quoting round-trips, NULL vs empty
	p2 := filepath.Join(dir, "e.csv")
	out, err = Run(ctx, db, q, func() (Sink, error) { return NewCSVFile(p2), nil }, Options{Values: ValueFormatter{Null: `\N`}})
	if err != nil || out.Rows != 5 || out.Truncated {
		t.Fatalf("export: %+v %v", out, err)
	}
	got := readCSV(t, p2)
	want := [][]string{{"name", "note"}, {"a", "plain"}, {"b", "has, comma"}, {"c", "line1\nline2 \"quoted\""}, {"d", `\N`}, {"e", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("export csv\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(p2 + ".part"); !os.IsNotExist(err) {
		t.Error(".part file left behind")
	}
}

func TestRunExecDoesNotCreateSink(t *testing.T) {
	db := testDB(t)
	out, err := Run(context.Background(), db, stmt("update t set note = 'x' where id <= 3"),
		func() (Sink, error) { t.Fatal("sink created for DML"); return nil, nil }, Options{})
	if err != nil || out.Kind != KindExecuted || out.Affected != 3 {
		t.Errorf("%+v %v", out, err)
	}
}

func TestRunReturning(t *testing.T) {
	db := testDB(t)
	tb := &Table{}
	out, err := Run(context.Background(), db, stmt("insert into t (name) values ('z') returning id, name"),
		func() (Sink, error) { return tb, nil }, Options{})
	if err != nil || out.Kind != KindResultSet || len(tb.Rows) != 1 || tb.Rows[0][1] != "z" {
		t.Errorf("%+v %v %q", out, err, tb.Rows)
	}
}

// cancelSink cancels the context after the first row, like sqlh.cancel would.
type cancelSink struct {
	*CSVFile
	cancel context.CancelFunc
}

func (c cancelSink) Row(v []string) error { c.cancel(); return c.CSVFile.Row(v) }

func TestRunCancelRemovesPartialFile(t *testing.T) {
	db := testDB(t)
	p := filepath.Join(t.TempDir(), "x.csv")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Run(ctx, db, stmt("select * from t"), func() (Sink, error) {
		return cancelSink{NewCSVFile(p), cancel}, nil
	}, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	for _, f := range []string{p, p + ".part"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s exists after cancel", filepath.Base(f))
		}
	}
}

func TestFiles(t *testing.T) {
	ws := t.TempDir()
	dir := ExpandDir("", ws, "oracle dev/1")
	if dir != filepath.Join(ws, ".sqlh", "results") {
		t.Errorf("ExpandDir default = %s", dir)
	}
	if d := ExpandDir("out/{conn}", ws, "oracle dev/1"); d != filepath.Join(ws, "out", "oracle_dev_1") {
		t.Errorf("ExpandDir relative = %s", d)
	}
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	gi, err := os.ReadFile(filepath.Join(ws, ".sqlh", ".gitignore"))
	if err != nil || !strings.Contains(string(gi), "*") {
		t.Errorf(".sqlh/.gitignore: %q %v", gi, err)
	}

	at := time.Date(2026, 10, 12, 17, 30, 15, 0, time.Local)
	p1 := ResultPath(dir, "pg_local", at, 0)
	if filepath.Base(p1) != "pg_local_20261012-173015.csv" {
		t.Errorf("ResultPath = %s", p1)
	}
	os.WriteFile(p1, nil, 0o644)
	if p2 := ResultPath(dir, "pg_local", at, 0); filepath.Base(p2) != "pg_local_20261012-173015-2.csv" {
		t.Errorf("collision = %s", p2)
	}
	if p3 := ResultPath(dir, "pg_local", at, 2); filepath.Base(p3) != "pg_local_20261012-173015_2.csv" {
		t.Errorf("indexed = %s", p3)
	}

	for _, f := range []string{"a.csv", "b.csv"} { // repoint latest
		os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644)
		if err := UpdateLatest(dir, filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if target, _ := os.Readlink(filepath.Join(dir, LatestName)); target != "b.csv" {
		t.Errorf("latest -> %q", target)
	}
}

func TestAffectedOnlyForDML(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	noSink := func() (Sink, error) { return &Table{}, nil }
	for q, want := range map[string]int64{
		"update t set note = 'x' where id = 1": 1,
		"delete from t where id > 100":         0,
		"create table u (a int)":               -1,
		"create index ix on t (name)":          -1,
	} {
		out, err := Run(ctx, db, stmt(q), noSink, Options{})
		if err != nil || out.Affected != want {
			t.Errorf("%q: affected=%d want %d (err %v)", q, out.Affected, want, err)
		}
	}
}

func TestNumberedPathAndCopy(t *testing.T) {
	for n, want := range map[int]string{0: "/x/report.csv", 1: "/x/report.csv", 2: "/x/report_2.csv", 10: "/x/report_10.csv"} {
		if got := NumberedPath("/x/report.csv", n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
	if got := NumberedPath("/x/out", 2); got != "/x/out_2" {
		t.Errorf("no extension: %s", got)
	}

	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.csv"), filepath.Join(dir, "sub", "dst.csv")
	os.WriteFile(src, []byte("a\n1\n"), 0o644)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst+".part", []byte("stale"), 0o644) // from an interrupted run
	os.WriteFile(dst, []byte("old"), 0o644)
	if err := CopyResult(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "a\n1\n" {
		t.Errorf("copied %q", b)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Error(".part left behind")
	}
	if err := CopyResult(filepath.Join(dir, "missing.csv"), dst); err == nil {
		t.Error("want an error for a missing source")
	}
	if b, _ := os.ReadFile(dst); string(b) != "a\n1\n" {
		t.Errorf("failed copy replaced the file: %q", b)
	}
}

func TestReadOnly(t *testing.T) {
	cases := map[string]bool{
		"select * from t": true,
		"-- c\nwith x as (select 1) select * from x": true,
		"values (1)":                        true,
		"table t":                           true,
		"select updated_at, deleted from t": true,
		"select * from t for update":        false,
		"with d as (delete from t returning *) select * from d": false,
		"select * into t2 from t":                               false,
		"select nextval('s')":                                   false,
		"insert into t values (1)":                              false,
		"update t set a = 1":                                    false,
		"begin":                                                 false,
		"commit":                                                false,
		"set search_path = x":                                   false,
		"explain analyze delete from t":                         false,
		"create table x (a int)":                                false,
	}
	for q, want := range cases {
		if got := ReadOnly(sqlsplit.Statement{SQL: q}); got != want {
			t.Errorf("%q: got %v want %v", q, got, want)
		}
	}
	if ReadOnly(sqlsplit.Statement{SQL: "begin null; end;", PLSQL: true}) {
		t.Error("PL/SQL is not read-only")
	}
}

func TestIsDDL(t *testing.T) {
	for sql, want := range map[string]bool{
		"CREATE TABLE t (a int)":                                    true,
		"-- note\nalter table t add b int":                          true,
		"DROP VIEW v":                                               true,
		"COMMENT ON TABLE t IS 'x'":                                 true,
		"RENAME a TO b":                                             true,
		"CREATE OR REPLACE PROCEDURE p IS BEGIN NULL; END;":         true,
		"BEGIN EXECUTE IMMEDIATE 'DROP TABLE t'; END;":              true,
		"DO $$ BEGIN EXECUTE 'create index i on t (a)'; END $$":     true,
		"BEGIN EXECUTE IMMEDIATE q'[CREATE TABLE x (a int)]'; END;": true,
		"SELECT 1":                 false,
		"INSERT INTO t VALUES (1)": false,
		"TRUNCATE TABLE t":         false,
		"GRANT SELECT ON t TO u":   false,
		"BEGIN EXECUTE IMMEDIATE 'DELETE FROM t'; END;": false,
	} {
		if got := IsDDL(sqlsplit.Statement{SQL: sql}); got != want {
			t.Errorf("%q: %v, want %v", sql, got, want)
		}
	}
}
