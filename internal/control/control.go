// Package control is the CLI ↔ LSP-server protocol: a JSON-RPC 2.0 service on
// a per-workspace Unix socket, so `sqlh run`, `sqlh connect`, … act on the
// same connection, session and live buffers as the editor.
package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

const (
	MethodStatus      = "sqlh/status"
	MethodConnections = "sqlh/connections"
	MethodConnect     = "sqlh/connect"
	MethodRun         = "sqlh/run"
	MethodCancel      = "sqlh/cancel"
	MethodRefresh     = "sqlh/refresh"
)

// Connection describes one configured connection.
type Connection struct {
	Index   int    `json:"index"` // 1-based
	Alias   string `json:"alias"`
	Driver  string `json:"driver"`
	Desc    string `json:"desc"` // host/service/path, passwords masked
	Current bool   `json:"current"`
}

// Status describes a server.
type Status struct {
	Workspace  string      `json:"workspace"`
	Socket     string      `json:"socket"`
	PID        int         `json:"pid"`
	Connection *Connection `json:"connection,omitempty"`
	Connected  bool        `json:"connected"`
	Job        string      `json:"job,omitempty"` // running job, if any
	ResultsDir string      `json:"resultsDir"`
}

type ConnectParams struct {
	Alias string `json:"alias"`
}

// RunParams selects what to run and how. Exactly one source is used, in
// order: SQL, then File+Line.
type RunParams struct {
	SQL   string `json:"sql,omitempty"`
	File  string `json:"file,omitempty"` // absolute path
	Line  int    `json:"line,omitempty"` // 1-based
	Col   int    `json:"col,omitempty"`  // 1-based, optional
	Limit int    `json:"limit,omitempty"`
	All   bool   `json:"all,omitempty"`   // export: no row limit
	Out   string `json:"out,omitempty"`   // CSV path for the (first) result set
	Table bool   `json:"table,omitempty"` // return a text table instead of writing CSV
}

type StatementResult struct {
	Line      int    `json:"line"` // 1-based
	Kind      string `json:"kind"` // rows | executed | plsql
	Rows      int64  `json:"rows"`
	Truncated bool   `json:"truncated"`
	Affected  int64  `json:"affected"` // -1 when not DML
	File      string `json:"file,omitempty"`
	ElapsedMS int64  `json:"elapsedMs"`
}

type RunResult struct {
	Connection string            `json:"connection"`
	Summary    string            `json:"summary"`
	Statements []StatementResult `json:"statements"`
	Files      []string          `json:"files"`
	Table      string            `json:"table,omitempty"`
	ElapsedMS  int64             `json:"elapsedMs"`
}

// --- socket locations ------------------------------------------------------

// Dir holds sockets and their sidecar files. Unix socket paths are limited
// to ~104 bytes on macOS, so this must stay short.
func Dir() string {
	if d := os.Getenv("SQLH_RUNTIME_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "sqlh")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "sqlh")
	}
	return filepath.Join(os.TempDir(), "sqlh-"+fmt.Sprint(os.Getuid()))
}

// Canonical returns the absolute, symlink-resolved form of a directory, so
// that the editor's root (e.g. /tmp/x) and a CLI's cwd (/private/tmp/x on
// macOS) map to the same socket.
func Canonical(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir)
}

func key(workspace string) string {
	sum := sha256.Sum256([]byte(Canonical(workspace)))
	return hex.EncodeToString(sum[:])[:12]
}

// SocketPath is the socket of the server for a workspace root.
func SocketPath(workspace string) string { return filepath.Join(Dir(), key(workspace)+".sock") }

// InfoPath is the sidecar describing that server.
func InfoPath(workspace string) string { return filepath.Join(Dir(), key(workspace)+".json") }

// Info is the sidecar content.
type Info struct {
	Workspace string    `json:"workspace"`
	PID       int       `json:"pid"`
	Started   time.Time `json:"started"`
}

// ErrNoServer means no running server was found.
var ErrNoServer = errors.New("no running sqlh server for this workspace")

// Find locates the server for start (a directory): SQLH_SOCK, else the first
// ancestor of start that has a live socket. It returns the socket and that
// ancestor (the server's workspace root).
func Find(start string) (sock, workspace string, err error) {
	if s := os.Getenv("SQLH_SOCK"); s != "" {
		return s, "", nil
	}
	dir := Canonical(start)
	for {
		p := SocketPath(dir)
		if _, err := os.Stat(p); err == nil && alive(p) {
			return p, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", ErrNoServer
		}
		dir = parent
	}
}

func alive(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// maxSocketPath is sizeof(sun_path) on macOS (Linux: 108).
const maxSocketPath = 104

// Listen creates the socket for a workspace. If a live server already owns
// it, Listen returns an error (the first server wins); a stale socket left by
// a crashed server is replaced.
func Listen(workspace string) (net.Listener, error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	p := SocketPath(workspace)
	if len(p) >= maxSocketPath {
		return nil, fmt.Errorf("socket path too long (%d bytes, limit %d): set SQLH_RUNTIME_DIR to a shorter directory: %s", len(p), maxSocketPath-1, p)
	}
	if _, err := os.Stat(p); err == nil {
		if alive(p) {
			return nil, fmt.Errorf("another sqlh server already serves %s (%s)", workspace, p)
		}
		_ = os.Remove(p)
	}
	l, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(p, 0o600)
	info, _ := json.Marshal(Info{Workspace: workspace, PID: os.Getpid(), Started: time.Now()})
	_ = os.WriteFile(InfoPath(workspace), info, 0o600)
	return l, nil
}

// Cleanup removes a workspace's socket and sidecar.
func Cleanup(workspace string) {
	_ = os.Remove(SocketPath(workspace))
	_ = os.Remove(InfoPath(workspace))
}

// Servers lists running servers (live sockets with a sidecar).
func Servers() []Info {
	var out []Info
	files, _ := filepath.Glob(filepath.Join(Dir(), "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var info Info
		if json.Unmarshal(b, &info) != nil {
			continue
		}
		if alive(strings.TrimSuffix(f, ".json") + ".sock") {
			out = append(out, info)
		}
	}
	return out
}

// --- client ----------------------------------------------------------------

// Client talks to a server over its socket.
type Client struct {
	conn *jsonrpc2.Conn
}

func Dial(ctx context.Context, sock string) (*Client, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	conn := jsonrpc2.NewConn(context.Background(),
		jsonrpc2.NewBufferedStream(nc, jsonrpc2.VSCodeObjectCodec{}),
		jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) {
			return nil, nil
		}))
	return &Client{conn: conn}, nil
}

func (c *Client) Call(ctx context.Context, method string, params, result interface{}) error {
	err := c.conn.Call(ctx, method, params, result)
	var rpcErr *jsonrpc2.Error
	if errors.As(err, &rpcErr) {
		return errors.New(rpcErr.Message) // plain message for the CLI
	}
	return err
}

func (c *Client) Close() error { return c.conn.Close() }
