package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNextQueryNumber(t *testing.T) {
	dir := t.TempDir()
	if n := nextQueryNumber(dir); n != 1 {
		t.Errorf("empty: %d", n)
	}
	if n := nextQueryNumber(filepath.Join(dir, "missing")); n != 1 {
		t.Errorf("missing: %d", n)
	}
	for _, f := range []string{"query_3.sql", "query_x.sql", "report.sql", "query_10.sql.bak", "Query_20.sql"} {
		_ = os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	if n := nextQueryNumber(dir); n != 4 {
		t.Errorf("after query_3: %d", n)
	}
}

func TestHelixQuote(t *testing.T) {
	if got := helixQuote("/w/it's.sql"); got != "'/w/it''s.sql'" {
		t.Errorf("got %s", got)
	}
}
