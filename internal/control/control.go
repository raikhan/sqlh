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
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/history"
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
	// MethodDDL: a table's or view's CREATE statement(s) (DDLParams → DDLResult)
	MethodDDL       = "sqlh/ddl"
	MethodNotify    = "sqlh/notify"
	MethodRelations = "sqlh/relations"
	MethodContext   = "sqlh/context"
	// MethodCatalog: the objects of the current connection for the panel's
	// tree (CatalogResult).
	MethodCatalog = "sqlh/catalog"
	// MethodSubscribe keeps the connection as a subscriber: the server sends
	// EventNotification messages on it (SubscribeParams; a panel registers
	// the editor pane it belongs to).
	MethodSubscribe = "sqlh/subscribe"
	// MethodPanelFocus asks the panel of an editor pane to show a cursor and
	// take the focus (PanelFocusParams → PanelFocusResult).
	MethodPanelFocus = "sqlh/panelFocus"
	// MethodPanelAction stores what to insert into an editor pane;
	// MethodPanelTake hands it out once (`sqlh panel take`).
	MethodPanelAction = "sqlh/panelAction"
	MethodPanelTake   = "sqlh/panelTake"
	// MethodRelease asks a headless server (the app (`sqlh`) without an editor) to
	// give the socket to an editor's server starting for the workspace.
	MethodRelease = "sqlh/release"
	// MethodPanels lists the registered panels ([]PanelInfo).
	MethodPanels = "sqlh/panels"
	// MethodSample: a few rows of a table, or the most common values of a
	// column (SampleParams → SampleResult), on a session of their own.
	MethodSample = "sqlh/sample"
	// MethodHistory: past runs, the versions of the statement at a cursor
	// first (HistoryParams → HistoryResult).
	MethodHistory = "sqlh/history"
	// MethodReplaceStatement replaces the statement at a cursor in the
	// editor's buffer (ReplaceParams; a workspace edit, one undo step).
	MethodReplaceStatement = "sqlh/replaceStatement"
	// MethodAllColumns lists every column of the catalog ([]ColumnRef): the
	// panel's column search.
	MethodAllColumns = "sqlh/allColumns"
	// MethodReloadConfig re-reads the config file (the panel edited it).
	MethodReloadConfig = "sqlh/reloadConfig"
	// MethodChains: the tables a chain of joins reaches from the statement's
	// tables, or the shortest chains to one (ChainsParams → ChainsResult).
	MethodChains = "sqlh/chains"
	// MethodFileOpen tells whether the editor has a file open (FileParams →
	// bool).
	MethodFileOpen = "sqlh/fileOpen"
	// MethodCurrentVersion finds today's version of a run's statement in its
	// file (CurrentVersionParams → CurrentVersionResult): the panel's diff.
	MethodCurrentVersion = "sqlh/currentVersion"

	// EventNotification is the server → subscriber notification (Event).
	EventNotification = "sqlh/event"
)

// Event kinds.
const (
	EventCache      = "cache"      // the schema cache changed (background load, refresh)
	EventConnection = "connection" // connection switched
	EventConfig     = "config"     // the config file changed (connections)
	EventCursor     = "cursor"     // the editor's cursor moved (an edit or a request)
	EventFocus      = "focus"      // `sqlh panel` asked this panel to show a cursor and focus itself
	EventRun        = "run"        // a query finished
	EventHistory    = "history"    // a run was added to the history
)

// Event is a server notification to subscribers.
type Event struct {
	Kind       string  `json:"kind"`
	Generation int64   `json:"generation"` // the catalog generation (changes with cache/connection)
	Cursor     *Cursor `json:"cursor,omitempty"`
	Message    string  `json:"message,omitempty"`
}

// Cursor is a position in a file: 1-based line and column.
type Cursor struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// SubscribeParams: who subscribes. Pane is the subscriber's Herdr pane,
// EditorPane the editor pane it belongs to (panels).
type SubscribeParams struct {
	Pane       string `json:"pane,omitempty"`
	EditorPane string `json:"editorPane,omitempty"`
}

// SubscribeResult: the current cursor, if known.
type SubscribeResult struct {
	Generation int64   `json:"generation"`
	Cursor     *Cursor `json:"cursor,omitempty"`
}

// PanelInfo is a registered panel.
type PanelInfo struct {
	Pane       string `json:"pane"`
	EditorPane string `json:"editorPane"`
}

type PanelFocusParams struct {
	EditorPane string  `json:"editorPane"`
	Cursor     *Cursor `json:"cursor,omitempty"`
}

type PanelFocusResult struct {
	Found bool   `json:"found"`
	Pane  string `json:"pane,omitempty"`
}

// JoinHop is one step of a path through relationships: Table joined to the
// previous table (or the anchor) on Cols (previous table's column, Table's
// column), as typed.
type JoinHop struct {
	Table string      `json:"table"`
	Cols  [][2]string `json:"cols,omitempty"`
}

// Panel action kinds.
const (
	ActionTable   = "table"   // a table or view: name, or a join after JOIN
	ActionColumns = "columns" // columns of a table, qualified by its reference in the statement
	ActionText    = "text"    // literal text
	ActionReplace = "replace" // replace the statement at the cursor with Text
)

// PanelAction is what the panel chose to insert; `sqlh panel take` turns it
// into text at the editor's cursor.
type PanelAction struct {
	Kind  string `json:"kind"`
	Table string `json:"table,omitempty"` // as typed
	// Anchor and Path: Table was reached from Anchor through Path (its last
	// hop is Table); after JOIN with Anchor in the statement the whole chain
	// is inserted.
	Anchor  string    `json:"anchor,omitempty"`
	Path    []JoinHop `json:"path,omitempty"`
	Columns []string  `json:"columns,omitempty"` // as typed
	Text    string    `json:"text,omitempty"`
	// Mode: "" (relations.insert), name or join.
	Mode string `json:"mode,omitempty"`
	// Copy: put the text on the clipboard instead of inserting it.
	Copy bool `json:"copy,omitempty"`
}

type PanelActionParams struct {
	EditorPane string      `json:"editorPane"`
	Action     PanelAction `json:"action"`
}

type PanelTakeParams struct {
	EditorPane string `json:"editorPane"`
}

// PanelTakeResult: the pending action, if any (Found false: none, or expired).
type PanelTakeResult struct {
	Found  bool        `json:"found"`
	Action PanelAction `json:"action"`
}

// HistoryParams: with File and Line, the versions of the statement there
// (and that file's other runs); without, the workspace's recent runs.
type HistoryParams struct {
	// Lineage: the versions of this lineage instead of the cursor's statement.
	Lineage string `json:"lineage,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Col     int    `json:"col,omitempty"`
	Limit   int    `json:"limit,omitempty"` // recent runs (default 50)
}

// ChainsParams: the statement's tables (as typed); Target "" lists the
// reachable tables, else the chains to Target.
type ChainsParams struct {
	From   []string `json:"from"`
	Target string   `json:"target,omitempty"`
	// MaxDepth: at most this many joins (0: relations.chainDepth).
	MaxDepth int `json:"maxDepth,omitempty"`
	Limit    int `json:"limit,omitempty"` // 0: relations.chainLimit
}

// ChainTarget: a reachable table: Joins away at the nearest, by Chains
// chains (counted up to the limit; More: there are more).
type ChainTarget struct {
	Table  string `json:"table"`
	Kind   string `json:"kind,omitempty"`
	Joins  int    `json:"joins"`
	Chains int    `json:"chains"`
	More   bool   `json:"more,omitempty"`
}

// Chain: a join chain from a statement table.
type Chain struct {
	Anchor string    `json:"anchor"`
	Hops   []JoinHop `json:"hops"`
}

type ChainsResult struct {
	Targets []ChainTarget `json:"targets"`
	Chains  []Chain       `json:"chains"`
	More    bool          `json:"more,omitempty"` // chains past the limit
}

// FileParams: a file (absolute path).
type FileParams struct {
	File string `json:"file"`
}

// CurrentVersionParams: a file and the SQL of an earlier run of it.
type CurrentVersionParams struct {
	File string `json:"file"`
	SQL  string `json:"sql"`
}

// CurrentVersionResult: the file's statement most like the SQL ("" = none
// similar enough), and its line (1-based).
type CurrentVersionResult struct {
	Statement string `json:"statement,omitempty"`
	Line      int    `json:"line,omitempty"`
}

// HistoryResult: Statement is the text at the cursor; Versions its earlier
// runs (distinct SQL, newest first); Recent the workspace's latest runs.
type HistoryResult struct {
	Statement string          `json:"statement,omitempty"`
	Versions  []history.Entry `json:"versions"`
	Recent    []history.Entry `json:"recent"`
}

// ReplaceParams: the statement at File:Line:Col becomes Text.
type ReplaceParams struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Text string `json:"text"`
}

// SampleParams: rows of Table (as typed), or with Column the most common
// values of that column.
type SampleParams struct {
	Table  string `json:"table"`
	Column string `json:"column,omitempty"`
	Limit  int    `json:"limit,omitempty"` // default 50 rows / 10 values
}

// SampleResult: the rows; nil cells are NULL. With Column the columns are
// the value and its count.
type SampleResult struct {
	Table     string      `json:"table"`
	Columns   []string    `json:"columns"`
	Types     []string    `json:"types"`
	Rows      [][]*string `json:"rows"`
	Truncated bool        `json:"truncated"` // more rows than the limit
	SQL       string      `json:"sql"`
	ElapsedMS int64       `json:"elapsedMs"`
}

// CatalogObject is an object of a schema. Kind: table, view, materialized
// view ("" for a table or view of another schema, where they aren't told
// apart), sequence, type, index, constraint, trigger.
type CatalogObject struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Insert string `json:"insert"`          // how to type it
	Table  string `json:"table,omitempty"` // indexes, constraints, triggers: their table
	Detail string `json:"detail,omitempty"`
	// Columns: a table's or view's column count (0: not loaded yet)
	Columns int `json:"columns,omitempty"`
}

// CatalogSchema is a schema and its objects.
type CatalogSchema struct {
	Name    string          `json:"name"`
	Default bool            `json:"default"`
	Objects []CatalogObject `json:"objects"`
}

// CatalogResult is the panel's tree: the current connection's schemas and
// their objects.
type CatalogResult struct {
	Connection  string `json:"connection"`
	Driver      string `json:"driver"`
	Environment string `json:"environment"`
	Database    string `json:"database,omitempty"`
	Schema      string `json:"schema,omitempty"`
	Generation  int64  `json:"generation"`
	// SamplePolicy: auto, manual or off (the connection's sample setting).
	SamplePolicy string `json:"samplePolicy"`
	// ViewsLoaded / ObjectsLoaded: the background load is done (view
	// kinds and lineage; indexes, sequences, …).
	ViewsLoaded   bool            `json:"viewsLoaded"`
	ObjectsLoaded bool            `json:"objectsLoaded"`
	Schemas       []CatalogSchema `json:"schemas"`
}

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

// DDLParams names a table or view: name, schema.name (quoted parts kept).
type DDLParams struct {
	Table string `json:"table"`
}

// DDLResult is a table's or view's definition.
type DDLResult struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`             // table | view | materialized view
	Source string `json:"source,omitempty"` // DBMS_METADATA, pg_catalog, …
	Note   string `json:"note,omitempty"`
	DDL    string `json:"ddl"`
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
	// Environment: dev, test or prod.
	Environment string `json:"environment,omitempty"`
	// Group: the connection's folder in the panel ("" = top level).
	Group string `json:"group,omitempty"`
	// Database and Schema the connection works in ("" = the login's).
	Database string `json:"database,omitempty"`
	Schema   string `json:"schema,omitempty"`
	// Fields: the connection part by part, passwords masked.
	Fields []Field `json:"fields,omitempty"`
	// Sample: the connection's sample policy (auto, manual, off).
	Sample string `json:"sample,omitempty"`
}

// Field is a labelled value.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ColumnRef is a column of the catalog, for search.
type ColumnRef struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	TableInsert string `json:"tableInsert"`
	Name        string `json:"name"`
	Insert      string `json:"insert"`
	Type        string `json:"type"`
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
	// Panel: the server's panel settings (defaults applied).
	Panel *PanelSettings `json:"panel,omitempty"`
}

// PanelSettings are the panel's settings from the server's configuration.
type PanelSettings struct {
	Placement  string   `json:"placement"`
	InsertKeys []string `json:"insertKeys"`
	// Keys: the panel's keymap per scope (connections, objects, statement,
	// queries, history, search, form): action → keys, defaults merged.
	Keys map[string]map[string][]string `json:"keys"`
	// StatementCollapsed: the Statement tab's tables start collapsed.
	StatementCollapsed bool `json:"statementCollapsed,omitempty"`
	// ChainsAll: a join-chain result opens all its chains at once.
	ChainsAll bool `json:"chainsAll,omitempty"`
	// Syntax: SQL colours by class (panel.syntax).
	Syntax     map[string]string `json:"syntax,omitempty"`
	Icons      string            `json:"icons"`
	QueriesDir string            `json:"queriesDir"` // resolved
	Editor     []string          `json:"editor"`     // tui.editor
	// ConfigPath: the config file the server reads ("" = not from a file):
	// the panel edits connections there.
	ConfigPath string `json:"configPath,omitempty"`
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
	SQL  string `json:"sql,omitempty"`
	File string `json:"file,omitempty"` // absolute path
	Line int    `json:"line,omitempty"` // 1-based
	Col  int    `json:"col,omitempty"`  // 1-based, optional
	// EndLine > Line: the statements on lines Line..EndLine (a selection)
	// instead of the one at Line.
	EndLine int    `json:"endLine,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	All     bool   `json:"all,omitempty"`   // export: no row limit
	Out     string `json:"out,omitempty"`   // CSV path for the first result set; later ones <stem>_N.csv
	Table   bool   `json:"table,omitempty"` // return a text table instead of writing CSV
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
	// Headless: started by the app (`sqlh`) without an editor; it gives way to an
	// editor's server (MethodRelease).
	Headless bool `json:"headless,omitempty"`
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
func Listen(workspace string) (net.Listener, error) { return listen(workspace, false) }

// ListenHeadless is Listen for a headless server: it never takes the socket
// from another server, and gives it to an editor's server on request.
func ListenHeadless(workspace string) (net.Listener, error) { return listen(workspace, true) }

func listen(workspace string, headless bool) (net.Listener, error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	p := SocketPath(workspace)
	if len(p) >= maxSocketPath {
		return nil, fmt.Errorf("socket path too long (%d bytes, limit %d): set SQLH_RUNTIME_DIR to a shorter directory: %s", len(p), maxSocketPath-1, p)
	}
	if _, err := os.Stat(p); err == nil {
		if alive(p) && !headless && isHeadless(workspace) {
			release(p)
		}
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
	info, _ := json.Marshal(Info{Workspace: workspace, PID: os.Getpid(), Started: time.Now(), Headless: headless})
	_ = os.WriteFile(InfoPath(workspace), info, 0o600)
	return l, nil
}

func isHeadless(workspace string) bool {
	var info Info
	b, err := os.ReadFile(InfoPath(workspace))
	return err == nil && json.Unmarshal(b, &info) == nil && info.Headless
}

// release asks the headless server on sock to stop serving it, and waits.
func release(sock string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if cl, err := Dial(ctx, sock); err == nil {
		var ok string
		_ = cl.Call(ctx, MethodRelease, nil, &ok)
		cl.Close()
	}
	for i := 0; i < 30 && alive(sock); i++ {
		time.Sleep(100 * time.Millisecond)
	}
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
	return DialEvents(ctx, sock, nil)
}

// DialEvents is Dial, passing the server's notifications (events after
// MethodSubscribe) to onEvent, in order.
func DialEvents(ctx context.Context, sock string, onEvent func(Event)) (*Client, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	conn := jsonrpc2.NewConn(context.Background(),
		jsonrpc2.NewBufferedStream(nc, jsonrpc2.VSCodeObjectCodec{}),
		jsonrpc2.HandlerWithError(func(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
			if onEvent != nil && req.Notif && req.Method == EventNotification && req.Params != nil {
				var ev Event
				if json.Unmarshal(*req.Params, &ev) == nil {
					onEvent(ev)
				}
			}
			return nil, nil
		}),
		jsonrpc2.SetLogger(log.Default())) // not straight to stderr: the panel owns the terminal
	return &Client{conn: conn}, nil
}

// Done is closed when the connection to the server is gone.
func (c *Client) Done() <-chan struct{} { return c.conn.DisconnectNotify() }

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
