package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/exec"
	"github.com/raikhan/sqlh/internal/report"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/internal/state"
)

func loadConfig(c *cli.Context) (*config.Config, error) {
	if p := c.String("config"); p != "" {
		return config.GetConfig(p)
	}
	cfg, err := config.GetDefaultConfig()
	if errors.Is(err, config.ErrNotFoundConfig) {
		return nil, fmt.Errorf("no config: create %s", config.YamlConfigPath)
	}
	return cfg, err
}

// workspaceRoot finds the project root like Helix does: the nearest ancestor
// with a VCS or .helix marker, else start itself.
func workspaceRoot(start string) string {
	abs, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for d := abs; ; d = filepath.Dir(d) {
		for _, m := range []string{".git", ".helix", ".jj", ".hg", ".svn"} {
			if _, err := os.Stat(filepath.Join(d, m)); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			return abs
		}
	}
}

// chooseConnection: --conn, else the workspace's remembered connection, else
// the first one in the config.
func chooseConnection(cfg *config.Config, workspace, alias string) (int, *database.DBConfig, error) {
	if len(cfg.Connections) == 0 {
		return 0, nil, errors.New("no connections configured")
	}
	want := alias
	if want == "" {
		want = state.Connection(workspace)
	}
	if want != "" {
		for i, c := range cfg.Connections {
			if c.Alias == want {
				return i, c, nil
			}
		}
		if alias != "" {
			return 0, nil, fmt.Errorf("unknown connection %q", alias)
		}
	}
	return 0, cfg.Connections[0], nil
}

func standaloneConnections(c *cli.Context) ([]control.Connection, error) {
	cfg, err := loadConfig(c)
	if err != nil {
		return nil, err
	}
	cur, _, _ := chooseConnection(cfg, workspaceRoot(startDir(c)), "")
	var out []control.Connection
	for i, cn := range cfg.Connections {
		out = append(out, control.Connection{Index: i + 1, Alias: cn.Alias, Driver: string(cn.Driver),
			Desc: database.DescribeConnection(cn), Current: i == cur})
	}
	return out, nil
}

func standaloneResultsDir(c *cli.Context) (string, error) {
	cfg, err := loadConfig(c)
	if err != nil {
		return "", err
	}
	ws := workspaceRoot(startDir(c))
	_, conn, err := chooseConnection(cfg, ws, c.String("conn"))
	if err != nil {
		return "", err
	}
	return exec.ExpandDir(cfg.Results.Dir, ws, alias(conn)), nil
}

func alias(c *database.DBConfig) string {
	if c.Alias != "" {
		return c.Alias
	}
	return string(c.Driver)
}

func statements(p control.RunParams, d sqlsplit.Dialect) ([]sqlsplit.Statement, error) {
	if p.SQL != "" {
		st := sqlsplit.Split(p.SQL, d)
		if len(st) == 0 {
			return nil, errors.New("no statement to execute")
		}
		return st, nil
	}
	b, err := os.ReadFile(p.File)
	if err != nil {
		return nil, err
	}
	text := string(b)
	all := sqlsplit.Split(text, d)
	if p.Line < 1 {
		if len(all) == 0 {
			return nil, errors.New("no statement to execute")
		}
		return all, nil
	}
	st, ok := sqlsplit.AtCursor(text, all, sqlsplit.Position{Line: p.Line - 1, Char: max(p.Col-1, 0)})
	if !ok {
		return nil, fmt.Errorf("no statement at %s:%d", filepath.Base(p.File), p.Line)
	}
	return []sqlsplit.Statement{st}, nil
}

// runStandalone runs without a server, on its own connection.
func runStandalone(c *cli.Context, p control.RunParams) (*control.RunResult, error) {
	cfg, err := loadConfig(c)
	if err != nil {
		return nil, err
	}
	ws := workspaceRoot(startDir(c))
	_, conn, err := chooseConnection(cfg, ws, c.String("conn"))
	if err != nil {
		return nil, err
	}
	name := alias(conn)
	d := sqlsplit.DialectOf(string(conn.Driver))
	stmts, err := statements(p, d)
	if err != nil {
		return nil, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dbc, err := database.Open(conn)
	if err != nil {
		return nil, err
	}
	defer dbc.Close()
	session, err := dbc.Conn.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	limit := 0
	if !p.All {
		limit = p.Limit
		if limit <= 0 {
			limit = cfg.Results.Limit()
		}
	}
	resultsDir := exec.ExpandDir(cfg.Results.Dir, ws, name)
	values := exec.ValueFormatter{Dialect: d, Null: cfg.Results.Null}
	progress := func(n int64) {}
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		progress = func(n int64) { fmt.Fprintf(os.Stderr, "\r%s… ", report.RowCount(n)) }
	}

	started := time.Now()
	res := &control.RunResult{Connection: name}
	var (
		outcomes []exec.Outcome
		files    []string
		tables   []*exec.Table
	)
	for i, st := range stmts {
		index := 0
		if len(stmts) > 1 {
			index = i + 1
		}
		var file string
		var table *exec.Table
		newSink := func() (exec.Sink, error) {
			if p.Table {
				table = &exec.Table{}
				return table, nil
			}
			if p.Out != "" && len(files) == 0 {
				file = p.Out
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					return nil, err
				}
				_ = os.Remove(file)
			} else {
				if err := exec.PrepareDir(resultsDir); err != nil {
					return nil, err
				}
				file = exec.ResultPath(resultsDir, name, started, index)
			}
			return exec.NewCSVFile(file), nil
		}
		out, err := exec.Run(ctx, session, st, newSink, exec.Options{Limit: limit, Values: values, Progress: progress})
		if err != nil {
			if ctx.Err() != nil {
				err = errors.New("cancelled")
			}
			if len(stmts) > 1 {
				err = fmt.Errorf("line %d: %w (%d of %d statements done)", st.Start.Line+1, err, i, len(stmts))
			}
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		outcomes = append(outcomes, out)
		tables = append(tables, table)
		sr := control.StatementResult{Line: out.Line + 1, Rows: out.Rows, Truncated: out.Truncated,
			Affected: out.Affected, ElapsedMS: out.Elapsed.Milliseconds(), Kind: kindName(out.Kind)}
		if file != "" && out.Kind == exec.KindResultSet {
			sr.File = file
			files = append(files, file)
			if file != p.Out {
				_ = exec.UpdateLatest(resultsDir, file)
			}
		}
		res.Statements = append(res.Statements, sr)
	}
	elapsed := time.Since(started)
	res.Files, res.ElapsedMS = files, elapsed.Milliseconds()
	if p.Table {
		res.Table = report.Text(outcomes, tables, false)
		res.Summary = fmt.Sprintf("%s: %s", name, report.Duration(elapsed))
	} else {
		res.Summary = report.Summary(name, ws, outcomes, files, elapsed)
	}
	return res, nil
}

func kindName(k exec.Kind) string {
	switch k {
	case exec.KindResultSet:
		return "rows"
	case exec.KindPLSQL:
		return "plsql"
	default:
		return "executed"
	}
}
