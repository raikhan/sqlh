package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/ddl"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
)

// Go to definition of a table or view: its definition lives in the database,
// so it is written to a read-only file (rewritten on every request) that the
// editor opens: <runtime dir>/views/<workspace key>/<connection>/<schema>.<name>.sql.
// Views of the current schema come from the schema cache; tables, and views
// of the other allowed schemas, are fetched (database.DefinitionFetcher)
// without holding the server lock, within definitionTimeout; if that fails,
// a table's definition is generated from the cache.

// definitionTimeout bounds the database work of one go to definition.
const definitionTimeout = 5 * time.Second

// defTarget is what go to definition opens: a table or view and, maybe, one
// of its columns.
type defTarget struct {
	schema string // the database's spelling; "" = the current schema
	name   string
	column string
	// view: a view of the current schema the cache has (no database needed)
	view *database.ViewDef
	// synonym: Oracle, a name that is no relation of the current schema but a
	// synonym, resolved when fetching
	synonym bool
}

// objectTarget finds the table or view at the position: a name (also
// schema.name, or the table of a DDL statement), or one of its columns
// (t.col, alias.col, or an unqualified column only one table of the
// statement has).
func objectTarget(text string, lpos lsp.Position, parsed ast.TokenList, pos token.Pos, nw *parseutil.NodeWalker, cache *database.DBCache, d sqlsplit.Dialect) *defTarget {
	if cache == nil {
		return nil
	}
	if t, ok := ddlTarget(text, lpos, cache, d); ok {
		return t
	}
	nodes := nw.CurNodeMatches(astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier, ast.TypeIdentifier}})
	ident, mem := findIdent(nodes)
	env, err := collectEnvironment(parsed, pos)
	if err != nil {
		env = &hoverEnvironment{}
	}
	// tableOf: a table of the statement by alias or name
	tableOf := func(ref string) *parseutil.TableInfo {
		for _, t := range env.tables {
			if strings.EqualFold(t.Alias, ref) || t.Alias == "" && strings.EqualFold(t.Name, ref) {
				return t
			}
		}
		return nil
	}
	switch {
	case mem != nil && (ident == nil || ident.NoQuoteString() == mem.ChildTok.NoQuoteString()):
		parent, child := mem.ParentTok.NoQuoteString(), mem.ChildTok.NoQuoteString()
		if t := tableOf(parent); t != nil {
			return newTarget(cache, t.DatabaseSchema, t.Name, child) // t.col
		}
		if _, ok := cache.Database(parent); ok {
			return newTarget(cache, parent, child, "") // schema.table
		}
	case mem != nil: // on the qualifier
		parent, child := mem.ParentTok.NoQuoteString(), mem.ChildTok.NoQuoteString()
		if t := tableOf(parent); t != nil {
			return newTarget(cache, t.DatabaseSchema, t.Name, "")
		}
		if _, ok := cache.Database(parent); ok {
			return newTarget(cache, parent, child, "")
		}
	case ident != nil:
		name := ident.NoQuoteString()
		if _, ok := cache.CanonicalTableName(name); ok {
			return newTarget(cache, "", name, "")
		}
		// a column of a table of the statement, unless another one has it too
		var hit *parseutil.TableInfo
		for _, t := range env.tables {
			cols, ok := tableColumns(cache, t.DatabaseSchema, t.Name)
			if !ok || !hasColumn(cols, name) {
				continue
			}
			if hit != nil && !(strings.EqualFold(hit.Name, t.Name) && strings.EqualFold(hit.DatabaseSchema, t.DatabaseSchema)) {
				return nil
			}
			hit = t
		}
		if hit != nil {
			return newTarget(cache, hit.DatabaseSchema, hit.Name, name)
		}
		if isSynonym(cache, name) {
			return &defTarget{name: name, synonym: true}
		}
	}
	return nil
}

// ddlTarget: a table or column a DDL statement (or DML tail) names at the
// position.
func ddlTarget(text string, lpos lsp.Position, cache *database.DBCache, d sqlsplit.Dialect) (*defTarget, bool) {
	st, ok := statementContaining(text, lpos, d)
	if !ok {
		return nil, false
	}
	a, _, ok := ddl.InStatement(st.SQL, d)
	if !ok || a.Kind == "" {
		return nil, false
	}
	p := toStatementPos(st, lpos)
	for _, r := range a.Refs {
		if !within(p, r.Name.From, r.Name.To) {
			continue
		}
		switch {
		case r.Kind.IsRelation() && r.Role != ddl.RoleCreate:
			return newTarget(cache, r.Name.Schema, r.Name.Name, ""), true
		case r.Kind == ddl.ObjColumn && r.Table != nil && (r.Role == ddl.RoleUse || r.Role == ddl.RoleDrop):
			return newTarget(cache, r.Table.Schema, r.Table.Name, r.Name.Name), true
		}
		return nil, true
	}
	return nil, false
}

// newTarget: schema.name with the cache's spellings; nil when the schema is
// not one the cache knows.
func newTarget(cache *database.DBCache, schema, name, column string) *defTarget {
	t := &defTarget{name: name, column: column}
	if schema != "" && !strings.EqualFold(schema, cache.DefaultSchema()) {
		s, ok := cache.Database(schema)
		if !ok {
			return nil
		}
		t.schema = s
		if cols, ok := cache.ColumnDatabase(s, name); ok && len(cols) > 0 {
			t.name = cols[0].Table
		}
		return t
	}
	if n, ok := cache.CanonicalTableName(name); ok {
		t.name = n
	}
	t.view = cache.Relations.View(t.name)
	return t
}

func tableColumns(cache *database.DBCache, schema, name string) ([]*database.ColumnDesc, bool) {
	if schema != "" {
		return cache.ColumnDatabase(schema, name)
	}
	return cache.ColumnDescs(name)
}

func hasColumn(cols []*database.ColumnDesc, name string) bool {
	for _, c := range cols {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// isSynonym: an Oracle synonym visible unqualified (the current schema's or
// PUBLIC), as loaded with the cache.
func isSynonym(cache *database.DBCache, name string) bool {
	for _, owner := range []string{cache.DefaultSchema(), "PUBLIC"} {
		for _, s := range cache.Synonyms[owner] {
			if strings.EqualFold(s, name) {
				return true
			}
		}
	}
	return false
}

// defDeps is what fetching a definition needs, captured under the server
// lock.
type defDeps struct {
	repo   database.DBRepository
	cache  *database.DBCache
	driver dialect.DatabaseDriver
	conn   string // the connection's alias (file path, header)
	// skipMetadata / failed: Oracle DBMS_METADATA refused on this connection
	skipMetadata bool
	failed       func()
}

// fetchDefinition gets the target's definition: a cached view as is, else
// from the database (an Oracle synonym resolved first), else generated from
// the cache.
func fetchDefinition(ctx context.Context, t *defTarget, deps defDeps) (*database.Definition, error) {
	if t.view != nil {
		g := deps.cache.Relations
		return &database.Definition{Schema: t.view.Schema, Name: t.view.Name, Kind: g.Kind(t.view.Name),
			View: t.view, DDL: t.view.CreateStatement()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, definitionTimeout)
	defer cancel()
	schema, name := t.schema, t.name
	if t.synonym {
		sr, ok := deps.repo.(database.SynonymResolver)
		if !ok || deps.repo == nil {
			return nil, nil
		}
		owner, target, err := sr.ResolveSynonym(ctx, name)
		if err != nil || owner == "" {
			return nil, err
		}
		s, ok := deps.cache.Database(owner)
		if !ok {
			return nil, nil // a schema outside the allow-list
		}
		schema, name = s, target
	}
	if schema == "" {
		schema = deps.cache.DefaultSchema()
	}
	var fetchErr error
	if f, ok := deps.repo.(database.DefinitionFetcher); ok && deps.repo != nil {
		d, err := f.Definition(ctx, schema, name, database.DefinitionOptions{SkipMetadata: deps.skipMetadata})
		if err == nil {
			if d.MetadataFailed && deps.failed != nil {
				deps.failed()
			}
			return d, nil
		}
		if errors.Is(err, database.ErrNoObject) {
			return nil, fmt.Errorf("%s.%s: %w", schema, name, err)
		}
		fetchErr = err
	}
	d, ok := database.GenerateDefinition(deps.cache, deps.driver, schema, name)
	if !ok {
		if fetchErr != nil {
			return nil, fetchErr
		}
		return nil, nil
	}
	if fetchErr != nil {
		d.Note = strings.TrimSpace(d.Note + "; the database didn't answer: " + fetchErr.Error())
	}
	return d, nil
}

var unsafeFileChars = regexp.MustCompile(`[^\w.$#-]+`)

// definitionFile writes a definition to its file under the workspace's
// views directory; returns the path and the text.
func definitionFile(workspace, conn string, d *database.Definition) (string, string, error) {
	if conn == "" {
		conn = "db"
	}
	// keyed by the workspace: the CLI finds this server from the file's directory
	dir := filepath.Join(control.ViewsDir(workspace), unsafeFileChars.ReplaceAllString(conn, "_"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, unsafeFileChars.ReplaceAllString(qualified(d), "_")+".sql")
	text := definitionText(conn, d)
	_ = os.Remove(path) // read-only: replace it
	if err := os.WriteFile(path, []byte(text), 0o400); err != nil {
		return "", "", err
	}
	return path, text, nil
}

func qualified(d *database.Definition) string {
	if d.Schema != "" {
		return d.Schema + "." + d.Name
	}
	return d.Name
}

// definitionText is a definition with its header comment.
func definitionText(conn string, d *database.Definition) string {
	kind := string(d.Kind)
	if kind == "" {
		kind = string(database.RelTable)
	}
	if d.View != nil {
		if w := d.View.Writes.Summary(); w != "" {
			kind += ", " + w
		}
	}
	what := "a read-only copy of its definition"
	if d.Source != "" {
		what += " (" + d.Source + ")"
	}
	head := fmt.Sprintf("-- %s (%s) in %s: %s, rewritten on every go-to-definition\n", qualified(d), kind, conn, what)
	if d.Note != "" {
		head += "-- " + d.Note + "\n"
	}
	return head + d.DDL
}

// definitionLocation: where the file shows the column (or the name).
func definitionLocation(path, text string, d *database.Definition, column string) lsp.Location {
	var line, from, to int
	if d.View != nil {
		line, from, to = viewColumnPosition(text, column)
	} else {
		line, from, to = tableColumnPosition(text, column)
	}
	return lsp.Location{
		URI: "file://" + path,
		Range: lsp.Range{
			Start: lsp.Position{Line: line, Character: from},
			End:   lsp.Position{Line: line, Character: to},
		},
	}
}

var fromRe = regexp.MustCompile(`(?i)^from\b`)

var createNameRe = regexp.MustCompile(`(?is)^CREATE\b.*?\b(?:VIEW|TABLE)\s+(?:IF\s+NOT\s+EXISTS\s+)?(\S+?)(\s*\(|\s+AS\b|\s*$)`)

// createStart: the offset of the first CREATE statement in a definition file.
func createStart(text string) int {
	if strings.HasPrefix(strings.ToUpper(text), "CREATE ") {
		return 0
	}
	if i := strings.Index(strings.ToUpper(text), "\nCREATE "); i >= 0 {
		return i + 1
	}
	return 0
}

// namePosition: the object's name in the CREATE statement.
func namePosition(text string, start int) (int, int, int) {
	if m := createNameRe.FindStringSubmatchIndex(text[start:]); m != nil {
		return position(text, start+m[2], m[3]-m[2])
	}
	return position(text, start, 0)
}

// viewColumnPosition finds where a view's text defines column: in the first
// select list, "… AS column", an item ending with it (t.column), or any
// mention. Without a column (or not found): the CREATE statement's name.
func viewColumnPosition(text, column string) (line, from, to int) {
	start := createStart(text)
	if column != "" {
		if off, n, ok := findViewColumn(text[start:], column); ok {
			return position(text, start+off, n)
		}
	}
	return namePosition(text, start)
}

// tableColumnPosition finds a column's line in the CREATE TABLE (a line
// starting with its name, quoted or not); else the table's name.
func tableColumnPosition(text, column string) (line, from, to int) {
	start := createStart(text)
	if column != "" {
		re := regexp.MustCompile(`(?im)^[\s(]*("?)(` + regexp.QuoteMeta(column) + `)("?)\s`)
		if m := re.FindStringSubmatchIndex(text[start:]); m != nil {
			return position(text, start+m[4], m[5]-m[4])
		}
	}
	return namePosition(text, start)
}

func findViewColumn(stmt, column string) (int, int, bool) {
	sel := regexp.MustCompile(`(?i)\bSELECT\b`).FindStringIndex(stmt)
	if sel == nil {
		return 0, 0, false
	}
	// the select list: up to FROM outside parentheses and quotes
	list := stmt[sel[1]:]
	depth, quote := 0, byte(0)
	for i := 0; i < len(list); i++ {
		c := list[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && (i == 0 || !isWordChar(list[i-1])) && fromRe.MatchString(list[i:]):
			list = list[:i]
		}
	}
	col := `"?(` + regexp.QuoteMeta(column) + `)"?`
	for _, re := range []string{
		`(?i)\bAS\s+` + col + `(?:[\s,]|$)`,
		`(?i)(?:^|[\s,.(])` + col + `\s*(?:,|$)`,
		`(?i)\b` + col + `\b`,
	} {
		if m := regexp.MustCompile(re).FindStringSubmatchIndex(list); m != nil {
			return sel[1] + m[2], m[3] - m[2], true
		}
	}
	return 0, 0, false
}

func isWordChar(c byte) bool {
	return c == '_' || c == '$' || c == '#' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// position converts a byte offset and length to line and characters (UTF-16
// would differ only for non-ASCII text before it on the line).
func position(text string, off, n int) (int, int, int) {
	line := strings.Count(text[:off], "\n")
	col := off - (strings.LastIndex(text[:off], "\n") + 1)
	return line, col, col + n
}

// controlDDL: `sqlh ddl NAME` — the definition go to definition would show.
// Called without s.mu.
func (s *Server) controlDDL(ctx context.Context, name string) (*control.DDLResult, error) {
	s.mu.Lock()
	cache := s.worker.Cache()
	if s.dbConn == nil || cache == nil {
		s.mu.Unlock()
		return nil, ErrNoConnection
	}
	deps := defDeps{cache: cache, conn: connAlias(s.curDBCfg), driver: s.curDBCfg.Driver}
	schema, table := database.SplitQualified(name, deps.driver)
	t := newTarget(cache, schema, table, "")
	if t == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("schema %q is not one of the connection's", schema)
	}
	if schema == "" && t.view == nil {
		if _, ok := cache.CanonicalTableName(table); !ok && isSynonym(cache, table) {
			t.synonym = true
		}
	}
	if repo, err := s.newDBRepository(ctx); err == nil {
		deps.repo = repo
	}
	key := deps.conn + "\t"
	_, deps.skipMetadata = s.noMetadataDDL.Load(key)
	deps.failed = func() { s.noMetadataDDL.Store(key, true) }
	s.mu.Unlock()

	d, err := fetchDefinition(ctx, t, deps)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%s: %w", name, database.ErrNoObject)
	}
	return ddlResult(d), nil
}

// ddlResult converts a definition for the CLI.
func ddlResult(d *database.Definition) *control.DDLResult {
	return &control.DDLResult{Schema: d.Schema, Name: d.Name, Kind: string(d.Kind), Source: d.Source, Note: d.Note, DDL: d.DDL}
}
