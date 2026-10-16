// Package tui is sqlh's terminal UI: the side panel next to the editor
// (object tree, previews) and, later, the standalone app and inline pickers.
// It talks to the workspace's server through a Backend, so it can be tested
// without a server, a database or a terminal multiplexer.
package tui

import "github.com/raikhan/sqlh/internal/control"

// Backend is what the panel needs from the outside world. Methods may block
// (they are called from Bubble Tea commands, never from Update).
type Backend interface {
	Catalog() (*control.CatalogResult, error)
	Connections() ([]control.Connection, error)
	Columns(table string) ([]control.ColumnItem, error)
	Relations(table string) (*control.RelationsResult, error)
	Context(c control.Cursor) (*control.ContextResult, error)
	DDL(table string) (*control.DDLResult, error)
	// Sample: rows of a table, or the most common values of a column.
	Sample(table, column string) (*control.SampleResult, error)
	// History: the versions of the statement at the cursor (nil: none) and
	// the workspace's recent runs.
	History(cur *control.Cursor) (*control.HistoryResult, error)
	// Open opens a result file in the viewer.
	Open(file string) error
	Connect(alias string) error
	UseSchema(name string) error
	Refresh() error
	// Act hands an action to the editor (insert at its cursor) and focuses it.
	Act(a control.PanelAction) error
	// Copy puts text on the clipboard.
	Copy(text string) error
	// BackToEditor focuses the editor's pane.
	BackToEditor() error
	// EditorAlive reports whether the panel's editor still runs; the panel
	// exits when it doesn't.
	EditorAlive() bool
}

// Workspace is what `sqlh tui` adds to a Backend: query files and an editor
// it opens them in.
type Workspace interface {
	// QueryFiles lists the query folder's files, newest first.
	QueryFiles() ([]string, error)
	// NewQuery creates the next query_<N>.sql with a connection header and
	// body, and returns its path.
	NewQuery(body string) (string, error)
	// OpenFile opens a file in the attached editor, or a new editor pane.
	OpenFile(path string) error
	// Editor describes the attached editor ("" = none).
	Editor() string
}

// Synthetic event kinds the backend sends besides the server's.
const (
	EventDisconnected = "disconnected" // lost the server; reconnecting
	EventReconnected  = "reconnected"
)

// Options configure the panel.
type Options struct {
	// Icons: unicode (default), nerd or ascii.
	Icons string
	// BackToEditorKey: Bubble Tea key name (default backspace).
	BackToEditorKey string
	// Cursor: the editor's cursor when the panel starts (may be nil).
	Cursor *control.Cursor
	// Title: shown when there is no connection yet.
	Workspace string
	// Standalone: `sqlh tui` (query files, new queries, an editor attached
	// on demand); the Backend implements Workspace.
	Standalone bool
}
