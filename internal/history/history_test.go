package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokens(t *testing.T) {
	got := strings.Join(Tokens("SELECT  a.Id, 'It''s' -- note\nFROM \"T\" /* x */ WHERE n>=1"), " ")
	want := `select a.id , 'It''s' from "t" where n > = 1`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSimilarity(t *testing.T) {
	a := Tokens("select * from orders o where o.total > 10")
	b := Tokens("SELECT *\nFROM orders o\nWHERE o.total > 100 and o.status = 'open'")
	c := Tokens("delete from audit_log where created < now() - interval '1 day'")
	if s := Similarity(a, b); s < Similar {
		t.Errorf("a~b %.2f", s)
	}
	if s := Similarity(a, c); s >= Similar {
		t.Errorf("a~c %.2f", s)
	}
	if Similarity(a, a) != 1 {
		t.Error("a~a")
	}
}

func TestAppendLineageAndVersions(t *testing.T) {
	ws := t.TempDir()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	add := func(file, sql string, min int) Entry {
		e, err := Append(ws, Entry{File: file, SQL: sql, Conn: "pg", Status: "ok", Time: base.Add(time.Duration(min) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e1 := add("/w/q.sql", "select * from orders o", 0)
	e2 := add("/w/q.sql", "select * from orders o where o.total > 10", 1)
	e3 := add("/w/q.sql", "delete from audit_log where created < now()", 2)
	e4 := add("/w/other.sql", "select * from orders o", 3) // another file: a new query
	e5 := add("/w/q.sql", "select * from orders o where o.total > 10", 4)
	if e2.Lineage != e1.Lineage || e5.Lineage != e1.Lineage {
		t.Errorf("versions: %s %s %s", e1.Lineage, e2.Lineage, e5.Lineage)
	}
	if e3.Lineage == e1.Lineage || e4.Lineage == e1.Lineage {
		t.Errorf("separate queries: %s %s", e3.Lineage, e4.Lineage)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".sqlh", ".gitignore")); !strings.Contains(string(b), "*") {
		t.Error("no .gitignore")
	}

	got, err := Find(ws, Query{File: "/w/q.sql", Like: "select * from orders o where o.total > 50"})
	if err != nil || len(got) != 3 || got[0].ID != e5.ID {
		t.Fatalf("find like: %d %v", len(got), err)
	}
	vs := Versions(got)
	if len(vs) != 1 || len(vs[0]) != 2 || vs[0][0].SQL != e5.SQL || vs[0][1].SQL != e1.SQL {
		t.Errorf("versions %+v", vs)
	}
	all, _ := Find(ws, Query{})
	if g := Versions(all); len(g) != 3 || g[0][0].ID != e5.ID {
		t.Errorf("all versions %d", len(g))
	}
	if lim, _ := Find(ws, Query{Limit: 2}); len(lim) != 2 {
		t.Errorf("limit %d", len(lim))
	}
}
