package handler

import (
	"testing"

	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lint"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

func oracleCache(synonyms map[string][]string) *database.DBCache {
	return &database.DBCache{
		Schemas:      map[string]string{"CADE": "CADE"},
		SchemaTables: map[string][]string{"CADE": {"CAPRODUCT"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{
			"CADE\tCAPRODUCT": {{ColumnBase: database.ColumnBase{Schema: "CADE", Table: "CAPRODUCT", Name: "PROD_ID"}}},
		},
		Synonyms: synonyms,
	}
}

func TestBuildCatalogOracleSynonyms(t *testing.T) {
	cache := oracleCache(nil)
	cache.SetDefaultSchema("CADE")
	// before the background load: an unknown unqualified name may be a synonym
	if ds := lint.Check("select * from caprodcut", buildCatalog(cache, sqlsplit.Oracle)); len(ds) != 0 {
		t.Errorf("synonyms not loaded: %v", ds)
	}
	cache = oracleCache(map[string][]string{"PUBLIC": {"USER_TABLES"}, "CADE": {"MYSYN"}})
	cache.SetDefaultSchema("CADE")
	cat := buildCatalog(cache, sqlsplit.Oracle)
	if ds := lint.Check("select * from caprodcut", cat); len(ds) != 1 {
		t.Errorf("synonyms loaded: want 1 diagnostic, got %v", ds)
	}
	if ds := lint.Check("select * from user_tables; select * from mysyn; select p.prod_id from caproduct p", cat); len(ds) != 0 {
		t.Errorf("synonyms/columns: %v", ds)
	}
	if ds := lint.Check("select p.nope from caproduct p", cat); len(ds) != 1 {
		t.Errorf("column: want 1, got %v", ds)
	}
}
