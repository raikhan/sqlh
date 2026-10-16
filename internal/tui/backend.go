// Package tui is sqlh's terminal UI: the panel next to the editor (`sqlh
// panel`), the same as an app (`sqlh tui`), and the inline picker. It talks
// to the workspace's server through a Backend, so it can be tested without a
// server, a database or a terminal multiplexer.
package tui

import (
	"github.com/raikhan/sqlh/internal/connedit"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

// Backend is what the panel needs from the outside world. Methods may block
// (they are called from Bubble Tea commands, never from Update).
type Backend interface {
	Catalog() (*control.CatalogResult, error)
	Connections() ([]control.Connection, error)
	Columns(table string) ([]control.ColumnItem, error)
	Relations(table string) (*control.RelationsResult, error)
	// AllColumns: every column of the catalog (column search).
	AllColumns() ([]control.ColumnRef, error)
	Context(c control.Cursor) (*control.ContextResult, error)
	DDL(table string) (*control.DDLResult, error)
	// Sample: rows of a table, or the most common values of a column.
	Sample(table, column string) (*control.SampleResult, error)
	// History: the versions of the statement at the cursor (nil: none) and
	// the workspace's recent runs.
	History(cur *control.Cursor) (*control.HistoryResult, error)
	// Versions: the versions of a query (a history lineage), newest first.
	Versions(lineage string) ([]history.Entry, error)
	// Open opens a result file in the viewer.
	Open(file string) error
	Connect(alias string) error
	Refresh() error
	// Act hands an action to the editor (insert at its cursor) and focuses it.
	Act(a control.PanelAction) error
	// Copy puts text on the clipboard.
	Copy(text string) error
	// BackToEditor focuses the editor's pane.
	BackToEditor() error
	// EditorAlive reports whether the panel's editor still runs; the panel
	// exits when it doesn't (sqlh tui: detaches it and stays).
	EditorAlive() bool
	// Editor describes the attached editor ("" = none: inserting copies).
	Editor() string

	// QueryFiles lists the query folder's files, newest first.
	QueryFiles() ([]string, error)
	// NewQuery creates the next query_<N>.sql with a connection header and
	// body, and returns its path.
	NewQuery(body string) (string, error)
	// OpenFile opens a file in the editor (sqlh tui: a new one if none).
	OpenFile(path string) error

	// ConnectionFields reads a connection of the config file.
	ConnectionFields(alias string) (connedit.Fields, error)
	// SaveConnection writes a connection to the config file: replaces old
	// (by alias), or adds one after `after` when old is "".
	SaveConnection(old string, f connedit.Fields, after string) error
	DeleteConnection(alias string) error
	// TestConnection connects with the fields and disconnects.
	TestConnection(f connedit.Fields) error
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
	// Keys: action → keys (the defaults for the rest).
	Keys map[string][]string
	// Cursor: the editor's cursor when the panel starts (may be nil).
	Cursor *control.Cursor
	// Workspace: the workspace root (titles).
	Workspace string
	// Standalone: `sqlh tui`: starts on the Objects tab and outlives
	// editors (else the panel starts on Statement and ends with its editor).
	Standalone bool
}
