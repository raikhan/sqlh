package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/exec"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/report"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

const (
	CommandRun     = "sqlh.run"     // run → CSV, first N rows
	CommandExport  = "sqlh.export"  // run → CSV, all rows
	CommandCancel  = "sqlh.cancel"  // cancel the running query
	CommandRefresh = "sqlh.refresh" // reload schema metadata
)

type jobKind int

const (
	jobText    jobKind = iota // render a text table into a document
	jobPreview                // CSV, first N rows
	jobExport                 // CSV, all rows
)

// job is a statement run executing in the background, so long exports don't
// block completion. One job runs at a time, on the pinned session.
type job struct {
	title  string
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	reason string // why it was cancelled

	// set before done is closed
	result *control.RunResult
	err    error
}

func (j *job) stop(reason string) {
	j.mu.Lock()
	if j.reason == "" {
		j.reason = reason
	}
	j.mu.Unlock()
	j.cancel()
}

func (j *job) cancelReason() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.reason
}

type jobState struct {
	mu  sync.Mutex
	cur *job

	// session is one connection pinned for executions, so session state
	// (an open transaction, ALTER SESSION, temp tables) carries over between
	// runs like in psql/sqlplus. Only used by the running job.
	session      *sql.Conn
	sessionOwner *database.DBConnection
}

type runRequest struct {
	kind     jobKind
	limit    int
	uri      string     // editor: document
	rng      *lsp.Range // editor: selection / cursor
	vertical bool

	// CLI sources (instead of uri/rng): explicit SQL, or a file + 1-based line
	sql     string
	file    string
	line    int
	col     int
	out     string // CSV path for the first result set
	fromCLI bool
}

// parseRunArgs reads [uri?, range?, limit?, "-show-vertical"?] in any order.
func parseRunArgs(args []interface{}) (uri string, rng *lsp.Range, limit int, vertical bool, err error) {
	for _, arg := range args {
		switch v := arg.(type) {
		case string:
			if v == "-show-vertical" {
				vertical = true
			} else {
				uri = v
			}
		case float64: // JSON numbers
			limit = int(v)
		case map[string]interface{}:
			if rng, err = rangeFromArg(v); err != nil {
				return
			}
		}
	}
	return
}

// resolveStatements works out what to run:
//   - CLI SQL (stdin/--sql): all statements in it;
//   - CLI file + line: the statement at that line, using the editor's live
//     buffer when the file is open (unsaved edits included), else the disk;
//   - editor selection: its statements; editor cursor: the statement under it;
//   - editor without a range: the whole document.
func (s *Server) resolveStatements(req runRequest) ([]sqlsplit.Statement, string, error) {
	d := sqlsplit.DialectOf(string(s.curDBCfg.Driver))
	nonEmpty := func(st []sqlsplit.Statement) ([]sqlsplit.Statement, error) {
		if len(st) == 0 {
			return nil, errors.New("no statement to execute")
		}
		return st, nil
	}
	switch {
	case strings.TrimSpace(req.sql) != "":
		st, err := nonEmpty(sqlsplit.Split(req.sql, d))
		return st, "", err
	case req.file != "":
		uri, text, err := s.documentText(req.file)
		if err != nil {
			return nil, "", err
		}
		if req.line < 1 {
			st, err := nonEmpty(sqlsplit.Split(text, d))
			return st, uri, err
		}
		col := max(req.col-1, 0)
		st, ok := sqlsplit.AtCursor(text, sqlsplit.Split(text, d), sqlsplit.Position{Line: req.line - 1, Char: col})
		if !ok {
			return nil, "", fmt.Errorf("no statement at %s:%d", filepath.Base(req.file), req.line)
		}
		return []sqlsplit.Statement{st}, uri, nil
	}

	uri := req.uri
	if uri == "" {
		uri = s.lastURI
	}
	if uri == "" {
		return nil, "", errors.New("no document: open a .sql file first")
	}
	f, ok := s.files[uri]
	if !ok {
		return nil, "", fmt.Errorf("document not found, %q", uri)
	}
	rng := req.rng
	if rng == nil {
		st, err := nonEmpty(sqlsplit.Split(f.Text, d))
		return st, uri, err
	}
	selected := extractRangeText(f.Text, rng.Start.Line, rng.Start.Character, rng.End.Line, rng.End.Character)
	// Helix's normal-mode cursor is a 1-character selection
	if len(strings.TrimSpace(selected)) > 1 {
		stmts := sqlsplit.Split(selected, d)
		// report line numbers relative to the document
		for i := range stmts {
			stmts[i].Start.Line += rng.Start.Line
			stmts[i].End.Line += rng.Start.Line
		}
		st, err := nonEmpty(stmts)
		return st, uri, err
	}
	st, ok := sqlsplit.AtCursor(f.Text, sqlsplit.Split(f.Text, d), sqlsplit.Position{Line: rng.Start.Line, Char: rng.Start.Character})
	if !ok {
		return nil, "", fmt.Errorf("no statement at cursor (line %d)", rng.Start.Line+1)
	}
	return []sqlsplit.Statement{st}, uri, nil
}

// documentText returns a file's text: the editor's live buffer if open,
// otherwise the file on disk.
func (s *Server) documentText(path string) (uri, text string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	for u, f := range s.files {
		if uriToPath(u) == abs {
			return u, f.Text, nil
		}
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", "", err
	}
	return "file://" + abs, string(b), nil
}

// sessionConn returns the pinned session, (re)opening it when missing,
// belonging to another connection, or broken (e.g. after a cancelled fetch).
func (s *Server) sessionConn(ctx context.Context) (*sql.Conn, error) {
	js := &s.jobs
	if js.session != nil && js.sessionOwner == s.dbConn {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := js.session.PingContext(pctx)
		cancel()
		if err == nil {
			return js.session, nil
		}
		log.Println("session lost, reconnecting:", err)
	}
	s.closeSession()
	c, err := s.dbConn.Conn.Conn(ctx)
	if err != nil {
		return nil, err
	}
	js.session, js.sessionOwner = c, s.dbConn
	return c, nil
}

func (s *Server) closeSession() {
	js := &s.jobs
	if js.session != nil {
		if err := js.session.Close(); err != nil {
			log.Println("close session:", err)
		}
	}
	js.session, js.sessionOwner = nil, nil
}

// stopJob cancels the running job and waits (bounded) for it to finish.
func (s *Server) stopJob(reason string) {
	s.jobs.mu.Lock()
	j := s.jobs.cur
	s.jobs.mu.Unlock()
	if j == nil {
		return
	}
	j.stop(reason)
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		log.Println("job did not stop in time:", j.title)
	}
}

func (s *Server) runningJob() *job {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	return s.jobs.cur
}

// runEnv is the snapshot a job needs; the job must not read server state.
type runEnv struct {
	conn       *jsonrpc2.Conn
	progress   bool
	alias      string
	dialect    sqlsplit.Dialect
	resultsDir string
	workspace  string
	limit      int
	null       string
}

// startRun validates a request and starts it as a background job. Call with
// s.mu held; the job itself does not touch server state.
func (s *Server) startRun(ctx context.Context, req runRequest) (*job, error) {
	if s.dbConn == nil || s.curDBCfg == nil {
		return nil, errors.New("database connection is not open")
	}
	if !req.fromCLI && (req.kind == jobPreview || req.kind == jobExport) && req.rng == nil {
		return nil, errors.New("no cursor position: use the code action menu (space a), or `sqlh run --file F --line L [--limit N]`")
	}
	stmts, uri, err := s.resolveStatements(req)
	if err != nil {
		return nil, err
	}
	if j := s.runningJob(); j != nil {
		return nil, fmt.Errorf("a query is already running (%s); cancel it with %s", j.title, CommandCancel)
	}
	session, err := s.sessionConn(ctx)
	if err != nil {
		return nil, err
	}

	cfg := s.getConfig().Results
	env := runEnv{
		conn:      s.editor,
		progress:  s.clientProgress,
		alias:     connAlias(s.curDBCfg),
		dialect:   sqlsplit.DialectOf(string(s.curDBCfg.Driver)),
		workspace: s.workspace,
		limit:     req.limit,
		null:      cfg.Null,
	}
	if env.workspace == "" {
		if p := uriToPath(uri); p != "" {
			env.workspace = filepath.Dir(p)
		} else if wd, err := os.Getwd(); err == nil {
			env.workspace = wd
		}
	}
	env.resultsDir = exec.ExpandDir(cfg.Dir, env.workspace, env.alias)
	switch {
	case req.kind == jobExport:
		env.limit = 0
	case env.limit <= 0:
		env.limit = cfg.Limit()
	}

	title := fmt.Sprintf("%s: %s", env.alias, describeRun(req.kind, env.limit, len(stmts)))
	jctx, cancel := context.WithCancel(context.Background())
	j := &job{title: title, cancel: cancel, done: make(chan struct{})}
	s.jobs.mu.Lock()
	s.jobs.cur = j
	s.jobs.mu.Unlock()

	go s.runJob(jctx, j, session, stmts, req, env)
	return j, nil
}

func describeRun(kind jobKind, limit, n int) string {
	what := "statement"
	if n > 1 {
		what = fmt.Sprintf("%d statements", n)
	}
	switch kind {
	case jobExport:
		return "export " + what + " → CSV"
	case jobPreview:
		return fmt.Sprintf("run %s → CSV (first %d rows)", what, limit)
	default:
		return fmt.Sprintf("run %s (first %d rows)", what, limit)
	}
}

func (s *Server) runJob(ctx context.Context, j *job, session *sql.Conn, stmts []sqlsplit.Statement, req runRequest, env runEnv) {
	defer func() {
		s.jobs.mu.Lock()
		if s.jobs.cur == j {
			s.jobs.cur = nil
		}
		s.jobs.mu.Unlock()
		j.cancel()
		close(j.done)
	}()
	m := lsp.NewMessenger(env.conn)
	notify := func(info bool, msg string) {
		if env.conn == nil {
			return
		}
		if info {
			_ = m.ShowInfo(context.Background(), msg)
		} else {
			_ = m.ShowError(context.Background(), msg)
		}
	}
	prog := lsp.StartProgress(env.conn, env.progress, j.title)
	started := time.Now()
	values := exec.ValueFormatter{Dialect: env.dialect, Null: env.null}

	res := &control.RunResult{Connection: env.alias}
	var (
		outcomes []exec.Outcome
		files    []string
		tables   []*exec.Table
	)
	for i, st := range stmts {
		if len(stmts) > 1 {
			prog.Report(fmt.Sprintf("statement %d of %d", i+1, len(stmts)))
		}
		index := 0
		if len(stmts) > 1 {
			index = i + 1
		}
		var file string
		var table *exec.Table
		newSink := func() (exec.Sink, error) {
			if req.kind == jobText {
				table = &exec.Table{}
				return table, nil
			}
			if req.out != "" && len(files) == 0 {
				file = req.out
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					return nil, err
				}
				_ = os.Remove(file) // --out overwrites
			} else {
				if err := exec.PrepareDir(env.resultsDir); err != nil {
					return nil, err
				}
				file = exec.ResultPath(env.resultsDir, env.alias, started, index)
			}
			return exec.NewCSVFile(file), nil
		}
		opts := exec.Options{
			Limit:  env.limit,
			Values: values,
			Progress: func(n int64) {
				prog.Report(report.RowCount(n))
			},
		}
		out, err := exec.Run(ctx, session, st, newSink, opts)
		if err != nil {
			prog.End("failed")
			// drivers report cancellation their own way (pgx: context
			// canceled, go-ora: ORA-01013); the job context is authoritative
			msg := strings.Join(strings.Fields(err.Error()), " ")
			if ctx.Err() != nil {
				msg = "cancelled"
				if r := j.cancelReason(); r != "" {
					msg += " (" + r + ")"
				}
				if req.kind != jobText && out.Kind == exec.KindResultSet {
					msg += "; partial file removed"
				}
			}
			if len(stmts) > 1 {
				msg = fmt.Sprintf("line %d: %s (%d of %d statements done)", out.Line+1, msg, i, len(stmts))
			}
			j.err = errors.New(env.alias + ": " + msg)
			notify(false, j.err.Error())
			return
		}
		outcomes = append(outcomes, out)
		tables = append(tables, table)
		sr := control.StatementResult{
			Line: out.Line + 1, Rows: out.Rows, Truncated: out.Truncated,
			Affected: out.Affected, ElapsedMS: out.Elapsed.Milliseconds(),
		}
		switch out.Kind {
		case exec.KindResultSet:
			sr.Kind = "rows"
		case exec.KindPLSQL:
			sr.Kind = "plsql"
		default:
			sr.Kind = "executed"
		}
		if file != "" && out.Kind == exec.KindResultSet {
			sr.File = file
			files = append(files, file)
			if req.out == "" || file != req.out {
				if err := exec.UpdateLatest(env.resultsDir, file); err != nil {
					log.Println("update latest.csv:", err)
				}
			}
		}
		res.Statements = append(res.Statements, sr)
	}
	elapsed := time.Since(started)
	res.Files = files
	res.ElapsedMS = elapsed.Milliseconds()

	if req.kind == jobText {
		res.Table = report.Text(outcomes, tables, req.vertical)
		res.Summary = fmt.Sprintf("%s: %s", env.alias, report.Duration(elapsed))
		prog.End("done")
		j.result = res
		if req.fromCLI {
			return // the CLI prints the table
		}
		dir := filepath.Join(os.TempDir(), "sqlh")
		path := filepath.Join(dir, CommandExecuteQuery+".txt")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if err = os.WriteFile(path, []byte(res.Table), 0o644); err == nil {
				m.ShowDocument("file://"+path, true)
				return
			}
		}
		notify(false, "cannot write results file")
		return
	}

	res.Summary = report.Summary(env.alias, env.workspace, outcomes, files, elapsed)
	prog.End(res.Summary)
	j.result = res
	notify(true, res.Summary)
}

func connAlias(cfg *database.DBConfig) string {
	if cfg == nil {
		return "db"
	}
	if cfg.Alias != "" {
		return cfg.Alias
	}
	return string(cfg.Driver)
}

// uriToPath converts a file:// URI to a local path ("" for other schemes).
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.FromSlash(u.Path)
}
