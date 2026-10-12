package handler

import (
	"bytes"
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

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/exec"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

const (
	CommandRun    = "sqlh.run"    // run → CSV, first N rows
	CommandExport = "sqlh.export" // run → CSV, all rows
	CommandCancel = "sqlh.cancel" // cancel the running query
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
	uri      string
	rng      *lsp.Range
	vertical bool
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

// targetStatements resolves what to run: the selection, else the statement
// under the cursor, else (no range) the whole document.
func (s *Server) targetStatements(uri string, rng *lsp.Range) ([]sqlsplit.Statement, string, error) {
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
	d := sqlsplit.DialectOf(string(s.curDBCfg.Driver))
	var stmts []sqlsplit.Statement
	if rng != nil {
		selected := extractRangeText(f.Text, rng.Start.Line, rng.Start.Character, rng.End.Line, rng.End.Character)
		// Helix's normal-mode cursor is a 1-character selection
		if len(strings.TrimSpace(selected)) > 1 {
			stmts = sqlsplit.Split(selected, d)
			// report line numbers relative to the document
			for i := range stmts {
				stmts[i].Start.Line += rng.Start.Line
				stmts[i].End.Line += rng.Start.Line
			}
		} else {
			all := sqlsplit.Split(f.Text, d)
			st, ok := sqlsplit.AtCursor(f.Text, all, sqlsplit.Position{Line: rng.Start.Line, Char: rng.Start.Character})
			if !ok {
				return nil, "", fmt.Errorf("no statement at cursor (line %d)", rng.Start.Line+1)
			}
			stmts = []sqlsplit.Statement{st}
		}
	} else {
		stmts = sqlsplit.Split(f.Text, d)
	}
	if len(stmts) == 0 {
		return nil, "", errors.New("no statement to execute")
	}
	return stmts, uri, nil
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

func (s *Server) startRun(ctx context.Context, conn *jsonrpc2.Conn, req runRequest) (interface{}, error) {
	if s.dbConn == nil || s.curDBCfg == nil {
		return nil, errors.New("database connection is not open")
	}
	if (req.kind == jobPreview || req.kind == jobExport) && req.rng == nil {
		return nil, errors.New("no cursor position: run this from the code action menu (space a)")
	}
	stmts, uri, err := s.targetStatements(req.uri, req.rng)
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
		conn:      conn,
		progress:  s.clientProgress,
		alias:     connAlias(s.curDBCfg),
		dialect:   sqlsplit.DialectOf(string(s.curDBCfg.Driver)),
		workspace: s.workspace,
		limit:     req.limit,
		null:      cfg.Null,
	}
	if env.workspace == "" {
		env.workspace = filepath.Dir(uriToPath(uri))
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
	return nil, nil
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
	prog := lsp.StartProgress(env.conn, env.progress, j.title)
	started := time.Now()
	values := exec.ValueFormatter{Dialect: env.dialect, Null: env.null}

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
			if err := exec.PrepareDir(env.resultsDir); err != nil {
				return nil, err
			}
			file = exec.ResultPath(env.resultsDir, env.alias, started, index)
			return exec.NewCSVFile(file), nil
		}
		opts := exec.Options{
			Limit:  env.limit,
			Values: values,
			Progress: func(n int64) {
				prog.Report(rowCount(n))
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
			_ = m.ShowError(context.Background(), env.alias+": "+msg)
			return
		}
		outcomes = append(outcomes, out)
		if file != "" && out.Kind == exec.KindResultSet {
			files = append(files, file)
			if err := exec.UpdateLatest(env.resultsDir, file); err != nil {
				log.Println("update latest.csv:", err)
			}
		}
		if table != nil {
			tables = append(tables, table)
		} else {
			tables = append(tables, nil)
		}
	}
	elapsed := time.Since(started)

	if req.kind == jobText {
		prog.End("done")
		text := renderText(outcomes, tables, req.vertical)
		dir := filepath.Join(os.TempDir(), "sqlh")
		path := filepath.Join(dir, CommandExecuteQuery+".txt")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if err = os.WriteFile(path, []byte(text), 0o644); err == nil {
				m.ShowDocument("file://"+path, true)
				return
			}
		}
		_ = m.ShowError(context.Background(), "cannot write results file")
		return
	}

	summary := summarize(env, outcomes, files, elapsed)
	prog.End(summary)
	_ = m.ShowInfo(context.Background(), summary)
}

func summarize(env runEnv, outcomes []exec.Outcome, files []string, elapsed time.Duration) string {
	rel := func(p string) string {
		if r, err := filepath.Rel(env.workspace, p); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return p
	}
	if len(outcomes) == 1 {
		o := outcomes[0]
		switch o.Kind {
		case exec.KindResultSet:
			more := ""
			if o.Truncated {
				more = " (more available)"
			}
			return fmt.Sprintf("%s: %s%s → %s · %s", env.alias, rowCount(o.Rows), more, rel(files[0]), dur(elapsed))
		case exec.KindPLSQL:
			return fmt.Sprintf("%s: PL/SQL block completed · %s", env.alias, dur(elapsed))
		default:
			return fmt.Sprintf("%s: %s · %s", env.alias, affected(o.Affected), dur(elapsed))
		}
	}
	var parts []string
	aff := int64(-1) // stays -1 when no DML ran
	execs := 0
	for _, o := range outcomes {
		if o.Kind != exec.KindResultSet {
			execs++
			if o.Affected >= 0 {
				aff = max(aff, 0) + o.Affected
			}
		}
	}
	parts = append(parts, fmt.Sprintf("%d statements", len(outcomes)))
	if len(files) > 0 {
		parts = append(parts, fmt.Sprintf("%d result files (latest → %s)", len(files), rel(filepath.Join(filepath.Dir(files[len(files)-1]), exec.LatestName))))
	}
	if execs > 0 {
		parts = append(parts, affected(aff))
	}
	parts = append(parts, dur(elapsed))
	return env.alias + ": " + strings.Join(parts, " · ")
}

func rowCount(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return thousands(n) + " rows"
}

func affected(n int64) string {
	switch {
	case n < 0:
		return "statement executed"
	case n == 1:
		return "1 row affected"
	default:
		return rowCount(n) + " affected"
	}
}

func dur(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func thousands(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func renderText(outcomes []exec.Outcome, tables []*exec.Table, vertical bool) string {
	buf := new(bytes.Buffer)
	for i, o := range outcomes {
		if i > 0 {
			fmt.Fprintln(buf)
		}
		switch o.Kind {
		case exec.KindPLSQL:
			fmt.Fprintln(buf, "PL/SQL block completed")
			continue
		case exec.KindExecuted:
			fmt.Fprintln(buf, "Query OK, "+affected(o.Affected))
			continue
		}
		t := tables[i]
		if vertical {
			vw := newVerticalTableWriter(buf)
			vw.setHeaders(t.Columns)
			for _, r := range t.Rows {
				vw.appendRow(r)
			}
			vw.render()
		} else {
			tbl := tablewriter.NewTable(buf, tablewriter.WithHeaderConfig(tw.CellConfig{
				Formatting: tw.CellFormatting{AutoFormat: tw.Off},
			}))
			headers := make([]any, len(t.Columns))
			for k, v := range t.Columns {
				headers[k] = v
			}
			tbl.Header(headers...)
			for _, r := range t.Rows {
				row := make([]any, len(r))
				for k, v := range r {
					row[k] = v
				}
				_ = tbl.Append(row...)
			}
			_ = tbl.Render()
		}
		if t.Truncated {
			fmt.Fprintf(buf, "first %d rows shown, more available (Export → CSV for all rows)\n", len(t.Rows))
		} else {
			fmt.Fprintf(buf, "%d rows in set\n", len(t.Rows))
		}
	}
	return buf.String()
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
