package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/state"
	"github.com/sourcegraph/jsonrpc2"
)

const (
	CommandExecuteQuery     = "executeQuery"
	CommandShowDatabases    = "showDatabases"
	CommandShowSchemas      = "showSchemas"
	CommandShowConnections  = "showConnections"
	CommandSwitchDatabase   = "switchDatabase"
	CommandSwitchSchema     = "switchSchema"
	CommandSwitchConnection = "switchConnections"
	CommandShowTables       = "showTables"
)

func (s *Server) handleTextDocumentCodeAction(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.CodeActionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	limit := s.getConfig().Results.Limit()
	commands := []lsp.Command{
		{
			Title:     fmt.Sprintf("Run → CSV (first %d rows)", limit),
			Command:   CommandRun,
			Arguments: []interface{}{params.TextDocument.URI, params.Range},
		},
		{
			Title:     "Export → CSV (all rows)",
			Command:   CommandExport,
			Arguments: []interface{}{params.TextDocument.URI, params.Range},
		},
		{
			Title:     fmt.Sprintf("Show results (text, first %d rows)", limit),
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{params.TextDocument.URI, params.Range},
		},
	}
	if hc, onHeader := s.headerActions(params.TextDocument.URI, params.Range); onHeader {
		commands = append(hc, commands...)
	} else {
		commands = append(commands, hc...)
	}
	if j := s.runningJob(); j != nil {
		commands = append([]lsp.Command{{
			Title:   "Cancel: " + j.title,
			Command: CommandCancel,
		}}, commands...)
	}
	commands = append(commands, []lsp.Command{
		{
			Title:     "Show Databases",
			Command:   CommandShowDatabases,
			Arguments: []interface{}{},
		},
		{
			Title:     "Show Schemas",
			Command:   CommandShowSchemas,
			Arguments: []interface{}{},
		},
		{
			Title:     "Show Connections",
			Command:   CommandShowConnections,
			Arguments: []interface{}{},
		},
		{
			Title:     "Switch Database",
			Command:   CommandSwitchDatabase,
			Arguments: []interface{}{},
		},
		{
			Title:     "Switch Connections",
			Command:   CommandSwitchConnection,
			Arguments: []interface{}{},
		},
		{
			Title:     "Show Tables",
			Command:   CommandShowTables,
			Arguments: []interface{}{},
		},
	}...)
	return commands, nil
}

func (s *Server) handleWorkspaceExecuteCommand(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.ExecuteCommandParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	switch params.Command {
	case CommandExecuteQuery, CommandRun, CommandExport:
		uri, rng, limit, vertical, perr := parseRunArgs(params.Arguments)
		if perr != nil {
			return nil, perr
		}
		if params.Range != nil {
			rng = params.Range
		}
		kind := map[string]jobKind{CommandExecuteQuery: jobText, CommandRun: jobPreview, CommandExport: jobExport}[params.Command]
		_, err = s.startRun(ctx, runRequest{kind: kind, limit: limit, uri: uri, rng: rng, vertical: vertical})
	case CommandRefresh:
		result, err = s.refreshCache(ctx)
	case CommandUseHeader, CommandStampHeader:
		uri, _ := firstString(params.Arguments)
		if uri == "" {
			uri = s.lastURI
		}
		if params.Command == CommandUseHeader {
			result, err = s.useHeader(ctx, uri)
		} else if _, ok := s.files[uri]; !ok {
			err = fmt.Errorf("document not open: %q", uri)
		} else {
			s.stampHeader(uri)
			result = "pointed the header at " + s.headerState().editor.String()
		}
	case CommandCancel:
		if j := s.runningJob(); j != nil {
			j.stop("by request")
			result = "cancelling: " + j.title
		} else {
			result = "no query is running"
		}
	case CommandShowDatabases:
		result, err = s.showDatabases(ctx, params)
	case CommandShowSchemas:
		result, err = s.showSchemas(ctx, params)
	case CommandShowConnections:
		result, err = s.showConnections(ctx, params)
	case CommandSwitchDatabase:
		result, err = s.switchDatabase(ctx, params)
	case CommandSwitchSchema:
		name, _ := firstString(params.Arguments)
		if name == "" {
			err = errors.New("required argument: the schema name")
		} else {
			result, err = s.useSchema(ctx, name)
		}
	case CommandSwitchConnection:
		result, err = s.switchConnections(ctx, params)
	case CommandShowTables:
		result, err = s.showTables(ctx, params)
	default:
		return nil, fmt.Errorf("unsupported command: %v", params.Command)
	}
	s.presentCommandResult(ctx, conn, params.Command, result, err)
	return result, err
}

// presentCommandResult pushes a command's outcome to the editor. Clients such as
// Helix discard the executeCommand response, so one-line results go to the
// status line (window/showMessage) and multi-line results are written to a file
// which the client is asked to open (window/showDocument).
func (s *Server) presentCommandResult(ctx context.Context, conn *jsonrpc2.Conn, command string, result interface{}, err error) {
	m := lsp.NewMessenger(conn)
	if err != nil {
		_ = m.ShowError(ctx, fmt.Sprintf("%s: %v", command, err))
		return
	}
	text, ok := result.(string)
	if !ok || strings.TrimSpace(text) == "" {
		if command == CommandSwitchConnection || command == CommandSwitchDatabase || command == CommandSwitchSchema {
			_ = m.ShowInfo(ctx, fmt.Sprintf("%s: done", command))
		}
		return
	}
	text = strings.TrimRight(text, "\n")
	if !strings.Contains(text, "\n") {
		_ = m.ShowInfo(ctx, text)
		return
	}
	dir := filepath.Join(os.TempDir(), "sqlh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_ = m.ShowError(ctx, err.Error())
		return
	}
	path := filepath.Join(dir, command+".txt")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
		_ = m.ShowError(ctx, err.Error())
		return
	}
	m.ShowDocument("file://"+path, true)
}

func firstString(args []interface{}) (string, bool) {
	if len(args) > 0 {
		if s, ok := args[0].(string); ok {
			return s, true
		}
	}
	return "", false
}

func rangeFromArg(v map[string]interface{}) (*lsp.Range, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var r lsp.Range
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("invalid range argument: %w", err)
	}
	return &r, nil
}

func extractRangeText(text string, startLine, startChar, endLine, endChar int) string {
	lines := strings.Split(text, "\n")
	if startLine < 0 {
		startLine = 0
	}
	if endLine >= len(lines) {
		endLine = len(lines) - 1
	}
	if startLine >= len(lines) || startLine > endLine {
		return ""
	}

	var builder strings.Builder
	for i := startLine; i <= endLine; i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		st, en := 0, utf16Len(line)
		if i == startLine {
			st = startChar
		}
		if i == endLine {
			en = endChar
		}
		builder.WriteString(sliceUTF16(line, st, en))
		if i != endLine {
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func utf16Len(s string) int {
	length := 0
	for _, r := range s {
		length++
		if r > utf8.MaxRune || r < 0 {
			continue
		}
		if r > 0xFFFF {
			length++
		}
	}
	return length
}

func sliceUTF16(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}

	utf16Pos := 0
	startByte, endByte := len(s), len(s)
	startFound, endFound := false, false

	for bytePos, r := range s {
		runeWidth := 1
		if r > 0xFFFF {
			runeWidth = 2
		}

		if !startFound && start <= utf16Pos {
			startByte = bytePos
			startFound = true
		}
		if !endFound && end <= utf16Pos {
			endByte = bytePos
			endFound = true
			break
		}

		nextUTF16Pos := utf16Pos + runeWidth
		if !startFound && start < nextUTF16Pos {
			startByte = bytePos
			startFound = true
		}
		if !endFound && end < nextUTF16Pos {
			endByte = bytePos + utf8.RuneLen(r)
			endFound = true
			break
		}

		utf16Pos = nextUTF16Pos
	}

	if !startFound && start <= utf16Pos {
		startByte = len(s)
	}
	if !endFound {
		endByte = len(s)
	}
	if startByte > endByte {
		startByte = endByte
	}
	return s[startByte:endByte]
}

func (s *Server) showDatabases(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	databases, err := repo.Databases(ctx)
	if err != nil {
		return nil, err
	}
	return strings.Join(databases, "\n"), nil
}

func (s *Server) showSchemas(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	schemas, err := repo.Schemas(ctx)
	if err != nil {
		return nil, err
	}
	return strings.Join(schemas, "\n"), nil
}

func (s *Server) switchDatabase(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	if len(params.Arguments) != 1 {
		return nil, fmt.Errorf("required arguments were not provided: <DB Name>")
	}
	dbName, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the db name as a string")
	}
	return s.useDatabase(ctx, dbName)
}

// useDatabase reconnects the current connection to another database
// (rolled back if the reconnect fails).
func (s *Server) useDatabase(ctx context.Context, dbName string) (string, error) {
	if s.curDBCfg != nil {
		switch s.curDBCfg.Driver {
		case dialect.DatabaseDriverOracle:
			return "", errors.New("Oracle has one database per connection; to work in another schema add it to the connection's `schemas:` list and switch to it (sqlh schema)")
		case dialect.DatabaseDriverSQLite3:
			return "", errors.New("SQLite has one database per file; add another connection instead")
		}
	}
	// a schema switch belongs to the previous database
	prev, prevSchema := s.curDBName, s.curSchema
	s.curDBName, s.curSchema = dbName, ""
	if err := s.reconnectionDB(ctx); err != nil {
		s.curDBName, s.curSchema = prev, prevSchema
		return "", err
	}
	s.relintOpen() // connection headers compare against the new target
	return fmt.Sprintf("switched database to %s", dbName), nil
}

// targetKey tells apart the databases and schemas of a connection (result
// reuse, executed DDL): "" when neither is switched.
func (s *Server) targetKey() string {
	if s.curSchema == "" {
		return s.curDBName
	}
	return s.curDBName + "\x1f" + s.curSchema
}

// currentSchema is the connection's current schema (as the cache knows it).
func (s *Server) currentSchema() string {
	if s.dbConn == nil {
		return ""
	}
	if c := s.worker.Cache(); c != nil {
		return c.DefaultSchema()
	}
	return ""
}

// schemaChoices lists the schemas the connection can switch to: its
// `schemas:` list; PostgreSQL without one: the database's schemas. The
// current one is marked Default.
func (s *Server) schemaChoices() ([]control.SchemaItem, error) {
	if s.dbConn == nil || s.curDBCfg == nil {
		return nil, ErrNoConnection
	}
	cfg := s.curDBCfg
	if !cfg.SupportsSchema() {
		return nil, fmt.Errorf("%s connections have no current schema to switch", cfg.Driver)
	}
	cur := s.currentSchema()
	var names []string
	switch {
	case len(cfg.Schemas) > 0:
		for _, n := range cfg.Schemas {
			if a, ok := cfg.AllowedSchema(n); ok {
				names = append(names, a)
			}
		}
	case cfg.Driver == dialect.DatabaseDriverPostgreSQL:
		src, opt, err := s.catalog(false)
		if err != nil {
			return nil, err
		}
		for _, it := range catalog.Schemas(src, opt) {
			names = append(names, it.Name)
		}
	default: // Oracle without a list: the login schema only
		names = []string{cur}
	}
	var out []control.SchemaItem
	seen := map[string]bool{}
	for _, n := range names {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, control.SchemaItem{Name: n, Default: n == cur})
		}
	}
	return out, nil
}

// useSchema makes another schema current: the connection is reopened with
// it (rolled back if that fails), the cache rebuilt for it.
func (s *Server) useSchema(ctx context.Context, name string) (string, error) {
	choices, err := s.schemaChoices()
	if err != nil {
		return "", err
	}
	target := ""
	var names []string
	for _, c := range choices {
		names = append(names, c.Name)
		if c.Name == name || s.curDBCfg.Driver == dialect.DatabaseDriverOracle && strings.EqualFold(c.Name, name) {
			target = c.Name
		}
	}
	if target == "" {
		return "", fmt.Errorf("schema %q is not one of %s (the connection's `schemas:`)", name, strings.Join(names, ", "))
	}
	if target == s.currentSchema() {
		return "already on schema " + target, nil
	}
	override := target
	if target == s.defaultSchema {
		override = "" // back to the connection's own
	}
	prev := s.curSchema
	s.curSchema = override
	if err := s.reconnectionDB(ctx); err != nil {
		s.curSchema = prev
		return "", err
	}
	s.relintOpen() // connection headers compare against the new target
	return "switched schema to " + target, nil
}

func (s *Server) showConnections(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	results := []string{}
	conns := s.getConfig().Connections
	for i, conn := range conns {
		mark := " "
		if i == s.curConnectionIndex && s.dbConn != nil {
			mark = "*"
		}
		res := fmt.Sprintf("%s %d %-10s %-12s %s", mark, i+1, conn.Driver, conn.Alias, database.DescribeConnection(conn))
		results = append(results, strings.TrimRight(res, " "))
	}
	return strings.Join(results, "\n"), nil
}

func (s *Server) switchConnections(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	if len(params.Arguments) != 1 {
		return nil, fmt.Errorf("required arguments were not provided: <Connection Index>")
	}
	name, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the connection alias or index as a string")
	}
	return s.switchTo(ctx, name)
}

// connectionIndex resolves an alias or 1-based index to a 0-based index.
func (s *Server) connectionIndex(name string) (int, error) {
	cfg := s.getConfig()
	if cfg != nil {
		for i, conn := range cfg.Connections {
			if conn.Alias == name {
				return i, nil
			}
		}
	}
	if n, err := strconv.Atoi(name); err == nil && cfg != nil && n >= 1 && n <= len(cfg.Connections) {
		return n - 1, nil
	}
	aliases := []string{}
	if cfg != nil {
		for i, conn := range cfg.Connections {
			if conn.Alias != "" {
				aliases = append(aliases, conn.Alias)
			} else {
				aliases = append(aliases, strconv.Itoa(i+1))
			}
		}
	}
	return 0, fmt.Errorf("unknown connection %q (available: %s)", name, strings.Join(aliases, ", "))
}

// switchTo switches to a connection by alias or 1-based index and remembers
// the choice for this workspace.
func (s *Server) switchTo(ctx context.Context, name string) (string, error) {
	index, err := s.connectionIndex(name)
	if err != nil {
		return "", err
	}
	// Reconnect database (rolled back if the reconnect fails). The database
	// override from switchDatabase belongs to the previous connection.
	prevIndex, prevDBName, prevSchema := s.curConnectionIndex, s.curDBName, s.curSchema
	s.curConnectionIndex, s.curDBName, s.curSchema = index, "", ""
	if err := s.reconnectionDB(ctx); err != nil {
		s.curConnectionIndex, s.curDBName, s.curSchema = prevIndex, prevDBName, prevSchema
		return "", err
	}
	if s.curDBCfg != nil && s.curDBCfg.Alias != "" {
		if err := state.SetConnection(s.workspace, s.curDBCfg.Alias); err != nil {
			log.Println("save state:", err)
		}
	}
	s.relintOpen() // connection headers compare against the new target
	return "switched connection to " + connectionLabel(index, s.curDBCfg), nil
}

func connectionLabel(index int, cfg *database.DBConfig) string {
	if cfg == nil {
		return strconv.Itoa(index + 1)
	}
	if cfg.Alias != "" {
		return fmt.Sprintf("%s (%s)", cfg.Alias, cfg.Driver)
	}
	return fmt.Sprintf("%d (%s)", index+1, cfg.Driver)
}

func (s *Server) showTables(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	m, err := repo.SchemaTables(ctx)
	if err != nil {
		return nil, err
	}
	schema, err := repo.CurrentSchema(ctx)
	if err != nil {
		return nil, err
	}
	results := []string{}
	for k, vv := range m {
		for _, v := range vv {
			if k != "" {
				if schema != k {
					continue
				}
				results = append(results, k+"."+v)
			} else {
				results = append(results, v)
			}
		}
	}
	return strings.Join(results, "\n"), nil
}
