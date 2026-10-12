package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/sourcegraph/jsonrpc2"
)

const (
	CommandExecuteQuery     = "executeQuery"
	CommandShowDatabases    = "showDatabases"
	CommandShowSchemas      = "showSchemas"
	CommandShowConnections  = "showConnections"
	CommandSwitchDatabase   = "switchDatabase"
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

	commands := []lsp.Command{
		{
			Title:     "Execute Query",
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{params.TextDocument.URI, params.Range},
		},
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
	}
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
	case CommandExecuteQuery:
		result, err = s.executeQuery(ctx, params)
	case CommandShowDatabases:
		result, err = s.showDatabases(ctx, params)
	case CommandShowSchemas:
		result, err = s.showSchemas(ctx, params)
	case CommandShowConnections:
		result, err = s.showConnections(ctx, params)
	case CommandSwitchDatabase:
		result, err = s.switchDatabase(ctx, params)
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
		if command == CommandSwitchConnection || command == CommandSwitchDatabase {
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

func (s *Server) executeQuery(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	if s.dbConn == nil {
		return nil, errors.New("database connection is not open")
	}

	// Arguments: [<File URI>?, <Range>?, "-show-vertical"?]
	// The URI defaults to the most recently used document.
	uri := s.lastURI
	showVertical := false
	var rng *lsp.Range
	for _, arg := range params.Arguments {
		switch v := arg.(type) {
		case string:
			if v == "-show-vertical" {
				showVertical = true
			} else {
				uri = v
			}
		case map[string]interface{}:
			r, err := rangeFromArg(v)
			if err != nil {
				return nil, err
			}
			rng = r
		}
	}
	if params.Range != nil {
		rng = params.Range
	}
	if uri == "" {
		return nil, fmt.Errorf("required arguments were not provided: <File URI>")
	}
	f, ok := s.files[uri]
	if !ok {
		return nil, fmt.Errorf("document not found, %q", uri)
	}

	// extract target statements: explicit selection, else statement under
	// cursor, else whole document
	dialectOf := sqlsplit.DialectOf(string(s.curDBCfg.Driver))
	var stmts []sqlsplit.Statement
	if rng != nil {
		selected := extractRangeText(f.Text, rng.Start.Line, rng.Start.Character, rng.End.Line, rng.End.Character)
		// Helix's normal-mode cursor is a 1-character selection
		if len(strings.TrimSpace(selected)) > 1 {
			stmts = sqlsplit.Split(selected, dialectOf)
		} else {
			all := sqlsplit.Split(f.Text, dialectOf)
			st, ok := sqlsplit.AtCursor(f.Text, all, sqlsplit.Position{Line: rng.Start.Line, Char: rng.Start.Character})
			if !ok {
				return nil, fmt.Errorf("no statement at cursor (line %d)", rng.Start.Line+1)
			}
			stmts = []sqlsplit.Statement{st}
		}
	} else {
		stmts = sqlsplit.Split(f.Text, dialectOf)
	}
	if len(stmts) == 0 {
		return nil, errors.New("no statement to execute")
	}

	// execute statements
	buf := new(bytes.Buffer)
	for _, stmt := range stmts {
		verb := stripLeadingComments(stmt.SQL)
		if verb == "" {
			continue
		}
		var (
			res string
			err error
		)
		switch _, isQuery := database.QueryExecType(verb, verb); {
		case stmt.PLSQL:
			// rows-affected is meaningless for PL/SQL units
			if _, err = s.exec(ctx, stmt.SQL, showVertical); err == nil {
				res = "PL/SQL block completed"
			}
		case isQuery:
			res, err = s.query(ctx, stmt.SQL, showVertical)
		default:
			res, err = s.exec(ctx, stmt.SQL, showVertical)
		}
		if err != nil {
			if len(stmts) > 1 {
				return nil, fmt.Errorf("line %d: %w", stmt.Start.Line+1, err)
			}
			return nil, err
		}
		fmt.Fprintln(buf, res)
	}
	return buf.String(), nil
}

// stripLeadingComments removes leading "--" line comments and "/* */" block
// comments so the first word of the statement is the verb (SELECT, INSERT, ...).
func stripLeadingComments(q string) string {
	for {
		q = strings.TrimSpace(q)
		switch {
		case strings.HasPrefix(q, "--"):
			if i := strings.IndexByte(q, '\n'); i >= 0 {
				q = q[i+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(q, "/*"):
			if i := strings.Index(q, "*/"); i >= 0 {
				q = q[i+2:]
			} else {
				return ""
			}
		default:
			return q
		}
	}
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

func (s *Server) query(ctx context.Context, query string, vertical bool) (string, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	rows, err := repo.Query(ctx, query)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	columns, err := database.Columns(rows)
	if err != nil {
		return "", err
	}
	stringRows, err := database.ScanRows(rows, len(columns))
	if err != nil {
		return "", err
	}

	buf := new(bytes.Buffer)
	if vertical {
		table := newVerticalTableWriter(buf)
		table.setHeaders(columns)
		for _, stringRow := range stringRows {
			table.appendRow(stringRow)
		}
		table.render()
	} else {
		table := tablewriter.NewTable(buf, tablewriter.WithHeaderConfig(tw.CellConfig{
			Formatting: tw.CellFormatting{AutoFormat: tw.Off},
		}))
		// Convert []string to []any for Header
		headers := make([]any, len(columns))
		for i, v := range columns {
			headers[i] = v
		}
		table.Header(headers...)
		for _, stringRow := range stringRows {
			// Convert []string to []any for Append
			row := make([]any, len(stringRow))
			for i, v := range stringRow {
				row[i] = v
			}
			if err := table.Append(row...); err != nil {
				return "", err
			}
		}
		if err := table.Render(); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(buf, "%d rows in set", len(stringRows))
	fmt.Fprintln(buf, "")
	fmt.Fprintln(buf, "")
	return buf.String(), nil
}

func (s *Server) exec(ctx context.Context, query string, vertical bool) (string, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	result, err := repo.Exec(ctx, query)
	if err != nil {
		return "", err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return "", err
	}

	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "Query OK, %d row affected", rowsAffected)
	fmt.Fprintln(buf, "")
	fmt.Fprintln(buf, "")
	return buf.String(), nil
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

	// Change current database (rolled back if the reconnect fails)
	prev := s.curDBName
	s.curDBName = dbName
	if err := s.reconnectionDB(ctx); err != nil {
		s.curDBName = prev
		return nil, err
	}

	return fmt.Sprintf("switched database to %s", dbName), nil
}

func (s *Server) showConnections(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	results := []string{}
	conns := s.getConfig().Connections
	for i, conn := range conns {
		mark := " "
		if i == s.curConnectionIndex && s.dbConn != nil {
			mark = "*"
		}
		res := fmt.Sprintf("%s %d %-10s %-12s %s", mark, i+1, conn.Driver, conn.Alias, connectionDesc(conn))
		results = append(results, strings.TrimRight(res, " "))
	}
	return strings.Join(results, "\n"), nil
}

var dsnPasswordRe = regexp.MustCompile(`(?i)(password=)('[^']*'|\S+)|(://[^:/@]+:)([^@]+)(@)`)

// connectionDesc describes a connection without exposing passwords.
func connectionDesc(conn *database.DBConfig) string {
	if conn.DataSourceName != "" {
		return dsnPasswordRe.ReplaceAllString(conn.DataSourceName, "${1}${3}***${5}")
	}
	var desc string
	switch conn.Proto {
	case database.ProtoTCP:
		desc = fmt.Sprintf("tcp(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
	case database.ProtoUDP:
		desc = fmt.Sprintf("udp(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
	case database.ProtoUnix:
		desc = fmt.Sprintf("unix(%s)/%s", conn.Path, conn.DBName)
	case database.ProtoHTTP:
		desc = fmt.Sprintf("http(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
	default:
		switch {
		case conn.Path != "":
			desc = conn.Path
		case conn.Host != "":
			desc = conn.Host
			if conn.Port != 0 {
				desc += ":" + strconv.Itoa(conn.Port)
			}
			desc += "/" + conn.DBName
		default:
			desc = conn.DBName
		}
	}
	if conn.User != "" && desc != "" {
		desc = conn.User + "@" + desc
	}
	if len(conn.Schemas) > 0 {
		desc += " schemas=" + strings.Join(conn.Schemas, ",")
	}
	return desc
}

func (s *Server) switchConnections(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	if len(params.Arguments) != 1 {
		return nil, fmt.Errorf("required arguments were not provided: <Connection Index>")
	}
	indexStr, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the connection index as a number")
	}

	var index int

	cfg := s.getConfig()
	if cfg != nil {
		for i, conn := range cfg.Connections {
			if conn.Alias == indexStr {
				index = i + 1
				break
			}
		}
	}
	if index <= 0 {
		index, err = strconv.Atoi(indexStr)
		if err != nil {
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
			return nil, fmt.Errorf("unknown connection %q (available: %s)", indexStr, strings.Join(aliases, ", "))
		}
	}

	if index <= 0 {
		return nil, fmt.Errorf("specify the connection index as a number")
	}
	index = index - 1

	// Reconnect database (rolled back if the reconnect fails). The database
	// override from switchDatabase belongs to the previous connection.
	prevIndex, prevDBName := s.curConnectionIndex, s.curDBName
	s.curConnectionIndex, s.curDBName = index, ""
	if err := s.reconnectionDB(ctx); err != nil {
		s.curConnectionIndex, s.curDBName = prevIndex, prevDBName
		return nil, err
	}

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

type verticalTableWriter struct {
	writer       io.Writer
	headers      []string
	rows         [][]string
	headerMaxLen int
}

func newVerticalTableWriter(writer io.Writer) *verticalTableWriter {
	return &verticalTableWriter{
		writer: writer,
	}
}

func (vtw *verticalTableWriter) setHeaders(headers []string) {
	vtw.headers = headers
	for _, h := range headers {
		length := len(h)
		if vtw.headerMaxLen < length {
			vtw.headerMaxLen = length
		}
	}
}

func (vtw *verticalTableWriter) appendRow(row []string) {
	vtw.rows = append(vtw.rows, row)
}

func (vtw *verticalTableWriter) render() {
	for rowNum, row := range vtw.rows {
		fmt.Fprintf(vtw.writer, "***************************[ %d. row ]***************************", rowNum+1)
		fmt.Fprintln(vtw.writer, "")
		for colNum, col := range row {
			header := vtw.headers[colNum]

			padHeader := fmt.Sprintf("%"+strconv.Itoa(vtw.headerMaxLen)+"s", header)
			fmt.Fprintf(vtw.writer, "%s | %s", padHeader, col)
			fmt.Fprintln(vtw.writer, "")
		}
	}
}
