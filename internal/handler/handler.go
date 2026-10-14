package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/jsoncomplete"
	"github.com/raikhan/sqlh/internal/lsp"
)

var (
	ErrNoConnection = errors.New("no database connection")
)

type Server struct {
	SpecificFileCfg *config.Config
	DefaultFileCfg  *config.Config
	WSCfg           *config.Config

	dbConn *database.DBConnection

	curDBCfg           *database.DBConfig
	curDBName          string
	curConnectionIndex int

	// The initOptionDBConfig is an optional param
	// sent by the client as part of the LSP InitializationOptions
	// payload. If non-nil, the server will ignore all
	// other configuration sources (workspace and user).
	initOptionDBConfig *database.DBConfig

	worker *database.Worker
	files  map[string]*File

	// lastURI is the most recently used document; used when a workspace command
	// is issued without a document argument (e.g. Helix :lsp-workspace-command).
	lastURI string

	// workspace is the client's root directory (initialize rootUri).
	workspace string
	// clientProgress: the client supports window/workDoneProgress.
	clientProgress bool

	jobs jobState

	// mu serialises request handling: the editor connection and CLI
	// connections (control socket) are served on different goroutines.
	mu sync.Mutex
	// editor is the LSP client connection (for messages from jobs/CLI runs).
	editor *jsonrpc2.Conn
	// control is the CLI socket listener (nil if not listening).
	control net.Listener
	// jsonKeys caches sampled JSON keys (cleared on switch/refresh).
	jsonKeys jsoncomplete.Cache
	// lint schedules diagnostics.
	lint lintState
}

type File struct {
	LanguageID string
	Text       string
	// Version: the client's document version (didOpen/didChange), echoed in
	// versioned workspace edits so the client accepts them.
	Version int
}

func NewServer() *Server {
	worker := database.NewWorker()
	worker.Start()

	s := &Server{
		files:    make(map[string]*File),
		worker:   worker,
		jsonKeys: jsoncomplete.Cache{TTL: 10 * time.Minute},
	}
	worker.OnChange = s.relintAll
	return s
}

func panicf(r interface{}, format string, v ...interface{}) error {
	if r != nil {
		// Same as net/http
		const size = 64 << 10
		buf := make([]byte, size)
		buf = buf[:runtime.Stack(buf, false)]
		id := fmt.Sprintf(format, v...)
		log.Printf("panic serving %s: %v\n%s", id, r, string(buf))
		return fmt.Errorf("unexpected panic: %v", r)
	}
	return nil
}

// Stop shuts the server down (from outside request handling).
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked()
}

// stopLocked shuts the server down; s.mu must be held.
func (s *Server) stopLocked() error {
	s.stopControl()
	s.stopJob("server stopping")
	s.closeSession()
	if s.dbConn != nil {
		err := s.dbConn.Close()
		s.dbConn = nil // Stop may run twice (exit, then main)
		if err != nil {
			return err
		}
	}
	s.worker.Stop()
	return nil
}

func (s *Server) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	// Prevent any uncaught panics from taking the entire server down.
	defer func() {
		if perr := panicf(recover(), "%v", req.Method); perr != nil {
			err = perr
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Method == "initialize" {
		s.editor = conn
	}
	res, err := s.handle(ctx, conn, req)
	if err != nil {
		log.Printf("error serving, %+v\n", err)
	}
	return res, err
}
func (s *Server) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	s.trackActiveDocument(req)
	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, conn, req)
	case "initialized":
		return
	case "shutdown":
		return s.handleShutdown(ctx, conn, req)
	case "exit":
		return s.handleExit(ctx, conn, req)
	case "textDocument/didOpen":
		return s.handleTextDocumentDidOpen(ctx, conn, req)
	case "textDocument/didChange":
		return s.handleTextDocumentDidChange(ctx, conn, req)
	case "textDocument/didSave":
		return s.handleTextDocumentDidSave(ctx, conn, req)
	case "textDocument/didClose":
		return s.handleTextDocumentDidClose(ctx, conn, req)
	case "textDocument/completion":
		return s.handleTextDocumentCompletion(ctx, conn, req)
	case "textDocument/hover":
		return s.handleTextDocumentHover(ctx, conn, req)
	case "textDocument/codeAction":
		return s.handleTextDocumentCodeAction(ctx, conn, req)
	case "workspace/executeCommand":
		return s.handleWorkspaceExecuteCommand(ctx, conn, req)
	case "workspace/didChangeConfiguration":
		return s.handleWorkspaceDidChangeConfiguration(ctx, conn, req)
	case "textDocument/formatting":
		return s.handleTextDocumentFormatting(ctx, conn, req)
	case "textDocument/rangeFormatting":
		return s.handleTextDocumentRangeFormatting(ctx, conn, req)
	case "textDocument/signatureHelp":
		return s.handleTextDocumentSignatureHelp(ctx, conn, req)
	case "textDocument/rename":
		return s.handleTextDocumentRename(ctx, conn, req)
	case "textDocument/definition":
		return s.handleDefinition(ctx, conn, req)
	case "textDocument/typeDefinition":
		return s.handleDefinition(ctx, conn, req)
	case "textDocument/references":
		return s.handleReferences(ctx, conn, req)
	case "window/showMessage":
		return
	}
	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: fmt.Sprintf("method not supported: %s", req.Method)}
}

func (s *Server) handleInitialize(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.InitializeParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	result = lsp.InitializeResult{
		Capabilities: lsp.ServerCapabilities{
			TextDocumentSync:   lsp.TDSKFull,
			HoverProvider:      true,
			CodeActionProvider: true,
			CompletionProvider: &lsp.CompletionOptions{
				TriggerCharacters: []string{"(", ".", "'"},
			},
			SignatureHelpProvider: &lsp.SignatureHelpOptions{
				TriggerCharacters:   []string{"(", ","},
				RetriggerCharacters: []string{"(", ","},
				WorkDoneProgressOptions: lsp.WorkDoneProgressOptions{
					WorkDoneProgress: false,
				},
			},
			DefinitionProvider:              true,
			ReferencesProvider:              true,
			DocumentFormattingProvider:      true,
			DocumentRangeFormattingProvider: true,
			RenameProvider:                  true,
			ExecuteCommandProvider: &lsp.ExecuteCommandOptions{
				Commands: []string{
					CommandRun,
					CommandExport,
					CommandCancel,
					CommandRefresh,
					CommandUseHeader,
					CommandStampHeader,
					CommandExecuteQuery,
					CommandShowDatabases,
					CommandShowSchemas,
					CommandShowConnections,
					CommandSwitchDatabase,
					CommandSwitchConnection,
					CommandShowTables,
				},
			},
		},
	}

	s.initOptionDBConfig = params.InitializationOptions.ConnectionConfig
	s.workspace = uriToPath(params.RootURI)
	if s.workspace == "" {
		s.workspace = params.RootPath
	}
	s.clientProgress = params.Capabilities.Window != nil && params.Capabilities.Window.WorkDoneProgress
	s.restoreConnection()
	s.startControl()

	// Initialize database database connection
	// NOTE: If no connection is found at this point, it is possible that the connection settings are sent to workspace config, so don't make an error
	messenger := lsp.NewMessenger(conn)
	if err := s.reconnectionDB(ctx); err != nil {
		if errors.Is(err, ErrNoConnection) {
			if err := messenger.ShowInfo(ctx, err.Error()); err != nil {
				log.Println("send info", err.Error())
				return nil, err
			}
		} else {
			log.Println("send err", err.Error())
			if err := messenger.ShowError(ctx, err.Error()); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func (s *Server) handleShutdown(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	s.stopControl()
	s.stopJob("server shutting down")
	s.closeSession()
	if s.dbConn != nil {
		s.dbConn.Close()
	}
	return nil, nil
}

func (s *Server) handleExit(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	err = s.stopLocked()
	return nil, err
}

func (s *Server) handleTextDocumentDidOpen(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidOpenTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if err := s.openFile(params.TextDocument.URI, params.TextDocument.LanguageID); err != nil {
		return nil, err
	}
	if err := s.updateFile(params.TextDocument.URI, params.TextDocument.Text); err != nil {
		return nil, err
	}
	s.files[params.TextDocument.URI].Version = params.TextDocument.Version
	s.scheduleLint(params.TextDocument.URI)
	return nil, nil
}

func (s *Server) handleTextDocumentDidChange(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidChangeTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if len(params.ContentChanges) == 0 {
		return nil, nil
	}
	if err := s.updateFile(params.TextDocument.URI, params.ContentChanges[0].Text); err != nil {
		return nil, err
	}
	s.files[params.TextDocument.URI].Version = params.TextDocument.Version
	s.scheduleLint(params.TextDocument.URI)
	return nil, nil
}

func (s *Server) handleTextDocumentDidSave(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidSaveTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if params.Text != "" {
		err = s.updateFile(params.TextDocument.URI, params.Text)
	} else {
		err = s.saveFile(params.TextDocument.URI)
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidClose(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidCloseTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if err := s.closeFile(params.TextDocument.URI); err != nil {
		return nil, err
	}
	s.clearLint(params.TextDocument.URI)
	return nil, nil
}

func (s *Server) openFile(uri string, languageID string) error {
	f := &File{
		Text:       "",
		LanguageID: languageID,
	}
	s.files[uri] = f
	return nil
}

func (s *Server) closeFile(uri string) error {
	delete(s.files, uri)
	return nil
}

func (s *Server) updateFile(uri string, text string) error {
	f, ok := s.files[uri]
	if !ok {
		return fmt.Errorf("document not found: %v", uri)
	}
	f.Text = text
	return nil
}

func (s *Server) saveFile(uri string) error {
	return nil
}

func (s *Server) handleWorkspaceDidChangeConfiguration(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	// Update changed configuration
	var params lsp.DidChangeConfigurationParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	s.WSCfg = params.Settings.SQLS

	// Skip database connection
	if s.dbConn != nil {
		return nil, nil
	}

	// Initialize database database connection
	messenger := lsp.NewMessenger(conn)
	if err := s.reconnectionDB(ctx); err != nil {
		if errors.Is(err, ErrNoConnection) {
			if err := messenger.ShowInfo(ctx, err.Error()); err != nil {
				log.Println("send info", err.Error())
				return nil, err
			}
		} else {
			log.Println("send err", err.Error())
			if err := messenger.ShowError(ctx, err.Error()); err != nil {
				return nil, err
			}
		}
	}

	return nil, nil
}

// reconnectionDB connects using the current connection settings and rebuilds
// the schema cache. It is all-or-nothing: the previous connection, config and
// cache stay active until the new connection is open and cached, so a failed
// switch leaves the editor on the old database.
func (s *Server) reconnectionDB(ctx context.Context) error {
	prevConn, prevCfg := s.dbConn, s.curDBCfg

	dbConn, cfg, err := s.newDBConnection(ctx)
	if err != nil {
		return err
	}
	dbRepo, err := newRepository(cfg, dbConn)
	if err != nil {
		_ = dbConn.Close()
		return err
	}
	if err := s.worker.ReCache(ctx, dbRepo); err != nil {
		_ = dbConn.Close()
		if prevConn != nil && prevCfg != nil {
			// restore the previous connection's repository in the worker
			if prevRepo, perr := newRepository(prevCfg, prevConn); perr == nil {
				s.worker.SetRepository(prevRepo)
			}
		}
		return err
	}

	// the running query and the pinned session belong to the old connection
	s.stopJob("connection switched")
	s.jsonKeys.Clear()
	s.closeSession()
	s.dbConn, s.curDBCfg = dbConn, cfg
	if prevConn != nil {
		if err := prevConn.Close(); err != nil {
			log.Println("close previous connection:", err)
		}
	}
	return nil
}

// newDBConnection opens a connection for the current connection index / db
// name without touching server state.
func (s *Server) newDBConnection(ctx context.Context) (*database.DBConnection, *database.DBConfig, error) {
	// Get the most preferred DB connection settings
	connCfg := s.topConnection()
	if connCfg == nil {
		return nil, nil, ErrNoConnection
	}
	if s.curConnectionIndex != 0 {
		connCfg = s.getConnection(s.curConnectionIndex)
	}
	if connCfg == nil {
		return nil, nil, fmt.Errorf("not found database connection config, index %d", s.curConnectionIndex+1)
	}
	if s.curDBName != "" {
		// copy: don't mutate the loaded configuration
		c := *connCfg
		c.DBName = s.curDBName
		connCfg = &c
	}

	conn, err := database.Open(connCfg)
	if err != nil {
		return nil, nil, err
	}
	return conn, connCfg, nil
}

func newRepository(cfg *database.DBConfig, conn *database.DBConnection) (database.DBRepository, error) {
	repo, err := database.CreateRepository(cfg.Driver, conn.Conn)
	if err != nil {
		return nil, err
	}
	if f, ok := repo.(database.SchemaFilterer); ok {
		f.SetSchemaFilter(cfg.Schemas)
	}
	return repo, nil
}

func (s *Server) newDBRepository(ctx context.Context) (database.DBRepository, error) {
	if s.curDBCfg == nil || s.dbConn == nil {
		return nil, ErrNoConnection
	}
	return newRepository(s.curDBCfg, s.dbConn)
}

func (s *Server) topConnection() *database.DBConfig {
	// if the init config is set, ignore all other connection configs
	if s.initOptionDBConfig != nil {
		return s.initOptionDBConfig
	}

	cfg := s.getConfig()
	if cfg == nil || len(cfg.Connections) == 0 {
		return nil
	}
	return cfg.Connections[0]
}

func (s *Server) getConnection(index int) *database.DBConfig {
	cfg := s.getConfig()
	if cfg == nil || index < 0 || len(cfg.Connections) <= index {
		return nil
	}
	return cfg.Connections[index]
}

func (s *Server) getConfig() *config.Config {
	var cfg *config.Config
	switch {
	case validConfig(s.SpecificFileCfg):
		cfg = s.SpecificFileCfg
	case validConfig(s.WSCfg):
		cfg = s.WSCfg
	case validConfig(s.DefaultFileCfg):
		cfg = s.DefaultFileCfg
	default:
		cfg = config.NewConfig()
	}
	return cfg
}

func validConfig(cfg *config.Config) bool {
	// if cfg != nil && len(cfg.Connections) > 0 {
	if cfg != nil {
		return true
	}
	return false
}

// trackActiveDocument remembers the document of the latest textDocument/* request.
func (s *Server) trackActiveDocument(req *jsonrpc2.Request) {
	if req.Params == nil || !strings.HasPrefix(req.Method, "textDocument/") || req.Method == "textDocument/didClose" {
		return
	}
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(*req.Params, &p); err == nil && p.TextDocument.URI != "" {
		s.lastURI = p.TextDocument.URI
	}
}
