package handler

import (
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

func TestViewDefinition(t *testing.T) {
	cache := viewCache()
	written := map[string]string{}
	filer := func(v *database.ViewDef) (string, string, error) {
		text := "-- header\n" + v.CreateStatement()
		written[v.Name] = text
		return "/views/" + v.Name + ".sql", text, nil
	}
	cases := []struct {
		name, sql string
		char      int
		file      string
		line      int
		at        string // the text at the location
	}{
		{"view in FROM", "select * from v_doc d", 15, "v_doc", 1, "cadetst.v_doc"},
		{"schema.view", "select * from cadetst.v_doc", 23, "v_doc", 1, "cadetst.v_doc"},
		{"alias.column", "select d.batch_name from v_doc d", 10, "v_doc", 4, "batch_name"},
		{"view.column", "select v_doc.batch_id from v_doc", 14, "v_doc", 3, "batch_id"},
		{"unqualified column of the only view", "select added_on from v_doc", 8, "v_doc", 5, "added_on"},
		{"column on the dot", "select d.id from v_doc d", 8, "v_doc", 2, "id"},
		{"materialized view", "select * from mv_sum", 15, "mv_sum", 1, "cadetst.mv_sum"},
		{"matview column", "select documents from mv_sum", 8, "mv_sum", 2, "documents"},
		{"column a table has too", "select batch_id from v_doc d join batch_document bd on bd.id = d.id", 8, "", 0, ""},
		{"table", "select * from batch", 15, "", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
				Position:     lsp.Position{Line: 0, Character: c.char},
			}}
			got, err := definition(testFileURI, c.sql, params, cache, filer)
			if err != nil {
				t.Fatal(err)
			}
			locs := []lsp.Location(got)
			if c.file == "" {
				if len(locs) != 0 {
					t.Errorf("want none, got %+v", got)
				}
				return
			}
			if len(locs) != 1 || locs[0].URI != "file:///views/"+c.file+".sql" {
				t.Fatalf("got %+v", got)
			}
			r := locs[0].Range
			lines := strings.Split(written[c.file], "\n")
			if r.Start.Line != c.line || lines[r.Start.Line][r.Start.Character:r.End.Character] != c.at {
				t.Errorf("location %+v in:\n%s", r, written[c.file])
			}
		})
	}
}

func TestViewFile(t *testing.T) {
	t.Setenv("SQLH_RUNTIME_DIR", t.TempDir())
	s := &Server{curDBCfg: &database.DBConfig{Alias: "pg local"}}
	v := viewCache().Relations.View("v_doc")
	for i := 0; i < 2; i++ { // rewritten although read-only
		path, text, err := s.viewFile(v)
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
}

func TestBuildCatalogViewWrites(t *testing.T) {
	cat := buildCatalog(viewCache(), sqlsplit.PostgreSQL)
	got := lint.Check("update v_doc set batch_name = 'x';\ndelete from mv_sum;\ninsert into batch (id) select id from v_doc", cat)
	if len(got) != 2 || got[0].Message != "v_doc is a read-only view: the database can't write through it" ||
		got[1].Message != "mv_sum is a materialized view: it can't be written (refresh it instead)" {
		t.Errorf("got %+v", got)
	}
}
