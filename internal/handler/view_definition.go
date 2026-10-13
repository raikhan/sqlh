package handler

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
)

// Go to definition of a view: its definition lives in the database, so it is
// written to a read-only file (rewritten on every request) that the editor
// opens: <runtime dir>/views/<workspace key>/<connection>/<schema>.<view>.sql.

// viewTarget finds the view at the position: its name (also as a qualifier or
// schema.view) or one of its columns (v.col, alias.col, or an unqualified
// column that only a view of the statement has).
func viewTarget(parsed ast.TokenList, pos token.Pos, nw *parseutil.NodeWalker, cache *database.DBCache) (*database.ViewDef, string) {
	g := cache.Relations
	if g == nil {
		return nil, ""
	}
	nodes := nw.CurNodeMatches(astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier, ast.TypeIdentifier}})
	ident, mem := findIdent(nodes)
	env, err := collectEnvironment(parsed, pos)
	if err != nil {
		return nil, ""
	}
	inScope := func(name string) *database.ViewDef { // a table name or alias of the statement
		if real, ok := env.getTableRealName(name); ok {
			name = real
		}
		return g.View(name)
	}
	switch {
	case mem != nil && (ident == nil || ident.NoQuoteString() == mem.ChildTok.NoQuoteString()):
		parent, child := mem.ParentTok.NoQuoteString(), mem.ChildTok.NoQuoteString()
		if v := inScope(parent); v != nil {
			return v, child // v.col
		}
		if strings.EqualFold(parent, cache.DefaultSchema()) {
			return g.View(child), "" // schema.view
		}
	case mem != nil:
		return inScope(mem.ParentTok.NoQuoteString()), "" // [v].col
	case ident != nil:
		name := ident.NoQuoteString()
		if v := g.View(name); v != nil {
			return v, ""
		}
		// a column of a view in the statement, unless another table has it too
		var hit *database.ViewDef
		for _, t := range env.tables {
			if t.DatabaseSchema != "" && !strings.EqualFold(t.DatabaseSchema, cache.DefaultSchema()) {
				continue
			}
			if _, ok := cache.Column(t.Name, name); !ok {
				continue
			}
			v := g.View(t.Name)
			if v == nil || (hit != nil && hit != v) {
				return nil, ""
			}
			hit = v
		}
		if hit != nil {
			return hit, name
		}
	}
	return nil, ""
}

var unsafeFileChars = regexp.MustCompile(`[^\w.$#-]+`)

// viewFile writes the view's definition to its file.
func (s *Server) viewFile(v *database.ViewDef) (string, string, error) {
	conn := "db"
	if s.curDBCfg != nil && s.curDBCfg.Alias != "" {
		conn = s.curDBCfg.Alias
	}
	// keyed by the workspace: the CLI finds this server from the file's directory
	dir := filepath.Join(control.ViewsDir(s.workspace), unsafeFileChars.ReplaceAllString(conn, "_"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	name := v.Name
	if v.Schema != "" {
		name = v.Schema + "." + name
	}
	path := filepath.Join(dir, unsafeFileChars.ReplaceAllString(name, "_")+".sql")
	kind := "view"
	if v.Materialized {
		kind = "materialized view"
	}
	if w := v.Writes.Summary(); w != "" {
		kind += ", " + w
	}
	text := fmt.Sprintf("-- %s (%s) in %s: a read-only copy of its definition, rewritten on every go-to-definition\n",
		name, kind, conn) + v.CreateStatement()
	_ = os.Remove(path) // read-only: replace it
	if err := os.WriteFile(path, []byte(text), 0o400); err != nil {
		return "", "", err
	}
	return path, text, nil
}

var fromRe = regexp.MustCompile(`(?i)^from\b`)

// viewColumnPosition finds where a view's text defines column: in the first
// select list, "… AS column", an item ending with it (t.column), or any
// mention. Without a column (or not found): the CREATE statement's name.
func viewColumnPosition(text, column string) (line, from, to int) {
	start := strings.Index(strings.ToUpper(text), "\nCREATE ")
	if start < 0 {
		start = 0
	} else {
		start++
	}
	if column != "" {
		if off, n, ok := findViewColumn(text[start:], column); ok {
			return position(text, start+off, n)
		}
	}
	// the view's name: the word before AS on the CREATE line
	head := text[start:]
	if m := regexp.MustCompile(`(?is)^CREATE\b.*?\bVIEW\s+(?:IF\s+NOT\s+EXISTS\s+)?(\S+?)(\s*\(|\s+AS\b)`).FindStringSubmatchIndex(head); m != nil {
		return position(text, start+m[2], m[3]-m[2])
	}
	return position(text, start, 0)
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
