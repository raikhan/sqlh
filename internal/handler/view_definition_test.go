package handler

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lint"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

const vDocQuery = ` SELECT bd.id,
    bd.batch_id,
    b.name AS batch_name,
    bd.added_on
   FROM batch_document bd
     JOIN batch b ON b.id = bd.batch_id;`

func viewCache() *database.DBCache {
	col := func(table, name string) *database.ColumnDesc {
		return &database.ColumnDesc{ColumnBase: database.ColumnBase{Schema: "cadetst", Table: table, Name: name}, Type: "bigint"}
	}
	cache := &database.DBCache{
		Schemas:      map[string]string{"CADETST": "cadetst"},
		SchemaTables: map[string][]string{"CADETST": {"batch", "batch_document", "v_doc", "mv_sum"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{
			"CADETST\tBATCH":          {col("batch", "id"), col("batch", "name")},
			"CADETST\tBATCH_DOCUMENT": {col("batch_document", "id"), col("batch_document", "batch_id"), col("batch_document", "added_on")},
			"CADETST\tV_DOC":          {col("v_doc", "id"), col("v_doc", "batch_id"), col("v_doc", "batch_name"), col("v_doc", "added_on")},
			"CADETST\tMV_SUM":         {col("mv_sum", "batch_id"), col("mv_sum", "documents")},
		},
	}
	cache.SetDefaultSchema("cadetst")
	cache.Relations = cache.BuildRelations([]*database.ViewDef{
		{Schema: "cadetst", Name: "v_doc", Query: vDocQuery, Uses: []string{"batch", "batch_document"}, Writes: &database.ViewWrites{}},
		{Schema: "cadetst", Name: "mv_sum", Materialized: true, Query: " SELECT bd.batch_id, count(*) AS documents FROM batch_document bd GROUP BY bd.batch_id;",
			Uses: []string{"batch_document"}, Writes: &database.ViewWrites{}},
	})
	return cache
}

// gotoDefinition: definitionLocal + fetchDefinition without a database (the
// cache's views; tables generated from the cache), the text as written.
func gotoDefinition(t *testing.T, cache *database.DBCache, sql string, char int) (*defTarget, string, lsp.Location) {
	t.Helper()
	params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 0, Character: char},
	}}
	local, target, err := definitionLocal(testFileURI, sql, params, cache, sqlsplit.PostgreSQL)
	if err != nil || local != nil {
		t.Fatalf("local %v %v", local, err)
	}
	if target == nil {
		return nil, "", lsp.Location{}
	}
	d, err := fetchDefinition(context.Background(), target, defDeps{cache: cache, driver: "postgresql"})
	if err != nil || d == nil {
		t.Fatalf("fetch %+v: %v", target, err)
	}
	text := definitionText("pg", d)
	return target, text, definitionLocation("/views/"+qualified(d)+".sql", text, d, target.column)
}

func TestViewDefinition(t *testing.T) {
	cache := viewCache()
	cases := []struct {
		name, sql string
		char      int
		file      string // "" = no definition
		line      int
		at        string // the text at the location
	}{
		{"view in FROM", "select * from v_doc d", 15, "cadetst.v_doc", 1, "cadetst.v_doc"},
		{"schema.view", "select * from cadetst.v_doc", 23, "cadetst.v_doc", 1, "cadetst.v_doc"},
		{"schema.view on the schema", "select * from cadetst.v_doc", 16, "cadetst.v_doc", 1, "cadetst.v_doc"},
		{"alias.column", "select d.batch_name from v_doc d", 10, "cadetst.v_doc", 4, "batch_name"},
		{"view.column", "select v_doc.batch_id from v_doc", 14, "cadetst.v_doc", 3, "batch_id"},
		{"unqualified column of the only view", "select added_on from v_doc", 8, "cadetst.v_doc", 5, "added_on"},
		{"column on the dot", "select d.id from v_doc d", 8, "cadetst.v_doc", 2, "id"},
		{"materialized view", "select * from mv_sum", 15, "cadetst.mv_sum", 1, "cadetst.mv_sum"},
		{"matview column", "select documents from mv_sum", 8, "cadetst.mv_sum", 2, "documents"},
		{"column a table has too", "select batch_id from v_doc d join batch_document bd on bd.id = d.id", 8, "", 0, ""},
		// tables: generated from the cache here (no database)
		{"table", "select * from batch", 15, "cadetst.batch", 2, "cadetst.batch"},
		{"table column", "select b.name from batch b", 9, "cadetst.batch", 4, "name"},
		{"schema.table column", "select b.name from cadetst.batch b", 9, "cadetst.batch", 4, "name"},
		{"DDL: ALTER TABLE", "alter table batch add column x int", 13, "cadetst.batch", 2, "cadetst.batch"},
		{"DDL: DROP COLUMN", "alter table batch drop column name", 31, "cadetst.batch", 4, "name"},
		{"DDL: a new table", "create table t2 (id int)", 14, "", 0, ""},
		{"unknown", "select * from nope", 15, "", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, text, loc := gotoDefinition(t, cache, c.sql, c.char)
			if c.file == "" {
				if target != nil {
					t.Errorf("want none, got %+v", target)
				}
				return
			}
			if target == nil || loc.URI != "file:///views/"+c.file+".sql" {
				t.Fatalf("got %+v %s", target, loc.URI)
			}
			r := loc.Range
			lines := strings.Split(text, "\n")
			if r.Start.Line != c.line || lines[r.Start.Line][r.Start.Character:r.End.Character] != c.at {
				t.Errorf("location %+v in:\n%s", r, text)
			}
		})
	}
}

func TestDefinitionFile(t *testing.T) {
	t.Setenv("SQLH_RUNTIME_DIR", t.TempDir())
	cache := viewCache()
	d, err := fetchDefinition(context.Background(), newTarget(cache, "", "v_doc", ""), defDeps{cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // rewritten although read-only
		path, text, err := definitionFile("", "pg local", d)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(path, control.ViewsDir("")+"/") || !strings.HasSuffix(path, "/pg_local/cadetst.v_doc.sql") {
			t.Errorf("path %s", path)
		}
		b, _ := os.ReadFile(path)
		fi, _ := os.Stat(path)
		if string(b) != text || fi.Mode().Perm() != 0o400 {
			t.Errorf("file %v:\n%s", fi.Mode(), b)
		}
		want := "-- cadetst.v_doc (view, read-only) in pg local: a read-only copy of its definition, rewritten on every go-to-definition\n" +
			"CREATE OR REPLACE VIEW cadetst.v_doc AS\n" + vDocQuery + "\n"
		if text != want {
			t.Errorf("got:\n%s\nwant:\n%s", text, want)
		}
	}
	// a table from the cache: generated, and says so
	td, _ := fetchDefinition(context.Background(), newTarget(cache, "", "batch", ""), defDeps{cache: cache, driver: "postgresql"})
	want := "-- cadetst.batch (table) in pg: a read-only copy of its definition (generated from the schema cache), rewritten on every go-to-definition\n" +
		"-- approximate: no CHECK constraints, index expressions or comments\n" +
		"CREATE TABLE cadetst.batch (\n  id bigint,\n  name bigint\n);\n"
	if got := definitionText("pg", td); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildCatalogViewWrites(t *testing.T) {
	cat := buildCatalog(viewCache(), sqlsplit.PostgreSQL)
	got := lint.Check("update v_doc set batch_name = 'x';\ndelete from mv_sum;\ninsert into batch (id) select id from v_doc", cat)
	if len(got) != 2 || got[0].Message != "v_doc is a read-only view: the database can't write through it" ||
		got[1].Message != "mv_sum is a materialized view: it can't be written (refresh it instead)" {
		t.Errorf("got %+v", got)
	}
}

// failingRepo: a repository whose definitions fail (a timeout, say).
type failingRepo struct {
	*database.MockDBRepository
	err error
}

func (r failingRepo) Definition(context.Context, string, string, database.DefinitionOptions) (*database.Definition, error) {
	return nil, r.err
}

func TestDefinitionFallsBackToTheCache(t *testing.T) {
	cache := viewCache()
	repo := failingRepo{database.NewMockDBRepository(nil).(*database.MockDBRepository), context.DeadlineExceeded}
	d, err := fetchDefinition(context.Background(), newTarget(cache, "", "batch", ""), defDeps{cache: cache, repo: repo, driver: "postgresql"})
	if err != nil || d == nil || !strings.HasPrefix(d.DDL, "CREATE TABLE cadetst.batch (") ||
		!strings.Contains(d.Note, "the database didn't answer: context deadline exceeded") {
		t.Fatalf("%+v %v", d, err)
	}
	// unknown to the database: an error, not a guess
	repo.err = database.ErrNoObject
	if _, err := fetchDefinition(context.Background(), newTarget(cache, "", "batch", ""), defDeps{cache: cache, repo: repo}); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("no object: %v", err)
	}
}
