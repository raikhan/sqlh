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
	// Chains: the tables a chain of joins reaches from the statement's
	// tables (target ""), or the shortest chains to target.
	Chains(from []string, target string) (*control.ChainsResult, error)
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
	// RenameQuery renames a query file (name: relative to the queries
	// folder, .sql added without an extension); through the editor when it
	// has the file open. It returns the new path.
	RenameQuery(path, name string) (string, error)
	// EnsureFile creates a file with text unless it exists (created: it
	// didn't).
	EnsureFile(path, text string) (created bool, err error)
	// DeleteQuery deletes a query file (not one open in the editor).
	DeleteQuery(path string) error
	// FileText reads a query file (its preview; long ones cut).
	FileText(path string) (string, error)
	// CurrentVersion: the statement of a file most like an earlier run's
	// SQL (its live buffer), for the run's diff.
	CurrentVersion(file, sql string) (*control.CurrentVersionResult, error)

	// ConnectionFields reads a connection of the config file.
	ConnectionFields(alias string) (connedit.Fields, error)
	// SaveConnection writes a connection to the config file: replaces old
	// (by alias), or adds one after `after` when old is "".
	SaveConnection(old string, f connedit.Fields, after string) error
	DeleteConnection(alias string) error
	// TestConnection connects with the fields and disconnects.
	TestConnection(f connedit.Fields) error

	// ConfigPath: the config file the server reads.
	ConfigPath() (string, error)
	// ReloadSettings has the server re-read the config file and returns
	// the panel's settings from it.
	ReloadSettings() (*control.PanelSettings, error)
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
	// Keys: scope (config.PanelScopes) → action → keys (the defaults for
	// the rest).
	Keys map[string]map[string][]string
	// StatementCollapsed: the Statement tab's tables start collapsed.
	StatementCollapsed bool
	// ChainsAll: a join-chain result opens all its chains at once (else one
	// at a time).
	ChainsAll bool
	// Syntax: SQL colours by class (panel.syntax; defaults for the rest).
	Syntax map[string]string
	// Cursor: the editor's cursor when the panel starts (may be nil).
	Cursor *control.Cursor
	// Workspace: the workspace root (titles).
	Workspace string
	// Standalone: `sqlh tui`: starts on the Objects tab and outlives
	// editors (else the panel starts on Statement and ends with its editor).
	Standalone bool
}
