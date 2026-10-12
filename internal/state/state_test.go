package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConnectionRoundTripThroughSymlink(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "l")
	os.Symlink(real, link)
	if Connection(real) != "" {
		t.Fatal("unexpected state")
	}
	if err := SetConnection(link, "pg_local"); err != nil {
		t.Fatal(err)
	}
	SetConnection(t.TempDir(), "other")
	if got := Connection(real); got != "pg_local" {
		t.Errorf("Connection = %q", got)
	}
}
