package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/exec"
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
	l, err := control.Listen(s.workspace)
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
			jsonrpc2.NewConn(context.Background(),
				jsonrpc2.NewBufferedStream(nc, jsonrpc2.VSCodeObjectCodec{}),
				jsonrpc2.AsyncHandler(jsonrpc2.HandlerWithError(s.handleControl)))
		}
	}()
}

func (s *Server) stopControl() {
	if s.control != nil {
		_ = s.control.Close()
		control.Cleanup(s.workspace)
		s.control = nil
	}
}

// handleControl serves CLI requests. Like editor requests they run under
// s.mu, except that run waits for its job without holding the lock.
func (s *Server) handleControl(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	defer func() {
		if perr := panicf(recover(), "%v", req.Method); perr != nil {
			err = perr
		}
	}()
	if req.Method == control.MethodRun {
		return s.controlRun(ctx, conn, req)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch req.Method {
	case control.MethodStatus:
		return s.controlStatus(), nil
	case control.MethodConnections:
		return s.controlConnections(), nil
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
	case control.MethodUseDatabase:
		var p control.DatabaseParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		msg, err := s.useDatabase(ctx, p.Name)
		if err != nil {
			return nil, err
		}
		s.stampFile(p.File)
		s.notifyEditor(msg)
		return msg, nil
	}
	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: "unknown method " + req.Method}
}

// stampFile updates the connection header of a file the CLI acted from
// (`connect/db --file`), if the editor has it open.
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
			Desc: database.DescribeConnection(s.curDBCfg), Current: true,
		}
		st.ResultsDir = exec.ExpandDir(s.getConfig().Results.Dir, s.workspace, connAlias(s.curDBCfg))
	}
	if j := s.runningJob(); j != nil {
		st.Job = j.title
	}
	if cfg := s.getConfig(); cfg != nil {
		st.Viewer = cfg.Results.Viewer
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
		out = append(out, control.Connection{
			Index: i + 1, Alias: c.Alias, Driver: string(c.Driver), Desc: database.DescribeConnection(c),
			Current: i == s.curConnectionIndex && s.dbConn != nil,
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
