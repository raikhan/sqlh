// Package cmd implements the sqlh CLI subcommands. With a running server for
// the workspace (the editor's LSP process) they act through its control
// socket; otherwise run/export/connections/latest work standalone from the
// config file.
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/control"
)

// headerFileFlag: the editor buffer a switch comes from; the server updates
// its connection header.
var headerFileFlag = &cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "update the connection header of `FILE` (if open in the editor)"}

// Commands returns the CLI subcommands.
func Commands() []*cli.Command {
	dirFlag := &cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "find the workspace from `DIR` (default: --file's directory, else cwd)"}
	jsonFlag := &cli.BoolFlag{Name: "json", Usage: "machine-readable output"}
	runFlags := []cli.Flag{
		&cli.StringFlag{Name: "sql", Aliases: []string{"e"}, Usage: "SQL to run (default: stdin when piped, if longer than one character)"},
		&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "SQL file; with --line runs the statement at that line (the editor's unsaved buffer if open)"},
		&cli.IntFlag{Name: "line", Aliases: []string{"l"}, Usage: "1-based line in --file (e.g. Helix %{cursor_line})"},
		&cli.IntFlag{Name: "col", Usage: "1-based column in --file"},
		&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "write the CSV to `PATH` instead of the results directory"},
		&cli.BoolFlag{Name: "table", Aliases: []string{"t"}, Usage: "print a text table instead of writing CSV"},
		&cli.StringFlag{Name: "conn", Usage: "run standalone on connection `ALIAS` (does not change the editor's connection)"},
		&cli.StringFlag{Name: "schema", Usage: "run standalone with current schema `NAME`, one of the connection's schemas: (the editor's: sqlh schema)"},
		&cli.BoolFlag{Name: "standalone", Usage: "do not use a running server"},
		&cli.BoolFlag{Name: "open", Usage: "open the CSV in results.viewer when done"},
		&cli.BoolFlag{Name: "bg", Usage: "return at once; the query continues in a detached process (reports in the editor)"},
		editorFlag("started from an editor keybinding: ignore stdin; errors in its status line, nothing on stderr"),
		dirFlag, jsonFlag,
	}
	return append([]*cli.Command{
		{
			Name:      "run",
			Usage:     "run SQL → CSV (first N rows); prints the CSV path(s)",
			ArgsUsage: " ",
			Flags: append([]cli.Flag{
				&cli.StringFlag{Name: "limit", Aliases: []string{"n"}, Usage: "rows to fetch: a number (default: results.previewLimit, 100), `all` or ask (pick with fzf)"},
			}, runFlags...),
			Action: editorAction(func(c *cli.Context) error { return runCmd(c, false) }),
		},
		{
			Name:      "export",
			Usage:     "run SQL → CSV (all rows); prints the CSV path(s)",
			ArgsUsage: " ",
			Flags:     runFlags,
			Action:    editorAction(func(c *cli.Context) error { return runCmd(c, true) }),
		},
		dumpCommand(dirFlag),
		{
			Name:      "connect",
			Usage:     "switch the editor's connection (no alias: pick with fzf)",
			ArgsUsage: "[alias]",
			Flags:     []cli.Flag{dirFlag, headerFileFlag, editorFlag("")},
			Action:    editorAction(connectCmd),
		},
		{
			Name:    "connections",
			Aliases: []string{"ls"},
			Usage:   "list connections (* = current)",
			Flags:   []cli.Flag{dirFlag, jsonFlag},
			Action:  connectionsCmd,
		},
		{
			Name:   "status",
			Usage:  "show the workspace's server: connection, running query, results dir",
			Flags:  []cli.Flag{dirFlag, wsFileFlag(), jsonFlag, &cli.BoolFlag{Name: "all", Usage: "list all running servers"}},
			Action: statusCmd,
		},
		{
			Name:   "cancel",
			Usage:  "cancel the running query",
			Flags:  []cli.Flag{dirFlag, wsFileFlag(), editorFlag("")},
			Action: editorAction(simpleCmd(control.MethodCancel)),
		},
		{
			Name:   "refresh",
			Usage:  "reload schema metadata (after DDL)",
			Flags:  []cli.Flag{dirFlag, wsFileFlag(), editorFlag("")},
			Action: editorAction(simpleCmd(control.MethodRefresh)),
		},
		{
			// For editor glue: commands started from a keybinding have no terminal
			// (Helix :pipe-to discards their output), so they report through here.
			Name:      "notify",
			Usage:     "show MESSAGE in the editor's status line",
			ArgsUsage: "MESSAGE",
			Hidden:    true,
			Flags:     []cli.Flag{dirFlag, &cli.BoolFlag{Name: "error", Usage: "as an error"}},
			Action:    notifyCmd,
		},
		{
			Name:   "latest",
			Usage:  "print the path of the latest result CSV (e.g. vd $(sqlh latest))",
			Flags:  []cli.Flag{dirFlag, wsFileFlag(), &cli.StringFlag{Name: "conn", Usage: "standalone: connection `ALIAS` (for a results dir using {conn})"}},
			Action: latestCmd,
		},
		{
			Name:      "open",
			Usage:     "open the latest result CSV (or CSV...) in results.viewer",
			ArgsUsage: "[CSV...]",
			Flags: []cli.Flag{dirFlag, wsFileFlag(), editorFlag(""),
				&cli.StringFlag{Name: "conn", Usage: "standalone: connection `ALIAS` (for a results dir using {conn})"}},
			Action: editorAction(openCmd),
		},
	}, append(catalogCommands(), relationCommands()...)...)
}

// expandHome expands a leading "~" (editors abbreviate paths: Helix's
// %{buffer_name} is ~/… for files under the home directory).
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

// absPath is p made absolute, "~" expanded.
func absPath(p string) (string, error) { return filepath.Abs(expandHome(p)) }

// optionalAbs is absPath, "" staying "".
func optionalAbs(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	return absPath(p)
}

// startDir is where workspace/server resolution starts.
func startDir(c *cli.Context) string {
	if d := c.String("dir"); d != "" {
		return expandHome(d)
	}
	if c.IsSet("file") {
		if abs, err := absPath(c.String("file")); err == nil {
			// a new file in a new directory, or a scratch buffer: the cwd
			if fi, err := os.Stat(filepath.Dir(abs)); err == nil && fi.IsDir() {
				return filepath.Dir(abs)
			}
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// server connects to the workspace's server; ErrNoServer if there is none.
func server(c *cli.Context) (*control.Client, string, error) {
	sock, ws, err := control.Find(startDir(c))
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cl, err := control.Dial(ctx, sock)
	return cl, ws, err
}

func call(cl *control.Client, method string, params, result interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return cl.Call(ctx, method, params, result)
}

func printJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- run / export ----------------------------------------------------------

// readStdin returns piped stdin. Helix ":pipe-to" sends the selection, which
// in normal mode is just the character under the cursor: anything not longer
// than one character is ignored so --file/--line take over.
func readStdin() (string, error) {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return "", nil // a terminal: nothing piped
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	if len(strings.TrimSpace(string(b))) <= 1 {
		return "", nil
	}
	return string(b), nil
}

func runParams(c *cli.Context, all bool, limit limitChoice) (control.RunParams, error) {
	p := control.RunParams{
		SQL: c.String("sql"), Line: c.Int("line"), Col: c.Int("col"),
		Table: c.Bool("table"), All: all || limit.all,
	}
	if !p.All {
		p.Limit = limit.n
	}
	if p.SQL == "-" && c.Bool("editor") {
		return p, errors.New("--sql - reads stdin, which --editor ignores")
	}
	if (p.SQL == "-" || p.SQL == "") && !c.Bool("editor") {
		s, err := readStdin()
		if err != nil {
			return p, err
		}
		p.SQL = s
	}
	if f := c.String("file"); f != "" {
		abs, err := absPath(f)
		if err != nil {
			return p, err
		}
		p.File = abs
	}
	if o := c.String("out"); o != "" {
		abs, err := absPath(o)
		if err != nil {
			return p, err
		}
		p.Out = abs
	}
	if strings.TrimSpace(p.SQL) == "" && p.File == "" {
		return p, errors.New("nothing to run: pipe SQL on stdin, or pass --sql, or --file with --line")
	}
	return p, nil
}

func runCmd(c *cli.Context, all bool) error {
	var limit limitChoice
	if !all {
		var err error
		if limit, err = parseLimit(c.String("limit")); err != nil {
			return err
		}
	}
	if c.Bool("bg") && (c.Bool("table") || c.Bool("json")) {
		return errors.New("--bg writes CSV files; it can't print --table or --json")
	}
	p, err := runParams(c, all, limit)
	if err != nil {
		return err
	}
	if limit.ask {
		l, ok, err := askLimit()
		if err != nil || !ok {
			return err
		}
		limit = l
		p.All, p.Limit = l.all, l.n
	}
	if c.Bool("bg") {
		drop := []string{"limit", "sql"}
		var extra []string
		if limit.all || limit.n > 0 {
			extra = append(extra, "--limit", limit.String())
		}
		if p.SQL != "" { // possibly from stdin, which the detached process doesn't get
			extra = append(extra, "--sql", p.SQL)
		}
		return detach(c, drop, extra...)
	}
	var res *control.RunResult
	if c.Bool("standalone") || c.String("conn") != "" || c.String("schema") != "" {
		res, err = runStandalone(c, p)
	} else {
		cl, _, serr := server(c)
		switch {
		case errors.Is(serr, control.ErrNoServer):
			res, err = runStandalone(c, p)
		case serr != nil:
			return serr
		default:
			res, err = runViaServer(c, cl, p)
		}
	}
	if err != nil {
		return err
	}
	if err := printRun(c, res); err != nil {
		return err
	}
	if c.Bool("open") {
		return openFiles(c, res.Files)
	}
	return nil
}

// runViaServer runs on the editor's server and waits. Ctrl-C cancels the
// query (via a second connection: the first is busy waiting).
func runViaServer(c *cli.Context, cl *control.Client, p control.RunParams) (*control.RunResult, error) {
	defer cl.Close()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		if _, ok := <-sigs; ok {
			if cl2, _, err := server(c); err == nil {
				var msg string
				_ = call(cl2, control.MethodCancel, nil, &msg)
				cl2.Close()
			}
		}
	}()
	var res control.RunResult
	// no timeout: exports can take long; Ctrl-C cancels
	if err := cl.Call(context.Background(), control.MethodRun, p, &res); err != nil {
		var re *control.RemoteError
		if errors.As(err, &re) {
			return nil, &shownError{err} // the server showed it in the editor
		}
		return nil, err
	}
	return &res, nil
}

func printRun(c *cli.Context, res *control.RunResult) error {
	if c.Bool("json") {
		return printJSON(res)
	}
	if res.Table != "" {
		fmt.Print(res.Table)
	}
	for _, f := range res.Files {
		fmt.Println(f)
	}
	if res.Summary != "" {
		fmt.Fprintln(os.Stderr, res.Summary)
	}
	return nil
}

// --- connections / connect / status ----------------------------------------

func connectionsCmd(c *cli.Context) error {
	var conns []control.Connection
	cl, _, err := server(c)
	switch {
	case errors.Is(err, control.ErrNoServer):
		if conns, err = standaloneConnections(c); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		defer cl.Close()
		if err := call(cl, control.MethodConnections, nil, &conns); err != nil {
			return err
		}
	}
	if c.Bool("json") {
		return printJSON(conns)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, cn := range conns {
		mark := " "
		if cn.Current {
			mark = "*"
		}
		fmt.Fprintf(w, "%s %d\t%s\t%s\t%s\n", mark, cn.Index, cn.Alias, cn.Driver, cn.Desc)
	}
	return w.Flush()
}

func connectCmd(c *cli.Context) error {
	cl, _, err := server(c)
	if err != nil {
		if errors.Is(err, control.ErrNoServer) {
			return errors.New("no running sqlh server for this workspace (open a .sql file in the editor first)")
		}
		return err
	}
	defer cl.Close()
	alias := c.Args().First()
	if alias == "" {
		var conns []control.Connection
		if err := call(cl, control.MethodConnections, nil, &conns); err != nil {
			return err
		}
		if alias, err = pickConnection(conns); err != nil || alias == "" {
			return err
		}
	}
	file, err := optionalAbs(c.String("file"))
	if err != nil {
		return err
	}
	var st control.Status
	if err := call(cl, control.MethodConnect, control.ConnectParams{Alias: alias, File: file}, &st); err != nil {
		return err
	}
	if st.Connection != nil {
		fmt.Fprintf(os.Stderr, "connected to %s (%s)\n", st.Connection.Alias, st.Connection.Desc)
	}
	return nil
}

// pickConnection lets the user choose a connection with fzf.
func pickConnection(conns []control.Connection) (string, error) {
	lines := make([]string, len(conns))
	for i, cn := range conns {
		mark := " "
		if cn.Current {
			mark = "*"
		}
		lines[i] = fmt.Sprintf("%s\t%s %-16s %-10s %s", cn.Alias, mark, cn.Alias, cn.Driver, cn.Desc)
	}
	vals, err := pick(lines, pickOpts{prompt: "connection"})
	if err != nil || len(vals) == 0 {
		return "", err
	}
	return vals[0], nil
}

func statusCmd(c *cli.Context) error {
	if c.Bool("all") {
		servers := control.Servers()
		if c.Bool("json") {
			return printJSON(servers)
		}
		for _, s := range servers {
			fmt.Printf("%-7d %s\n", s.PID, s.Workspace)
		}
		return nil
	}
	cl, _, err := server(c)
	if err != nil {
		return err
	}
	defer cl.Close()
	var st control.Status
	if err := call(cl, control.MethodStatus, nil, &st); err != nil {
		return err
	}
	if c.Bool("json") {
		return printJSON(st)
	}
	conn := "(none)"
	if st.Connection != nil {
		conn = fmt.Sprintf("%s (%s %s)", st.Connection.Alias, st.Connection.Driver, st.Connection.Desc)
		if !st.Connected {
			conn += " [not connected]"
		}
	}
	job := st.Job
	if job == "" {
		job = "(idle)"
	}
	schema := st.Schema
	if schema == "" {
		schema = "-"
	}
	fmt.Printf("workspace:  %s\nconnection: %s\nschema:     %s\nquery:      %s\nresults:    %s\nserver:     pid %d, %s\n",
		st.Workspace, conn, schema, job, st.ResultsDir, st.PID, st.Socket)
	return nil
}

func simpleCmd(method string) cli.ActionFunc {
	return func(c *cli.Context) error {
		cl, _, err := server(c)
		if err != nil {
			return err
		}
		defer cl.Close()
		var msg string
		if err := call(cl, method, nil, &msg); err != nil {
			return err
		}
		if c.Bool("editor") {
			notifyEditor(c, msg, false)
		}
		fmt.Fprintln(os.Stderr, msg)
		return nil
	}
}

func notifyCmd(c *cli.Context) error {
	msg := strings.TrimSpace(strings.Join(c.Args().Slice(), " "))
	if msg == "" {
		return errors.New("usage: sqlh notify [--error] MESSAGE")
	}
	cl, _, err := server(c)
	if err != nil {
		return err
	}
	defer cl.Close()
	var ok string
	return call(cl, control.MethodNotify, control.NotifyParams{Message: msg, Error: c.Bool("error")}, &ok)
}

func latestCmd(c *cli.Context) error {
	p, err := latestPath(c)
	if err != nil {
		return err
	}
	fmt.Println(p)
	return nil
}

// latestPath is the newest result CSV of the workspace.
func latestPath(c *cli.Context) (string, error) {
	var dir string
	cl, _, err := server(c)
	switch {
	case errors.Is(err, control.ErrNoServer):
		if dir, err = standaloneResultsDir(c); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		defer cl.Close()
		var st control.Status
		if err := call(cl, control.MethodStatus, nil, &st); err != nil {
			return "", err
		}
		dir = st.ResultsDir
	}
	target, err := filepath.EvalSymlinks(filepath.Join(dir, "latest.csv"))
	if err != nil {
		return "", fmt.Errorf("no results yet in %s", dir)
	}
	return target, nil
}
