package database

import (
	"strings"
	"testing"
)

func TestResolveSecrets(t *testing.T) {
	t.Setenv("SQLH_TEST_USER", "scott")
	t.Setenv("SQLH_TEST_PW", "tiger")

	c := &DBConfig{Alias: "a", User: "${SQLH_TEST_USER}", Passwd: "${SQLH_TEST_PW}", DataSourceName: "u=${SQLH_TEST_USER}"}
	r, err := c.ResolveSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if r.User != "scott" || r.Passwd != "tiger" || r.DataSourceName != "u=scott" {
		t.Errorf("got %+v", r)
	}
	if c.Passwd != "${SQLH_TEST_PW}" {
		t.Error("original config mutated")
	}

	r, err = (&DBConfig{Alias: "b", PasswdCmd: "printf 'from-cmd\\n'"}).ResolveSecrets()
	if err != nil || r.Passwd != "from-cmd" {
		t.Errorf("passwdCmd: %q %v", r.Passwd, err)
	}

	// passwd takes precedence; the command must not run
	r, err = (&DBConfig{Alias: "c", Passwd: "x", PasswdCmd: "exit 1"}).ResolveSecrets()
	if err != nil || r.Passwd != "x" {
		t.Errorf("precedence: %v", err)
	}

	_, err = (&DBConfig{Alias: "d", PasswdCmd: "echo nope >&2; exit 3"}).ResolveSecrets()
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected stderr in error, got %v", err)
	}

	// bare $NAME is literal
	r, err = (&DBConfig{Alias: "f", Passwd: "pa$word"}).ResolveSecrets()
	if err != nil || r.Passwd != "pa$word" {
		t.Errorf("literal $: %q %v", r.Passwd, err)
	}

	_, err = (&DBConfig{Alias: "e", Passwd: "${SQLH_TEST_UNSET_VAR}"}).ResolveSecrets()
	if err == nil || !strings.Contains(err.Error(), "SQLH_TEST_UNSET_VAR") {
		t.Errorf("expected missing env error, got %v", err)
	}
}

func TestValidateOracle(t *testing.T) {
	ok := []*DBConfig{
		{Driver: "oracle", Host: "h", DBName: "svc", User: "u", PasswdCmd: "echo x"}, // no proto/port
		{Driver: "oracle", Host: "h", Port: 1522, DBName: "svc", User: "u", Passwd: "p"},
		{Driver: "oracle", DataSourceName: "oracle://u:p@h:1521/svc"},
	}
	for i, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := []*DBConfig{
		{Driver: "oracle", DBName: "svc", User: "u", Passwd: "p"}, // no host
		{Driver: "oracle", Host: "h", User: "u", Passwd: "p"},     // no service
		{Driver: "oracle", Host: "h", DBName: "svc", Passwd: "p"}, // no user
		{Driver: "oracle", Host: "h", DBName: "svc", User: "u"},   // no password source
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("bad[%d]: expected error", i)
		}
	}
}

func TestPgDSNWithDB(t *testing.T) {
	cases := map[string]string{
		"host=localhost dbname=sdsai sslmode=disable": "host=localhost dbname=sdsai sslmode=disable dbname=postgres",
		"postgres://u:p@h:5432/sdsai?sslmode=disable": "postgres://u:p@h:5432/postgres?sslmode=disable",
		"postgresql://h/sdsai":                        "postgresql://h/postgres",
	}
	for in, want := range cases {
		if got, err := pgDSNWithDB(in, "postgres"); err != nil || got != want {
			t.Errorf("pgDSNWithDB(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, _ := pgDSNWithDB("host=h", "my db"); got != "host=h dbname='my db'" {
		t.Errorf("quoting: %q", got)
	}
}
