package database

import (
	"database/sql"
	"testing"

	"github.com/raikhan/sqlh/dialect"
)

func TestOracleType(t *testing.T) {
	n := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	none := sql.NullInt64{}
	tests := []struct {
		typ         string
		length, cl  int64
		used        string
		prec, scale sql.NullInt64
		want        string
	}{
		{"VARCHAR2", 240, 60, "C", none, none, "VARCHAR2(60 CHAR)"},
		{"VARCHAR2", 60, 60, "B", none, none, "VARCHAR2(60)"},
		{"NVARCHAR2", 200, 100, "C", none, none, "NVARCHAR2(100)"},
		{"NUMBER", 22, 0, "", none, none, "NUMBER"},
		{"NUMBER", 22, 0, "", n(10), n(0), "NUMBER(10)"},
		{"NUMBER", 22, 0, "", n(12), n(2), "NUMBER(12,2)"},
		{"NUMBER", 22, 0, "", none, n(0), "NUMBER(*,0)"},
		{"FLOAT", 22, 0, "", n(126), none, "FLOAT(126)"},
		{"RAW", 16, 0, "", none, none, "RAW(16)"},
		{"TIMESTAMP(6)", 11, 0, "", none, n(6), "TIMESTAMP(6)"},
		{"CLOB", 4000, 0, "", none, none, "CLOB"},
	}
	for _, tt := range tests {
		if got := oracleType(tt.typ, tt.length, tt.cl, tt.used, tt.prec, tt.scale); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.typ, got, tt.want)
		}
	}
}

func TestSplitQualified(t *testing.T) {
	tests := []struct {
		in     string
		driver dialect.DatabaseDriver
		schema string
		name   string
	}{
		{"castock", dialect.DatabaseDriverOracle, "", "CASTOCK"},
		{"cade.castock", dialect.DatabaseDriverOracle, "CADE", "CASTOCK"},
		{`cade."Mixed"`, dialect.DatabaseDriverOracle, "CADE", "Mixed"},
		{"Public.Batch", dialect.DatabaseDriverPostgreSQL, "public", "batch"},
		{`"My Schema"."a.b"`, dialect.DatabaseDriverPostgreSQL, "My Schema", "a.b"},
		{"Orders", dialect.DatabaseDriverSQLite3, "", "Orders"},
	}
	for _, tt := range tests {
		s, n := SplitQualified(tt.in, tt.driver)
		if s != tt.schema || n != tt.name {
			t.Errorf("%s: %q %q", tt.in, s, n)
		}
	}
}
