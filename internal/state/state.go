// Package state remembers per-workspace choices (the current connection)
// across editor restarts, in $XDG_STATE_HOME/sqlh/state.json
// (~/.local/state/sqlh/state.json).
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type workspaceState struct {
	Connection string `json:"connection,omitempty"`
}

type file struct {
	Workspaces map[string]workspaceState `json:"workspaces"`
}

// Path is the state file.
func Path() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "sqlh", "state.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "sqlh", "state.json")
}

func load() file {
	f := file{Workspaces: map[string]workspaceState{}}
	if b, err := os.ReadFile(Path()); err == nil {
		_ = json.Unmarshal(b, &f)
		if f.Workspaces == nil {
			f.Workspaces = map[string]workspaceState{}
		}
	}
	return f
}

// norm: absolute and symlink-resolved, so /tmp/x and /private/tmp/x match.
func norm(workspace string) string {
	if abs, err := filepath.Abs(workspace); err == nil {
		workspace = abs
	}
	if real, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = real
	}
	return filepath.Clean(workspace)
}

// Connection returns the remembered connection alias for a workspace.
func Connection(workspace string) string {
	if workspace == "" {
		return ""
	}
	return load().Workspaces[norm(workspace)].Connection
}

// SetConnection remembers the connection alias for a workspace.
func SetConnection(workspace, alias string) error {
	if workspace == "" {
		return nil
	}
	f := load()
	ws := f.Workspaces[norm(workspace)]
	ws.Connection = alias
	f.Workspaces[norm(workspace)] = ws
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		return err
	}
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, Path())
}
