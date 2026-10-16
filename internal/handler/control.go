package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/exec"
	"github.com/raikhan/sqlh/internal/history"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/state"
)

// restoreConnection selects the connection remembered for this workspace
// (see internal/state). Called at initialize, before connecting.
func (s *Server) restoreConnection() {
	alias := state.Connection(s.workspace)
	if alias == "" {
		return
	}
	cfg := s.getConfig()
	if cfg == nil {
		return
	}
	for i, c := range cfg.Connections {
		if c.Alias == alias {
			s.curConnectionIndex = i
			return
		}
	}
}

// startControl listens on the workspace's control socket for the CLI.
func (s *Server) startControl() {
	if s.workspace == "" || s.control != nil {
		return
	}
	listen := control.Listen
	if s.headless {
		listen = control.ListenHeadless
	}
	l, err := listen(s.workspace)
	if err != nil {
		log.Println("control socket:", err)
		return
	}
	s.control = l
	log.Println("control socket:", l.Addr())
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Println("control accept:", err)
				}
				return
			}
			// Async: a run waits for its job inside the handler; the read
			// loop must keep going to notice the CLI disconnecting (Ctrl-C,
			// kill). Every control method takes s.mu itself.
			conn := jsonrpc2.NewConn(context.Background(),
				jsonrpc2.NewBufferedStream(nc, jsonrpc2.VSCodeObjectCodec{}),
				jsonrpc2.AsyncHandler(jsonrpc2.HandlerWithError(s.handleControl)),
				// its own logger writes to stderr: follow the log package's
				// output (the app sends it to a file)
				jsonrpc2.SetLogger(log.Default()))
			s.trackControlConn(conn)
		}
	}()
}

func (s *Server) stopControl() {
	if s.control != nil {
		_ = s.control.Close()
		control.Cleanup(s.workspace)
		s.control = nil
		// subscribers (panels) must notice and reconnect to the next server
		s.controlConns.Range(func(k, _ any) bool {
			_ = k.(*jsonrpc2.Conn).Close()
			return true
		})
	}
}

// trackControlConn remembers an open control connection until it closes.
func (s *Server) trackControlConn(conn *jsonrpc2.Conn) {
	s.controlConns.Store(conn, true)
	go func() {
		<-conn.DisconnectNotify()
		s.controlConns.Delete(conn)
	}()
}

// handleControl serves CLI requests. Like editor requests they run under
// s.mu, except that run waits for its job without holding the lock.
func (s *Server) handleControl(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	defer func() {
		if perr := panicf(recover(), "%v", req.Method); perr != nil {
			err = perr
		}
	}()
	switch req.Method {
	case control.MethodRun:
		return s.controlRun(ctx, conn, req)
	case control.MethodDDL: // may wait for the database: takes s.mu itself
		var p control.DDLParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.controlDDL(ctx, p.Table)
	case control.MethodSubscribe:
		var p control.SubscribeParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.subscribe(conn, p), nil
	case control.MethodPanelFocus:
		var p control.PanelFocusParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.panelFocus(p), nil
	case control.MethodPanelAction:
		var p control.PanelActionParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		s.setPanelAction(p)
		return "ok", nil
	case control.MethodPanelTake:
		var p control.PanelTakeParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.takePanelAction(p), nil
	case control.MethodPanels:
		return s.panels(), nil
	case control.MethodRelease:
		if !s.headless {
			return nil, errors.New("this server belongs to an editor")
		}
		// an editor's server starts for the workspace: stop serving (the
		// subscribers reconnect to the new server) but stay warm — the
		// connection and the schema cache — to take over again at once when
		// the editor exits (Resume)
		s.mu.Lock()
		s.stopJob("the editor's server took over")
		s.stopControl()
		s.releasedAt = time.Now()
		s.mu.Unlock()
		return "released", nil
	case control.MethodSample: // queries the database: takes s.mu itself
		var p control.SampleParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.controlSample(ctx, p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch req.Method {
	case control.MethodStatus:
		// the panel reads its settings here (placement, keys, …): a config
		// file edited since counts at once
		s.reloadConfigIfChanged(ctx)
		return s.controlStatus(), nil
	case control.MethodConnections:
		s.reloadConfigIfChanged(ctx)
		return s.controlConnections(), nil
	case control.MethodReloadConfig:
		return s.reloadConfigIfChanged(ctx)
	case control.MethodAllColumns:
		return s.allColumns()
	case control.MethodCatalog:
		return s.controlCatalog()
	case control.MethodFileOpen:
		var p control.FileParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		_, ok := s.openURI(p.File)
		return ok, nil
	case control.MethodCurrentVersion:
		var p control.CurrentVersionParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.currentVersion(p)
	case control.MethodHistory:
		var p control.HistoryParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.controlHistory(p)
	case control.MethodReplaceStatement:
		var p control.ReplaceParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.replaceStatement(p)
	case control.MethodConnect:
		var p control.ConnectParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		msg, err := s.switchTo(ctx, p.Alias)
		if err != nil {
			return nil, err
		}
		s.stampFile(p.File)
		s.notifyEditor(msg)
		return s.controlStatus(), nil
	case control.MethodCancel:
		if j := s.runningJob(); j != nil {
			j.stop("cancelled from the CLI")
			return "cancelling: " + j.title, nil
		}
		return "no query is running", nil
	case control.MethodRefresh:
		return s.refreshCache(ctx)
	case control.MethodNotify:
		var p control.NotifyParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		if p.Error {
			s.notifyEditorError(p.Message)
		} else {
			s.notifyEditor(p.Message)
		}
		return "ok", nil
	case control.MethodSchemas:
		var p control.SchemasParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		src, opt, err := s.catalog(p.All)
		if err != nil {
			return nil, err
		}
		return catalog.Schemas(src, opt), nil
	case control.MethodTables:
		var p control.TablesParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		src, opt, err := s.catalog(p.All)
		if err != nil {
			return nil, err
		}
		return catalog.Tables(src, p.Schema, opt), nil
	case control.MethodColumns:
		var p control.ColumnsParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		src, opt, err := s.catalog(false)
		if err != nil {
			return nil, err
		}
		return catalog.Columns(src, p.Table, opt)
	case control.MethodRelations:
		var p control.RelationsParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		src, opt, err := s.catalog(false)
		if err != nil {
			return nil, err
		}
		return catalog.Relations(src, p.Table, opt, s.getConfig().Relations.MaybeEnabled())
	case control.MethodChains:
		var p control.ChainsParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		src, opt, err := s.catalog(false)
		if err != nil {
			return nil, err
		}
		cfg := s.getConfig()
		depth := p.MaxDepth
		if depth <= 0 {
			depth = cfg.Relations.ChainDepthOrDefault()
		}
		limit := p.Limit
		if limit <= 0 {
			limit = cfg.Relations.ChainLimitOrDefault()
		}
		return catalog.Chains(src, p.From, p.Target, depth, limit, opt, cfg.Relations.MaybeEnabled())
	case control.MethodContext:
		var p control.ContextParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		_, text, err := s.documentText(p.File)
		if err != nil {
			return nil, err
		}
		res := catalog.Context(text, p.Line, p.Col)
		cfg := s.getConfig()
		res.LowercaseKeywords = cfg.LowercaseKeywords
		res.Insert = cfg.Relations.InsertMode()
		res.Maybe = cfg.Relations.MaybeEnabled()
		return res, nil
	case control.MethodDatabases:
		repo, err := s.newDBRepository(ctx)
		if err != nil {
			return nil, err
		}
		return repo.Databases(ctx)
	}
	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: "unknown method " + req.Method}
}

// RunHeadless starts a server without an editor for a workspace (the app (`sqlh`)):
// connected, serving the control socket until an editor's server takes it
// (MethodRelease) or Stop.
func RunHeadless(cfg *config.Config, configPath, workspace string) (*Server, error) {
	s := NewServer()
	s.SpecificFileCfg = cfg
	s.ConfigPath = configPath
	s.NoteConfigRead()
	s.workspace = workspace
	s.headless = true
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreConnection()
	s.startControl()
	if s.control == nil {
		_ = s.stopLocked()
		return nil, errors.New("could not serve the workspace (another server?)")
	}
	if err := s.reconnectionDB(context.Background()); err != nil {
		log.Println("headless server:", err) // the catalog says "not connected"
	}
	return s, nil
}

// Resume serves the workspace again after a release (the editor's server
// is gone), with the connection and schema cache it kept: the config
// re-read if it changed, the workspace's remembered connection followed,
// the cache refreshed in the background if DDL ran meanwhile.
func (s *Server) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.control != nil {
		return nil
	}
	ctx := context.Background()
	if _, err := s.reloadConfigIfChanged(ctx); err != nil {
		log.Println("resume: config:", err)
	}
	if alias := state.Connection(s.workspace); alias != "" && (s.curDBCfg == nil || s.curDBCfg.Alias != alias) {
		if _, err := s.switchTo(ctx, alias); err != nil {
			log.Println("resume: connect", alias+":", err)
		}
	}
	s.startControl()
	if s.control == nil {
		return errors.New("could not serve the workspace (another server?)")
	}
	if ranDDLSince(s.workspace, s.releasedAt) {
		go func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, err := s.refreshCache(context.Background()); err != nil {
				log.Println("resume: refresh:", err)
			}
		}()
	}
	return nil
}

// ddlStatement: a statement that changes the schema.
var ddlStatement = regexp.MustCompile(`(?im)^\s*(create|alter|drop|rename|comment\s+on)\b`)

// ranDDLSince: the workspace's history has DDL run after t.
func ranDDLSince(workspace string, t time.Time) bool {
	entries, err := history.Find(workspace, history.Query{Limit: 200})
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Time.After(t) && e.Status == "ok" && ddlStatement.MatchString(e.SQL) {
			return true
		}
	}
	return false
}

// Released reports whether the server has given its socket away.
func (s *Server) Released() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headless && s.control == nil
}

// stampFile updates the connection header of a file the CLI acted from
// (`connect/db/schema --file`), if the editor has it open.
func (s *Server) stampFile(path string) {
	if path == "" {
		return
	}
	if uri, ok := s.openURI(path); ok {
		s.stampHeader(uri)
	}
}

func unmarshal(req *jsonrpc2.Request, v interface{}) error {
	if req.Params == nil {
		return nil
	}
	return json.Unmarshal(*req.Params, v)
}

// notifyEditor shows a status-line message in the editor (CLI actions).
func (s *Server) notifyEditor(msg string) {
	if s.editor != nil {
		_ = lsp.NewMessenger(s.editor).ShowInfo(context.Background(), msg)
	}
}

func (s *Server) notifyEditorError(msg string) {
	if s.editor != nil {
		_ = lsp.NewMessenger(s.editor).ShowError(context.Background(), msg)
	}
}

func (s *Server) controlRun(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	// Errors are also shown in the editor: the CLI may run detached from a
	// keybinding, with nowhere to print (job errors are reported by the job).
	fail := func(err error) (interface{}, error) {
		s.notifyEditorError(err.Error())
		return nil, err
	}
	var p control.RunParams
	if err := unmarshal(req, &p); err != nil {
		return fail(err)
	}
	rr := runRequest{
		kind: jobPreview, limit: p.Limit, sql: p.SQL, file: p.File, line: p.Line, col: p.Col,
		out: p.Out, fromCLI: true,
	}
	switch {
	case p.Dump:
		if p.Out == "" {
			return fail(errors.New("dump: no output path"))
		}
		rr.kind, rr.dump, rr.fresh, rr.limit = jobExport, true, p.Fresh, 0
	case p.Table:
		rr.kind = jobText
	case p.All:
		rr.kind = jobExport
	}
	if p.SQL == "" && p.File == "" {
		return fail(errors.New("nothing to run: pass SQL on stdin or --sql, or --file and --line"))
	}

	s.mu.Lock()
	j, err := s.startRun(ctx, rr)
	s.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	// Wait without holding s.mu; if the CLI goes away (Ctrl-C), cancel.
	select {
	case <-j.done:
	case <-conn.DisconnectNotify():
		j.stop("CLI disconnected")
		<-j.done
	}
	if j.err != nil {
		return nil, j.err
	}
	return j.result, nil
}

func (s *Server) controlStatus() control.Status {
	st := control.Status{Workspace: s.workspace, PID: os.Getpid(), Connected: s.dbConn != nil}
	if s.control != nil {
		st.Socket = s.control.Addr().String()
	}
	if s.curDBCfg != nil {
		st.Connection = &control.Connection{
			Index: s.curConnectionIndex + 1, Alias: s.curDBCfg.Alias, Driver: string(s.curDBCfg.Driver),
			Desc: database.DescribeConnection(s.curDBCfg), Current: true, Environment: s.curDBCfg.Env(),
		}
		st.ResultsDir = exec.ExpandDir(s.getConfig().Results.Dir, s.workspace, connAlias(s.curDBCfg))
		st.Schema = s.currentSchema()
		st.Database = s.curDBCfg.DBName
	}
	if j := s.runningJob(); j != nil {
		st.Job = j.title
	}
	if cfg := s.getConfig(); cfg != nil {
		st.Viewer = cfg.Results.Viewer
		st.Panel = &control.PanelSettings{
			Placement:          cfg.Panel.PlacementOrDefault(),
			InsertKeys:         cfg.Panel.InsertKeysOrDefault(),
			Keys:               keymaps(cfg.Panel.KeyMaps()),
			StatementCollapsed: cfg.Panel.StatementCollapsed(),
			ChainsAll:          cfg.Panel.ChainsAll(),
			Syntax:             cfg.Panel.Syntax,
			Icons:              cfg.Panel.Icons,
			QueriesDir:         cfg.TUI.QueriesDirIn(s.workspace),
			Editor:             cfg.TUI.EditorOrDefault(),
			ConfigPath:         s.editableConfigPath(),
		}
	}
	return st
}

func (s *Server) controlConnections() []control.Connection {
	var out []control.Connection
	cfg := s.getConfig()
	if cfg == nil {
		return out
	}
	for i, c := range cfg.Connections {
		db, schema := c.Target()
		var fields []control.Field
		for _, f := range c.Fields() {
			fields = append(fields, control.Field{Label: f.Label, Value: f.Value})
		}
		out = append(out, control.Connection{
			Index: i + 1, Alias: c.Alias, Driver: string(c.Driver), Desc: database.DescribeConnection(c),
			Current: i == s.curConnectionIndex && s.dbConn != nil, Environment: c.Env(),
			Group: strings.TrimSpace(c.Group), Database: db, Schema: schema, Fields: fields, Sample: c.SamplePolicy(),
		})
	}
	return out
}

// refreshCache reloads schema metadata for the current connection.
func (s *Server) refreshCache(ctx context.Context) (string, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	if err := s.worker.ReCache(ctx, repo); err != nil {
		return "", err
	}
	s.jsonKeys.Clear()
	return fmt.Sprintf("%s: schema cache refreshed", connAlias(s.curDBCfg)), nil
}

// catalog exposes the schema cache to the CLI pickers.
func (s *Server) catalog(all bool) (catalog.Source, catalog.Options, error) {
	cache := s.worker.Cache()
	if s.dbConn == nil || s.curDBCfg == nil || cache == nil {
		return nil, catalog.Options{}, errors.New("not connected")
	}
	return catalog.CacheSource{C: cache}, catalog.Options{
		Driver: s.curDBCfg.Driver, Lower: s.getConfig().LowercaseIdentifiers, All: all,
	}, nil
}

func keymaps(in map[string]config.Keymap) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for scope, km := range in {
		out[scope] = km
	}
	return out
}
