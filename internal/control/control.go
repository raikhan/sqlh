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
	MethodSchemas     = "sqlh/schemas"
	MethodTables      = "sqlh/tables"
	MethodColumns     = "sqlh/columns"
	MethodDatabases   = "sqlh/databases"
	MethodUseDatabase = "sqlh/useDatabase"
	// MethodSchemaChoices lists the schemas `sqlh schema` can switch to
	// ([]SchemaItem, Default = current); MethodUseSchema switches.
	MethodSchemaChoices = "sqlh/schemaChoices"
	MethodUseSchema     = "sqlh/useSchema"
	MethodNotify        = "sqlh/notify"
	MethodRelations     = "sqlh/relations"
	MethodContext       = "sqlh/context"
)

// SchemaItem is a schema; Default marks the connection's current schema.
type SchemaItem struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// TableItem is a table or view. Insert is how to type it in SQL: unqualified
// in the default schema, quoted when needed.
type TableItem struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Insert  string `json:"insert"`
	Default bool   `json:"default"`
	Columns int    `json:"columns"`
}

type ColumnItem struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Null   string `json:"null"`
	Key    string `json:"key"`
	Insert string `json:"insert"`
}

type TablesParams struct {
	Schema string `json:"schema,omitempty"`
	All    bool   `json:"all,omitempty"` // include system schemas
}

type ColumnsParams struct {
	Table string `json:"table"` // table, schema.table, or a TableItem.Insert
}

type RelationsParams struct {
	Table string `json:"table"` // table, schema.table, or a TableItem.Insert
}

// RelationItem is one relationship of a table, seen from it.
type RelationItem struct {
	Table  string `json:"table"`  // the related table or view (database spelling)
	Insert string `json:"insert"` // how to type it
	Kind   string `json:"kind"`   // table | view | materialized view
	// Edge: fk (declared foreign key), view (a view exposes a base table's
	// relationship), maybe (guessed from column names), uses (view dependency).
	Edge   string `json:"edge"`
	Marker string `json:"marker"` // → this table references it, ← it references this table, ◇ view dependency
	Out    bool   `json:"out"`
	Maybe  bool   `json:"maybe,omitempty"`
	// Cols: join columns as (this table's column, its column), as typed in SQL.
	// Empty when the two can't be joined on columns (most view dependencies).
	Cols      [][2]string `json:"cols,omitempty"`
	Condition string      `json:"condition,omitempty"` // other.col = col [and …]
	Note      string      `json:"note,omitempty"`      // via orders, maybe, 1:1, nullable, reads, …
}

// RelationsResult describes a table or view and its relationships.
type RelationsResult struct {
	Table     string         `json:"table"`
	Schema    string         `json:"schema"`
	Insert    string         `json:"insert"`
	Kind      string         `json:"kind"`
	Relations []RelationItem `json:"relations"`
	// ViewsLoaded: view dependencies and lineage are included (they come with
	// the server's background load).
	ViewsLoaded bool   `json:"viewsLoaded"`
	Definition  string `json:"definition,omitempty"` // a view's query
	Writes      string `json:"writes,omitempty"`     // a view's: "read-only", "no INSERT", …
	Note        string `json:"note,omitempty"`
}

// ContextParams locates a cursor: 1-based line and column (Helix
// %{cursor_line}, %{cursor_column}).
type ContextParams struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// ScopeTable is a database table used by the statement at the cursor.
type ScopeTable struct {
	Name   string `json:"name"`
	Schema string `json:"schema,omitempty"`
	Alias  string `json:"alias,omitempty"`
	Ref    string `json:"ref"` // how the statement refers to it: alias, else the name
}

// ContextResult describes the statement at a cursor for the table picker.
type ContextResult struct {
	InJoin    bool         `json:"inJoin"` // the cursor is where a JOIN's table goes
	Tables    []ScopeTable `json:"tables"` // with InJoin: the tables before the cursor
	Focus     string       `json:"focus,omitempty"`
	NeedSpace bool         `json:"needSpace,omitempty"`
	Indent    string       `json:"indent,omitempty"`
	// settings
	LowercaseKeywords bool   `json:"lowercaseKeywords"`
	Insert            string `json:"insert"` // relations.insert: join | name
	Maybe             bool   `json:"maybe"`  // relations.maybe
}

type SchemasParams struct {
	All bool `json:"all,omitempty"`
}

// NotifyParams: a message for the editor's status line.
type NotifyParams struct {
	Message string `json:"message"`
	Error   bool   `json:"error,omitempty"`
}

// DatabaseParams: the database (MethodUseDatabase) or schema (MethodUseSchema)
// to switch to.
type DatabaseParams struct {
	Name string `json:"name"`
	File string `json:"file,omitempty"` // as ConnectParams.File
}

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
	// Database and Schema: the connection's current ones ("" if unknown).
	Database   string `json:"database,omitempty"`
	Schema     string `json:"schema,omitempty"`
	Job        string `json:"job,omitempty"` // running job, if any
	ResultsDir string `json:"resultsDir"`
	// Viewer: results.viewer, the command that opens result files.
	Viewer []string `json:"viewer,omitempty"`
}

type ConnectParams struct {
	Alias string `json:"alias"`
	// File: the editor buffer the switch came from; its connection header
	// is updated (absolute path, optional).
	File string `json:"file,omitempty"`
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
	Out   string `json:"out,omitempty"`   // CSV path for the first result set; later ones <stem>_N.csv
	Table bool   `json:"table,omitempty"` // return a text table instead of writing CSV
	// Dump: an export to Out (all rows) that may copy a complete result of
	// the same statement the server still has, unless Fresh.
	Dump  bool `json:"dump,omitempty"`
	Fresh bool `json:"fresh,omitempty"`
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
	// ReusedFrom: the dump copied this earlier result instead of running.
	ReusedFrom string `json:"reusedFrom,omitempty"`
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

// ViewsDir holds the files a workspace's server writes for the editor to open
// (view definitions). Find maps a directory under it back to that server.
func ViewsDir(workspace string) string { return filepath.Join(viewsRoot(), key(workspace)) }

func viewsRoot() string { return filepath.Join(Dir(), "views") }

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
	if sock, ws, ok := findForViews(dir); ok {
		return sock, ws, nil
	}
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

// findForViews: a directory under ViewsDir belongs to the server of the
// workspace it is keyed by (a view definition opened by go to definition is
// outside the workspace).
func findForViews(dir string) (sock, workspace string, ok bool) {
	rel, err := filepath.Rel(Canonical(viewsRoot()), dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", "", false
	}
	k := strings.SplitN(rel, string(filepath.Separator), 2)[0]
	sock = filepath.Join(Dir(), k+".sock")
	if !alive(sock) {
		return "", "", false
	}
	var info Info
	if b, err := os.ReadFile(filepath.Join(Dir(), k+".json")); err == nil {
		_ = json.Unmarshal(b, &info)
	}
	return sock, info.Workspace, true
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
		return &RemoteError{Message: rpcErr.Message} // plain message for the CLI
	}
	return err
}

// RemoteError is an error the server returned (as opposed to a failure to
// reach it).
type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return e.Message }

func (c *Client) Close() error { return c.conn.Close() }
