// Package herdr talks to the Herdr terminal multiplexer through its socket
// API ($HERDR_SOCKET_PATH, newline-delimited JSON requests), which unlike the
// CLI can focus an exact pane. Herdr IDs are server-local: callers validate
// targets before sending input and never fall back to another pane.
package herdr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

// ErrNotInHerdr: not running inside Herdr.
var ErrNotInHerdr = errors.New("not running inside Herdr")

// Client is a Herdr socket client.
type Client struct {
	Socket string
	// Timeout per request (default 5 s).
	Timeout time.Duration
	// NoFocus: never move the focus (SQLH_NO_FOCUS=1, for scripted tests
	// next to someone working).
	NoFocus bool
}

// FromEnv returns a client for the Herdr session this process runs in.
func FromEnv() (*Client, error) {
	sock := os.Getenv("HERDR_SOCKET_PATH")
	if os.Getenv("HERDR_ENV") != "1" || sock == "" {
		return nil, ErrNotInHerdr
	}
	return &Client{Socket: sock, NoFocus: os.Getenv("SQLH_NO_FOCUS") == "1"}, nil
}

// CurrentPane is this process's pane ($HERDR_PANE_ID, "" outside Herdr).
func CurrentPane() string {
	if os.Getenv("HERDR_ENV") != "1" {
		return ""
	}
	return os.Getenv("HERDR_PANE_ID")
}

// Error is an error response.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return "herdr: " + e.Message }

// NotFound reports a pane_not_found (or similar) error.
func NotFound(err error) bool {
	var he *Error
	return errors.As(err, &he) && (he.Code == "pane_not_found" || he.Code == "not_found")
}

var seq atomic.Int64

// Call sends one request and decodes its result.
func (c *Client) Call(method string, params, result any) error {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	conn, err := net.DialTimeout("unix", c.Socket, timeout)
	if err != nil {
		return fmt.Errorf("herdr: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	id := "sqlh-" + strconv.FormatInt(seq.Add(1), 10)
	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("herdr: %w", err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return fmt.Errorf("herdr %s: %w", method, err)
	}
	var resp struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("herdr %s: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result != nil {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

// Pane describes a pane.
type Pane struct {
	ID          string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Focused     bool   `json:"focused"`
	Cwd         string `json:"cwd"`
}

// Pane returns a pane's description.
func (c *Client) Pane(id string) (*Pane, error) {
	var res struct {
		Pane Pane `json:"pane"`
	}
	if err := c.Call("pane.get", map[string]any{"pane_id": id}, &res); err != nil {
		return nil, err
	}
	return &res.Pane, nil
}

// Process is a foreground process of a pane.
type Process struct {
	PID   int      `json:"pid"`
	Name  string   `json:"name"`
	Argv0 string   `json:"argv0"`
	Argv  []string `json:"argv"`
}

// ProcessInfo describes a pane's foreground processes.
type ProcessInfo struct {
	ShellPID  int       `json:"shell_pid"`
	Group     int       `json:"foreground_process_group_id"`
	Processes []Process `json:"foreground_processes"`
}

// ProcessInfo returns a pane's foreground processes.
func (c *Client) ProcessInfo(id string) (*ProcessInfo, error) {
	var res struct {
		Info ProcessInfo `json:"process_info"`
	}
	if err := c.Call("pane.process_info", map[string]any{"pane_id": id}, &res); err != nil {
		return nil, err
	}
	return &res.Info, nil
}

// HasProcess reports whether a pane's foreground runs a program named name
// (pid > 0: that exact process).
func (p *ProcessInfo) HasProcess(name string, pid int) bool {
	for _, pr := range p.Processes {
		if pid > 0 && pr.PID != pid {
			continue
		}
		names := []string{pr.Name, pr.Argv0}
		if len(pr.Argv) > 0 {
			names = append(names, pr.Argv[0])
		}
		for _, n := range names {
			if n != "" && filepath.Base(n) == name {
				return true
			}
		}
	}
	return false
}

// SplitOptions: a new pane next to Target.
type SplitOptions struct {
	Target    string
	Direction string  // right or down
	Ratio     float64 // the share kept by Target (0: Herdr's default)
	Cwd       string
	Env       map[string]string
	Focus     bool
}

// Split creates a pane and returns its ID.
func (c *Client) Split(o SplitOptions) (string, error) {
	params := map[string]any{"target_pane_id": o.Target, "direction": o.Direction, "focus": o.Focus && !c.NoFocus}
	if o.Ratio > 0 {
		params["ratio"] = o.Ratio
	}
	if o.Cwd != "" {
		params["cwd"] = o.Cwd
	}
	if c.NoFocus {
		env := map[string]string{"SQLH_NO_FOCUS": "1"}
		for k, v := range o.Env {
			env[k] = v
		}
		o.Env = env
	}
	if len(o.Env) > 0 {
		params["env"] = o.Env
	}
	var res struct {
		Pane Pane `json:"pane"`
	}
	if err := c.Call("pane.split", params, &res); err != nil {
		return "", err
	}
	return res.Pane.ID, nil
}

// Swap exchanges two panes' places.
func (c *Client) Swap(source, target string) error {
	return c.Call("pane.swap", map[string]any{"source_pane_id": source, "target_pane_id": target}, nil)
}

// Focus focuses a pane.
func (c *Client) Focus(id string) error {
	if c.NoFocus {
		return nil
	}
	return c.Call("pane.focus", map[string]any{"pane_id": id}, nil)
}

// SendKeys sends key presses (Herdr key names: esc, enter, space, a, …).
func (c *Client) SendKeys(id string, keys ...string) error {
	return c.Call("pane.send_keys", map[string]any{"pane_id": id, "keys": keys}, nil)
}

// SendText sends literal text (as a paste).
func (c *Client) SendText(id, text string) error {
	return c.Call("pane.send_text", map[string]any{"pane_id": id, "text": text}, nil)
}

// Close closes a pane.
func (c *Client) Close(id string) error {
	return c.Call("pane.close", map[string]any{"pane_id": id}, nil)
}
