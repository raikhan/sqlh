package database

import (
	"context"
	"database/sql"
	"testing"
)

func TestSQLiteDescribeViews(t *testing.T) {
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE VIEW v_ro AS SELECT id, name FROM t`,
		`CREATE VIEW v_ins(a, b) AS SELECT id, name FROM t`,
		`CREATE TRIGGER v_ins_insert INSTEAD OF INSERT ON v_ins BEGIN INSERT INTO t VALUES (new.a, new.b); END`,
		`CREATE TRIGGER t_upd AFTER UPDATE ON t BEGIN SELECT 1; END`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	views, err := NewSQLite3DBRepository(conn).(ViewDescriber).DescribeViews(context.Background(), "")
	if err != nil || len(views) != 2 {
		t.Fatalf("%v %v", views, err)
	}
	ins, ro := views[0], views[1]
	if ins.Writes.Summary() != "only INSERT" || ins.Query != "" || ins.CreateStatement() != "CREATE VIEW v_ins(a, b) AS SELECT id, name FROM t;\n" {
		t.Errorf("v_ins: %+v %q", ins.Writes, ins.CreateStatement())
	}
	if !ro.Writes.ReadOnly() || ro.Query != "SELECT id, name FROM t" {
		t.Errorf("v_ro: %+v %q", ro.Writes, ro.Query)
	}
}
