package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortDir: t.TempDir() paths exceed the 104-byte Unix socket limit on macOS.
func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "sqlh-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestFindThroughSymlinkAndSubdir(t *testing.T) {
	t.Setenv("SQLH_RUNTIME_DIR", shortDir(t))
	t.Setenv("SQLH_SOCK", "")
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(real, "a", "b")
	os.MkdirAll(sub, 0o755)

	if _, _, err := Find(sub); !errors.Is(err, ErrNoServer) {
		t.Fatalf("want ErrNoServer, got %v", err)
	}
	// server registered under the symlinked path, CLI starts in the real subdir
	l, err := Listen(link)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	sock, ws, err := Find(sub)
	if err != nil || sock != SocketPath(link) || ws != Canonical(real) {
		t.Fatalf("Find = %q %q %v", sock, ws, err)
	}
	// a second server for the same workspace is refused
	if _, err := Listen(real); err == nil {
		t.Fatal("second Listen succeeded")
	}
	if n := len(Servers()); n != 1 {
		t.Errorf("Servers() = %d", n)
	}
	// SQLH_SOCK overrides
	t.Setenv("SQLH_SOCK", "/x.sock")
	if s, _, _ := Find(sub); s != "/x.sock" {
		t.Errorf("SQLH_SOCK ignored: %s", s)
	}
	t.Setenv("SQLH_SOCK", "")
	l.Close()
	Cleanup(link)
	if _, _, err := Find(sub); !errors.Is(err, ErrNoServer) {
		t.Errorf("after cleanup: %v", err)
	}
	_ = context.Background()
}

func TestStaleSocketReplaced(t *testing.T) {
	t.Setenv("SQLH_RUNTIME_DIR", shortDir(t))
	ws := t.TempDir()
	os.MkdirAll(Dir(), 0o700)
	os.WriteFile(SocketPath(ws), nil, 0o600) // left behind by a crash
	l, err := Listen(ws)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	l.Close()
}

func TestSocketPathTooLong(t *testing.T) {
	long := "/tmp/" + strings.Repeat("x", 120)
	t.Setenv("SQLH_RUNTIME_DIR", long)
	defer os.RemoveAll(long)
	if _, err := Listen(t.TempDir()); err == nil || !strings.Contains(err.Error(), "SQLH_RUNTIME_DIR") {
		t.Errorf("want a clear error, got %v", err)
	}
}
