package database

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
)

func TestPgSearchPath(t *testing.T) {
	tests := []struct{ schema, path, want string }{
		{"public", `"$user", public`, `"public", "$user"`},
		{"cadetst", `cadetst,public`, `"cadetst", public`},
		{"sales", `"$user", public`, `"sales", "$user", public`},
		{"Mixed", `"Mixed", mixed, "a,b"`, `"Mixed", mixed, "a,b"`},
		{"x", ``, `"x"`},
		{"x", `X`, `"x"`}, // unquoted X folds to x
	}
	for _, tt := range tests {
		if got := pgSearchPath(tt.schema, tt.path); got != tt.want {
			t.Errorf("pgSearchPath(%q, %q) = %s, want %s", tt.schema, tt.path, got, tt.want)
		}
	}
}

func TestCurrentSchema(t *testing.T) {
	ora := func(schema string, schemas ...string) *DBConfig {
		return &DBConfig{Driver: dialect.DatabaseDriverOracle, Schema: schema, Schemas: schemas}
	}
	pg := func(schema string, schemas ...string) *DBConfig {
		return &DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, Schema: schema, Schemas: schemas}
	}
	tests := []struct {
		name string
		cfg  *DBConfig
		want string
	}{
		{"oracle: first of the list", ora("", "cade", "cade_exp"), "CADE"},
		{"oracle: schema", ora("cade_exp", "CADE", "CADE_EXP"), "CADE_EXP"},
		{"oracle: no list, the login's", ora(""), ""},
		{"postgresql: as written", pg("", "Sales", "public"), "Sales"},
		{"postgresql: no list, the search_path's", pg(""), ""},
		{"sqlite: none", &DBConfig{Driver: dialect.DatabaseDriverSQLite3, Schemas: []string{"main"}}, ""},
	}
	for _, tt := range tests {
		if got := tt.cfg.CurrentSchema(); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
	if s, ok := ora("", "Cade").AllowedSchema("CADE"); !ok || s != "CADE" {
		t.Errorf("oracle AllowedSchema: %q %v", s, ok)
	}
	if _, ok := pg("", "Sales").AllowedSchema("sales"); ok {
		t.Error("postgresql AllowedSchema is case-sensitive")
	}
}

func TestValidateSchema(t *testing.T) {
	c := &DBConfig{Driver: dialect.DatabaseDriverOracle, DataSourceName: "oracle://u:p@h/s", Schema: "other", Schemas: []string{"CADE"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "not in connections[].schemas") {
		t.Errorf("schema outside the list: %v", err)
	}
	c.Schema = "cade"
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	s := &DBConfig{Driver: dialect.DatabaseDriverSQLite3, Schema: "main"}
	if err := s.Validate(); err == nil {
		t.Error("schema on SQLite accepted")
	}
	// host without proto: TCP
	p := &DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, Host: "db", User: "u"}
	if err := p.Validate(); err != nil || p.Proto != ProtoTCP {
		t.Errorf("proto default: %v %q", err, p.Proto)
	}
	if dsn, err := genPostgresConfig(&DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, Host: "db", Port: 5411, User: "u"}); err != nil || !strings.Contains(dsn, "port=5411") {
		t.Errorf("genPostgresConfig without proto: %q %v", dsn, err)
	}
}
