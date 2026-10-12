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
