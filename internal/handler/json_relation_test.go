package handler

import (
	"testing"

	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
)

func TestRelationColumnForJSON(t *testing.T) {
	cache := &database.DBCache{ColumnsWithParent: map[string][]*database.ColumnDesc{
		"CADETST\tSDS_DATA_RAW": {
			{ColumnBase: database.ColumnBase{Schema: "cadetst", Table: "sds_data_raw", Name: "id"}, Type: "bigint"},
			{ColumnBase: database.ColumnBase{Schema: "cadetst", Table: "sds_data_raw", Name: "raw_data"}, Type: "jsonb"},
		},
	}}
	cache.SetDefaultSchema("cadetst")
	tests := []struct {
		name, sql, qualifier, column string
		ok                           bool
	}{
		{"derived table", "select tmp.raw_data->>'' from (select sds.id, sds.raw_data from sds_data_raw sds) tmp", "tmp", "raw_data", true},
		{"renamed column", "select tmp.rd->>'' from (select raw_data as rd from sds_data_raw) tmp", "tmp", "rd", true},
		{"unqualified", "select rd->>'' from (select raw_data as rd from sds_data_raw) tmp", "", "rd", true},
		{"CTE", "with x as (select * from sds_data_raw) select x.raw_data->>'' from x", "x", "raw_data", true},
		{"expression", "select tmp.j->>'' from (select raw_data || '{}' as j from sds_data_raw) tmp", "tmp", "j", false},
	}
	for _, tt := range tests {
		parsed, err := parser.Parse(tt.sql)
		if err != nil {
			t.Fatal(err)
		}
		rels, err := parseutil.ExtractSubQueryViews(parsed, token.Pos{Line: 0, Col: 8})
		if err != nil {
			t.Fatal(err)
		}
		target, typ, ok := relationColumn(cache, rels, tt.qualifier, tt.column)
		if ok != tt.ok {
			t.Errorf("%s: ok = %v", tt.name, ok)
			continue
		}
		if ok && (target.Table != "sds_data_raw" || target.Column != "raw_data" || typ != "jsonb") {
			t.Errorf("%s: got %+v %q", tt.name, target, typ)
		}
	}
}
