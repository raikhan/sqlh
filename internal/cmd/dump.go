package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/header"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

func dumpCommand(dirFlag cli.Flag) *cli.Command {
	return &cli.Command{
		Name:  "dump",
		Usage: "run query files → <file>.csv (all rows) on the connection in their header",
		Description: `Runs each FILE on the connection named in its header ("-- sqlh: conn=ALIAS
[db=NAME]", kept up to date by the editor) and writes all rows to DIR/<file>.csv
(later result sets <file>_2.csv, …), replacing it once complete. --conn overrides
the header; without either the file fails. Several files run independently.

With --editor (the Helix binding) the statement runs in the editor's server on
its connection, and may copy a complete result of the same statement from the
results directory instead of running it again (results.reuseFor; --fresh: run).
Errors then show in the editor's status line, not on stderr; --bg returns at
once and lets the query finish in a detached process.`,
		ArgsUsage: "FILE...",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "out-dir", Aliases: []string{"d"}, Usage: "write `DIR`/<file>.csv (default: current directory)"},
			&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "CSV `PATH` (one FILE only)"},
			&cli.IntFlag{Name: "line", Aliases: []string{"l"}, Usage: "only the statement at this 1-based line (one FILE only)"},
			&cli.IntFlag{Name: "col", Usage: "1-based column, with --line"},
			&cli.StringFlag{Name: "conn", Usage: "run on connection `ALIAS` instead of the header's"},
			editorFlag("run in the editor's server (live buffer, its connection; header updated); errors in its status line"),
			&cli.BoolFlag{Name: "bg", Usage: "return at once; the query continues in a detached process (reports in the editor)"},
			&cli.BoolFlag{Name: "fresh", Usage: "with --editor: always run, never copy an earlier result"},
			dirFlag,
		},
		Action: editorAction(dumpCmd),
	}
}

func dumpCmd(c *cli.Context) error {
	files := c.Args().Slice()
	one := len(files) == 1
	switch {
	case len(files) == 0:
		return errors.New("usage: sqlh dump FILE... [-d DIR | -o FILE] [--line L] [--conn ALIAS]")
	case c.IsSet("out") && c.IsSet("out-dir"):
		return errors.New("-o and -d exclude each other")
	case !one && c.IsSet("out"):
		return errors.New("-o takes one FILE; use -d DIR for several")
	case !one && c.IsSet("line"):
		return errors.New("--line takes one FILE")
	case !one && c.Bool("editor"):
		return errors.New("--editor takes one FILE")
	}
	if one && c.Bool("editor") && !c.IsSet("dir") {
		// the file's workspace: its server runs it and shows errors
		if abs, err := absPath(files[0]); err == nil {
			_ = c.Set("dir", filepath.Dir(abs))
		}
	}
	if c.Bool("bg") {
		return detach(c, nil)
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	outDir := wd
	if d := c.String("out-dir"); d != "" {
		if outDir, err = absPath(d); err != nil {
			return err
		}
	}

	var cfg *config.Config // loaded once, when needed
	failed := 0
	for _, f := range files {
		res, err := dumpFile(c, f, outDir, wd, &cfg)
		if err != nil {
			if one {
				return err
			}
			fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
			failed++
			continue
		}
		for _, p := range res.Files {
			fmt.Println(p)
		}
		if res.Summary != "" {
			fmt.Fprintln(os.Stderr, res.Summary)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d files failed", failed, len(files))
	}
	return nil
}

func dumpFile(c *cli.Context, file, outDir, wd string, cfg **config.Config) (*control.RunResult, error) {
	abs, err := absPath(file)
	if err != nil {
		return nil, err
	}
	out := filepath.Join(outDir, strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))+".csv")
	if o := c.String("out"); o != "" {
		if out, err = absPath(o); err != nil {
			return nil, err
		}
	}
	if out == abs {
		return nil, fmt.Errorf("the CSV would overwrite the query file")
	}
	p := control.RunParams{File: abs, Line: c.Int("line"), Col: c.Int("col"), All: true, Out: out, Dump: true}

	if c.Bool("editor") {
		if !c.IsSet("dir") {
			_ = c.Set("dir", filepath.Dir(abs)) // the file's workspace (also for Ctrl-C)
		}
		cl, _, err := server(c)
		switch {
		case err == nil:
			p.Dump, p.Fresh = true, c.Bool("fresh")
			return runViaServer(c, cl, p)
		case !errors.Is(err, control.ErrNoServer):
			return nil, err
		}
		// no editor server: like the terminal, on the header's connection
	}

	if *cfg == nil {
		if *cfg, err = loadConfig(c); err != nil {
			return nil, err
		}
	}
	conn, err := dumpConnection(*cfg, abs, c.String("conn"))
	if err != nil {
		return nil, err
	}
	stmts, err := statements(p, sqlsplit.DialectOf(string(conn.Driver)))
	if err != nil {
		return nil, err
	}
	return execStandalone(*cfg, workspaceRoot(filepath.Dir(abs)), wd, conn, stmts, p)
}

// dumpConnection: --conn, else the file's header (db=/schema= in it are an
// error: a connection is the target).
func dumpConnection(cfg *config.Config, file, override string) (*database.DBConfig, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	h := header.Parse(string(b))
	if err := h.ErrSwitch(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	name := override
	if name == "" {
		name = h.Conn
	}
	if name == "" {
		return nil, errors.New(`no connection: add "-- sqlh: conn=ALIAS" at the top of the file (running it in the editor does), or pass --conn`)
	}
	for _, cn := range cfg.Connections {
		if cn.Alias == name {
			return cn, nil
		}
	}
	var names []string
	for _, cn := range cfg.Connections {
		if cn.Alias != "" {
			names = append(names, cn.Alias)
		}
	}
	src := "header"
	if override != "" {
		src = "--conn"
	}
	return nil, fmt.Errorf("unknown connection %q (%s); configured: %s", name, src, strings.Join(names, ", "))
}
