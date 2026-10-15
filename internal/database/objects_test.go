package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestSQLiteSchemaObjects(t *testing.T) {
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, s := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE, a INT, b INT)",
		"CREATE INDEX t_ab ON t (a, b)",
		"CREATE TRIGGER t_trg AFTER INSERT ON t BEGIN SELECT 1; END",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	objs, err := (&SQLite3DBRepository{Conn: conn}).SchemaObjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range objs.List() {
		got = append(got, x.Kind+" "+x.Name+" "+x.Table+" "+strings.Join(x.Columns, ","))
	}
	want := "index t_ab t a,b|trigger t_trg t "
	if strings.Join(got, "|") != want {
		t.Errorf("got %q, want %q", strings.Join(got, "|"), want)
	}
}

func TestSchemaObjectsLookup(t *testing.T) {
	o := NewSchemaObjects([]*SchemaObject{
		{Kind: ObjectIndex, Schema: "s", Name: "IX_B", Table: "T"},
		{Kind: ObjectIndex, Schema: "s", Name: "ix_a", Table: "t"},
		{Kind: ObjectConstraint, Schema: "s", Name: "fk", Table: "t", Detail: "foreign key", Columns: []string{"c"}, RefTable: "u"},
		{Kind: ObjectIndex, Schema: "other", Name: "ix_a", Table: "t"},
	})
	if n := len(o.All(ObjectIndex, "S")); n != 2 {
		t.Errorf("All: %d", n)
	}
	if len(o.Find(ObjectIndex, "s", "IX_A")) != 1 || len(o.FindAnySchema(ObjectIndex, "ix_a")) != 2 {
		t.Error("Find")
	}
	if len(o.OfTable(ObjectIndex, "s", "T")) != 2 {
		t.Error("OfTable")
	}
	if d := o.Find(ObjectConstraint, "s", "fk")[0].Doc(); d != "`fk` foreign key constraint on `t` (c) → `u`" {
		t.Errorf("Doc: %q", d)
	}
	o.MarkPartial(ObjectTrigger, "s")
	if !o.Partial(ObjectTrigger, "S") || o.Partial(ObjectIndex, "s") {
		t.Error("Partial")
	}
	var nilObjects *SchemaObjects
	if nilObjects.All(ObjectIndex, "s") != nil || nilObjects.Find(ObjectIndex, "s", "x") != nil {
		t.Error("nil")
	}
}
