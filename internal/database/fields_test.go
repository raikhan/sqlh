package database

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
)

func fieldMap(fs []Field) map[string]string {
	m := map[string]string{}
	for _, f := range fs {
		m[f.Label] = f.Value
	}
	return m
}

func TestFields(t *testing.T) {
	cases := []struct {
		name     string
		cfg      DBConfig
		db, sch  string
		want     map[string]string
		notInAny string
	}{
		{"pg key=value", DBConfig{Driver: dialect.DatabaseDriverPostgreSQL,
			DataSourceName: "host=localhost port=5432 user=admin password=verysecret dbname=sdsai sslmode=disable options='-c search_path=cadetst,public'"},
			"sdsai", "cadetst", map[string]string{"Host": "localhost", "Port": "5432", "User": "admin", "Password": masked, "SSL mode": "disable", "options": "-c search_path=cadetst,public"}, "verysecret"},
		{"pg url", DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, DataSourceName: "postgres://u:pw@db.example:5433/app?sslmode=require"},
			"app", "", map[string]string{"Host": "db.example", "Port": "5433", "User": "u", "Password": masked, "SSL mode": "require"}, "pw@"},
		{"pg fields", DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, Host: "h", Port: 5411, DBName: "sds_ai", User: "svc", Passwd: "x", Schemas: []string{"public"}, Params: map[string]string{"sslmode": "disable"}},
			"sds_ai", "public", map[string]string{"Host": "h", "Port": "5411", "Database": "sds_ai", "Schema": "public", "SSL mode": "disable", "Schemas": "public"}, ""},
		{"oracle", DBConfig{Driver: dialect.DatabaseDriverOracle, Host: "o", Port: 1521, DBName: "CADEPROD", User: "sdsai", PasswdCmd: "security x"},
			"CADEPROD", "SDSAI", map[string]string{"Password cmd": "security x", "Schema": "SDSAI"}, ""},
		{"oracle schemas", DBConfig{Driver: dialect.DatabaseDriverOracle, DBName: "CADEV1", User: "u", Schemas: []string{"cade"}},
			"CADEV1", "CADE", nil, ""},
		{"sqlite", DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: "file:/tmp/sqls-build/scratch.db"},
			"scratch.db", "", map[string]string{"File": "/tmp/sqls-build/scratch.db"}, ""},
		{"mysql", DBConfig{Driver: dialect.DatabaseDriverMySQL, DataSourceName: "root:pw@tcp(127.0.0.1:3306)/shop?parseTime=true"},
			"shop", "", map[string]string{"Host": "127.0.0.1", "Port": "3306", "User": "root", "Password": masked, "parseTime": "true"}, "pw@"},
	}
	for _, c := range cases {
		db, sch := c.cfg.Target()
		if db != c.db || sch != c.sch {
			t.Errorf("%s: target %q %q", c.name, db, sch)
		}
		got := fieldMap(c.cfg.Fields())
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q (%v)", c.name, k, got[k], v, got)
			}
		}
		if c.notInAny != "" {
			for k, v := range got {
				if strings.Contains(v, c.notInAny) {
					t.Errorf("%s: %s shows %q", c.name, k, v)
				}
			}
		}
	}
}
