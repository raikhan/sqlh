package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/control"
)

// Editor mode (--editor): the command was started from an editor keybinding
// (Helix :pipe-to / :insert-output), not a terminal. Helix 25.07 takes a
// failed command's stderr as its *output* — :pipe-to discards it,
// :insert-output inserts it into the buffer — so in editor mode nothing goes
// to stderr: errors are shown in the editor's status line through the
// workspace's server (`sqlh notify`), and the command exits 1 silently.
// Stdin is ignored too: :pipe-to sends the selection, which is whatever the
// last motion selected, not SQL to run.

// ErrReported: the error was shown in the editor; exit 1 without a message.
var ErrReported = errors.New("reported in the editor")

func editorFlag(usage string) cli.Flag {
	if usage == "" {
		usage = "started from an editor keybinding: errors in its status line, nothing on stderr"
	}
	return &cli.BoolFlag{Name: "editor", Usage: usage}
}

// wsFileFlag: the editor buffer a command acts for; its directory finds the
// workspace (Helix %{buffer_name}).
func wsFileFlag() cli.Flag {
	return &cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "find the workspace from `FILE`'s directory (the editor's buffer)"}
}

// editorAction wraps a command's action for --editor.
func editorAction(fn cli.ActionFunc) cli.ActionFunc {
	return func(c *cli.Context) error {
		if !c.Bool("editor") {
			return fn(c)
		}
		// Summaries and confirmations go to stderr; the server shows its own
		// in the editor. fzf draws on /dev/tty, not stderr.
		stderr := os.Stderr
		if null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
			os.Stderr = null
			defer func() { os.Stderr = stderr; null.Close() }()
		}
		err := fn(c)
		if err == nil || errors.Is(err, ErrReported) {
			return err
		}
		var shown *shownError
		if !errors.As(err, &shown) {
			notifyEditor(c, err.Error(), true)
		}
		// Without a server Helix reports "Shell command failed: status 1".
		return ErrReported
	}
}

// shownError: the server has already shown the error in the editor (failed
// queries report themselves).
type shownError struct{ err error }

func (e *shownError) Error() string { return e.err.Error() }
func (e *shownError) Unwrap() error { return e.err }

// notifyEditor shows msg in the editor's status line; false without a server.
func notifyEditor(c *cli.Context, msg string, isErr bool) bool {
	cl, _, err := server(c)
	if err != nil {
		return false
	}
	defer cl.Close()
	var ok string
	return call(cl, control.MethodNotify, control.NotifyParams{Message: msg, Error: isErr}, &ok) == nil
}

// --- viewer ----------------------------------------------------------------

// viewerCommand is results.viewer (the server's configuration, else the
// config file — also for a server predating results.viewer), default the
// OS opener.
func viewerCommand(c *cli.Context) []string {
	var v []string
	if cl, _, err := server(c); err == nil {
		var st control.Status
		if call(cl, control.MethodStatus, nil, &st) == nil {
			v = st.Viewer
		}
		cl.Close()
	}
	if len(v) == 0 {
		if cfg, err := loadConfig(c); err == nil {
			v = cfg.Results.Viewer
		}
	}
	if len(v) > 0 {
		return v
	}
	if runtime.GOOS == "darwin" {
		return []string{"open"}
	}
	return []string{"xdg-open"}
}

// viewerArgv builds the viewer's command line: "{}" arguments become the
// files, else they are appended; a leading "~" in the program is expanded.
func viewerArgv(viewer, files []string) []string {
	argv := []string{expandHome(viewer[0])}
	placed := false
	for _, a := range viewer[1:] {
		if a == "{}" {
			argv = append(argv, files...)
			placed = true
		} else {
			argv = append(argv, a)
		}
	}
	if !placed {
		argv = append(argv, files...)
	}
	return argv
}

// openFiles opens result files in the viewer. From a terminal it inherits
// the terminal (a viewer such as vd runs in it); otherwise it waits for the
// viewer to start and reports its output if it fails.
func openFiles(c *cli.Context, files []string) error {
	if len(files) == 0 {
		return nil
	}
	argv := viewerArgv(viewerCommand(c), files)
	cmd := exec.Command(argv[0], argv[1:]...)
	if isTerminal(os.Stdout) && !c.Bool("editor") {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.Join(strings.Fields(string(out)), " ")
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("viewer %s: %s", filepath.Base(argv[0]), msg)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// openCmd opens the latest result (or the given CSV files) in the viewer.
func openCmd(c *cli.Context) error {
	files := c.Args().Slice()
	if len(files) == 0 {
		p, err := latestPath(c)
		if err != nil {
			return err
		}
		files = []string{p}
	}
	return openFiles(c, files)
}
