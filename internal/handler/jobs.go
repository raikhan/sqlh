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
	"github.com/raikhan/sqlh/internal/history"
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

	// results are complete results in the results directory, which a dump
	// of the same statement copies instead of running it. Cleared whenever
	// the session may have changed data (any statement that isn't a plain
	// read, a new session).
	results map[resultKey]resultRecord

	// executed: DDL statements run successfully (connection \t database \t
	// normalised text); their objects aren't linted any more (DROP TABLE t:
	// t is gone now, which is the point).
	executed map[string]bool
}

// maxExecuted bounds the remembered DDL statements.
const maxExecuted = 2000

// normalizeSQL: a statement without leading comments, white space collapsed.
func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(exec.StripLeadingComments(sql)), " ")
}

func (js *jobState) rememberDDL(prefix, sql string) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.executed == nil || len(js.executed) >= maxExecuted {
		js.executed = map[string]bool{}
	}
	js.executed[prefix+normalizeSQL(sql)] = true
}

// executedDDLFilter tells whether a statement was run (nil without a prefix,
// i.e. no connection).
func (js *jobState) executedDDLFilter(prefix string) func(string) bool {
	if prefix == "" {
		return nil
	}
	return func(sql string) bool {
		js.mu.Lock()
		defer js.mu.Unlock()
		return js.executed[prefix+normalizeSQL(sql)]
	}
}

// resultKey identifies a statement's result: connection, database override
// and the exact statement text (leading comments, e.g. the header, are not
// part of it).
type resultKey struct{ conn, db, sql string }

type resultRecord struct {
	file string
	rows int64
	at   time.Time
}

// maxResults bounds the remembered results (the oldest goes first).
const maxResults = 64

// forgetResults: the session may have changed data.
func (js *jobState) forgetResults() {
	js.mu.Lock()
	js.results = nil
	js.mu.Unlock()
}

func (js *jobState) rememberResult(k resultKey, r resultRecord) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.results == nil {
		js.results = map[resultKey]resultRecord{}
	}
	if _, ok := js.results[k]; !ok && len(js.results) >= maxResults {
		var oldest resultKey
		first := true
		for k2, r2 := range js.results {
			if first || r2.at.Before(js.results[oldest].at) {
				oldest, first = k2, false
			}
		}
		delete(js.results, oldest)
	}
	js.results[k] = r
}

// reusableResult returns a remembered result younger than window whose file
// still exists.
func (js *jobState) reusableResult(k resultKey, window time.Duration) (resultRecord, bool) {
	js.mu.Lock()
	r, ok := js.results[k]
	js.mu.Unlock()
	if !ok || window <= 0 || time.Since(r.at) > window {
		return resultRecord{}, false
	}
	if _, err := os.Stat(r.file); err != nil {
		return resultRecord{}, false
	}
	return r, true
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
	out     string // CSV path for the first result set (later ones numbered)
	fromCLI bool

	// dump: an export to out that may copy a remembered result of the same
	// statement (unless fresh)
	dump  bool
	fresh bool
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
		if samePath(uriToPath(u), abs) {
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
	js.forgetResults() // a new session: its transaction, temp tables, … are gone
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
	db         string // database and schema switches (targetKey), "" = the connection's own
	limit      int
	null       string
	reuse      *resultRecord // dump: copy this result instead of running
	// executedPrefix keys the DDL statements run (connection \t database \t)
	executedPrefix string
	// refresh reloads the schema cache (nil: refreshAfterDDL off)
	refresh func(context.Context) error
	// for the history: the file run from, the database/schema, environment
	file, database, schema, env string
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
		db:        s.targetKey(),
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
	if req.sql == "" {
		env.file = uriToPath(uri)
	}
	env.database, env.schema, env.env = s.curDBCfg.DBName, s.currentSchema(), s.curDBCfg.Env()
	env.executedPrefix = env.alias + "\t" + env.db + "\t"
	if s.getConfig().RefreshAfterDDLEnabled() {
		if repo, err := s.newDBRepository(ctx); err == nil {
			env.refresh = func(ctx context.Context) error {
				if err := s.worker.ReCache(ctx, repo); err != nil {
					return err
				}
				s.jsonKeys.Clear()
				return nil
			}
		}
	}
	switch {
	case req.kind == jobExport:
		env.limit = 0
	case env.limit <= 0:
		env.limit = cfg.Limit()
	}

	if req.dump && !req.fresh && len(stmts) == 1 {
		k := resultKey{env.alias, env.db, stmts[0].SQL}
		if r, ok := s.jobs.reusableResult(k, cfg.ReuseWindow()); ok {
			env.reuse = &r
		}
	}

	title := fmt.Sprintf("%s: %s", env.alias, describeRun(req, env.limit, len(stmts)))
	jctx, cancel := context.WithCancel(context.Background())
	j := &job{title: title, cancel: cancel, done: make(chan struct{})}
	s.jobs.mu.Lock()
	s.jobs.cur = j
	s.jobs.mu.Unlock()

	go s.runJob(jctx, j, session, stmts, req, env)
	if req.sql == "" && uri != "" {
		s.stampHeader(uri)
	}
	return j, nil
}

func describeRun(req runRequest, limit, n int) string {
	what := "statement"
	if n > 1 {
		what = fmt.Sprintf("%d statements", n)
	}
	switch kind := req.kind; {
	case req.dump:
		return "dump " + what + " → " + filepath.Base(req.out)
	case kind == jobExport:
		return "export " + what + " → CSV"
	case kind == jobPreview:
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
		s.emit(control.Event{Kind: control.EventRun, Message: j.title})
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

	if env.reuse != nil {
		res, err := reuseResult(env, req.out, stmts[0])
		if err == nil {
			prog.End(res.Summary)
			j.result = res
			notify(true, res.Summary)
			return
		}
		log.Println("reuse result, running instead:", err)
	}

	res := &control.RunResult{Connection: env.alias}
	var (
		outcomes []exec.Outcome
		files    []string
		tables   []*exec.Table
		ddlRan   bool
	)
	// afterDDL: once DDL ran, reload the schema cache (also when a later
	// statement failed) and re-lint (the statements run are no longer
	// checked). Returns a note for the summary.
	afterDDL := func() string {
		if !ddlRan {
			return ""
		}
		defer func() { go s.relintAll() }()
		if env.refresh == nil || ctx.Err() != nil {
			return ""
		}
		if err := env.refresh(ctx); err != nil {
			log.Println("refresh after DDL:", err)
			return " · schema refresh failed: " + err.Error()
		}
		return " · schema refreshed"
	}
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
			if req.out != "" {
				// --out / dump: overwritten once complete; later result sets numbered
				file = exec.NumberedPath(req.out, len(files)+1)
				if err := exec.PrepareOut(file); err != nil {
					return nil, err
				}
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
		readOnly := exec.ReadOnly(st)
		if !readOnly {
			s.jobs.forgetResults() // it may change what queries return
		}
		out, err := exec.Run(ctx, session, st, newSink, opts)
		s.record(env, req, st, out, file, err, ctx.Err() != nil)
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
			msg += afterDDL()
			j.err = errors.New(env.alias + ": " + msg)
			notify(false, j.err.Error())
			return
		}
		if exec.IsDDL(st) {
			ddlRan = true
			s.jobs.rememberDDL(env.executedPrefix, st.SQL)
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
			if req.out == "" {
				if err := exec.UpdateLatest(env.resultsDir, file); err != nil {
					log.Println("update latest.csv:", err)
				}
				if readOnly && !out.Truncated {
					s.jobs.rememberResult(resultKey{env.alias, env.db, st.SQL},
						resultRecord{file: file, rows: out.Rows, at: time.Now()})
				}
			}
		}
		res.Statements = append(res.Statements, sr)
	}
	elapsed := time.Since(started)
	res.Files = files
	res.ElapsedMS = elapsed.Milliseconds()

	note := afterDDL()
	if req.kind == jobText {
		res.Table = report.Text(outcomes, tables, req.vertical)
		res.Summary = fmt.Sprintf("%s: %s", env.alias, report.Duration(elapsed)) + note
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

	res.Summary = report.Summary(env.alias, env.workspace, outcomes, files, elapsed, req.out == "") + note
	prog.End(res.Summary)
	j.result = res
	notify(true, res.Summary)
}

// record adds a statement run to the workspace's history and tells the
// subscribers.
func (s *Server) record(env runEnv, req runRequest, st sqlsplit.Statement, out exec.Outcome, file string, err error, cancelled bool) {
	e := history.Entry{
		Kind: runKind(req), File: env.file, Line: st.Start.Line + 1, SQL: strings.TrimSpace(st.SQL),
		Conn: env.alias, Database: env.database, Schema: env.schema, Env: env.env,
		ElapsedMS: out.Elapsed.Milliseconds(), Rows: out.Rows, Affected: out.Affected, Truncated: out.Truncated, Status: "ok",
	}
	if out.Kind == exec.KindResultSet {
		e.Result = file
	}
	switch {
	case cancelled:
		e.Status, e.Result = "cancelled", ""
	case err != nil:
		e.Status, e.Error, e.Result = "error", strings.Join(strings.Fields(err.Error()), " "), ""
	}
	if _, err := history.Append(env.workspace, e); err != nil {
		log.Println("history:", err)
		return
	}
	s.emit(control.Event{Kind: control.EventHistory})
}

func runKind(req runRequest) string {
	switch {
	case req.dump:
		return "dump"
	case req.kind == jobExport:
		return "export"
	case req.kind == jobText:
		return "text"
	}
	return "preview"
}

// reuseResult copies a remembered result to out instead of running st.
func reuseResult(env runEnv, out string, st sqlsplit.Statement) (*control.RunResult, error) {
	started := time.Now()
	r := env.reuse
	if err := exec.CopyResult(r.file, out); err != nil {
		return nil, err
	}
	elapsed := time.Since(started)
	return &control.RunResult{
		Connection: env.alias,
		Summary:    report.Reused(env.alias, env.workspace, out, r.rows, r.at),
		Statements: []control.StatementResult{{
			Line: st.Start.Line + 1, Kind: "rows", Rows: r.rows, Affected: -1,
			File: out, ElapsedMS: elapsed.Milliseconds(),
		}},
		Files:      []string{out},
		ReusedFrom: r.file,
		ElapsedMS:  elapsed.Milliseconds(),
	}, nil
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

// samePath: two absolute paths name the same file, also through symlinks
// (macOS /tmp → /private/tmp: the editor and the CLI may spell it apart).
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" || filepath.Base(a) != filepath.Base(b) {
		return false
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// uriToPath converts a file:// URI to a local path ("" for other schemes).
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.FromSlash(u.Path)
}
