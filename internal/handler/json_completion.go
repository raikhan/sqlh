package handler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/jsoncomplete"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
)

// jsonKeyWait is how long completion waits for a key sample before
// answering "incomplete" (the fetch continues; the next keystroke gets it).
const jsonKeyWait = 1500 * time.Millisecond

// jsonCompletion completes keys of a JSON column (see internal/jsoncomplete).
func (s *Server) jsonCompletion(text string, params lsp.CompletionParams, jc *jsoncomplete.Context, d sqlsplit.Dialect) (interface{}, error) {
	empty := lsp.CompletionList{Items: []lsp.CompletionItem{}}
	if s.dbConn == nil || s.curDBCfg == nil {
		return empty, nil
	}
	target, colType, ok := s.jsonTarget(text, params.Position, jc)
	if !ok {
		return empty, nil
	}
	// PostgreSQL: only json/jsonb columns have these operators. Oracle has
	// no IS JSON constraints here: using the column in json_value() is the hint.
	if d == sqlsplit.PostgreSQL && !strings.Contains(strings.ToLower(colType), "json") {
		return empty, nil
	}

	db := s.dbConn.Conn // the pool, not the job session
	alias := connAlias(s.curDBCfg)
	var (
		keys  []jsoncomplete.Key
		ready bool
		err   error
	)
	switch d {
	case sqlsplit.Oracle:
		id := fmt.Sprintf("%s|%s.%s.%s", alias, target.Schema, target.Table, target.Column)
		var all []jsoncomplete.Key
		all, ready, err = s.jsonKeys.Get(id, jsonKeyWait, func(ctx context.Context) ([]jsoncomplete.Key, error) {
			return jsoncomplete.OracleGuide(ctx, db, target)
		})
		keys = jsoncomplete.Children(all, jc.Path)
	default:
		id := fmt.Sprintf("%s|%s.%s.%s|%s", alias, target.Schema, target.Table, target.Column, strings.Join(jc.Path, "\x00"))
		keys, ready, err = s.jsonKeys.Get(id, jsonKeyWait, func(ctx context.Context) ([]jsoncomplete.Key, error) {
			return jsoncomplete.PostgresKeys(ctx, db, target, jc.Path)
		})
	}
	if err != nil {
		log.Println("json keys:", err)
		return empty, nil
	}
	if !ready {
		return lsp.CompletionList{IsIncomplete: true, Items: []lsp.CompletionItem{}}, nil
	}

	cursor := lsp.Position{Line: params.Position.Line, Character: params.Position.Character}
	start := lsp.Position{Line: jc.Replace.Line, Character: jc.Replace.Char}
	items := make([]lsp.CompletionItem, 0, len(keys))
	for i, k := range keys {
		item := lsp.CompletionItem{
			Label:      k.Name,
			Kind:       lsp.PropertyCompletion,
			Detail:     "json " + jsoncomplete.Detail(k),
			FilterText: k.Name,
			SortText:   fmt.Sprintf("%05d", i),
			TextEdit: &lsp.TextEdit{
				Range:   lsp.Range{Start: start, End: cursor},
				NewText: jsoncomplete.SQLText(jsoncomplete.InsertText(jc.Style, k.Name)),
			},
		}
		if doc, ok := jsoncomplete.ArrayHint(jc, k); ok {
			// selectable (the user may be building the expression), but flagged
			item.Detail = "⚠ array element key · " + jsoncomplete.Detail(k)
			item.Documentation = &lsp.MarkupContent{Kind: lsp.Markdown, Value: doc}
		}
		items = append(items, item)
	}
	return lsp.CompletionList{Items: items}, nil
}

// jsonTarget resolves the column reference of a JSON context to a column
// (exact names and type) using the statement's FROM clause and the schema
// cache.
func (s *Server) jsonTarget(text string, pos lsp.Position, jc *jsoncomplete.Context) (jsoncomplete.Target, string, bool) {
	cache := s.worker.Cache()
	if cache == nil {
		return jsoncomplete.Target{}, "", false
	}
	var tables []*parseutil.TableInfo
	var rels []*parseutil.SubQueryInfo
	// The key's string is still open at the cursor (unless the editor
	// auto-paired the quote) and would swallow the FROM clause: parse a copy
	// with the string closed.
	if parsed, err := parser.Parse(closeStringAt(text, pos)); err == nil {
		at := token.Pos{Line: pos.Line, Col: pos.Character}
		tables, _ = parseutil.ExtractTable(parsed, at)
		rels, _ = parseutil.ExtractSubQueryViews(parsed, at)
	}
	// a column of a derived table or CTE: follow it to its table column
	if t, typ, ok := relationColumn(cache, rels, jc.Qualifier, jc.Column); ok {
		return t, typ, true
	}

	var cands []*parseutil.TableInfo
	switch q := jc.Qualifier; {
	case strings.Contains(q, "."): // schema.table
		i := strings.LastIndex(q, ".")
		cands = []*parseutil.TableInfo{{DatabaseSchema: q[:i], Name: q[i+1:]}}
	case q != "":
		for _, t := range tables {
			if strings.EqualFold(t.Alias, q) || strings.EqualFold(t.Name, q) {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			cands = []*parseutil.TableInfo{{Name: q}}
		}
	default:
		cands = tables
	}
	if t, typ, ok := findColumn(cache, cands, jc.Column); ok {
		return t, typ, true
	}
	// Fallback (unqualified, FROM not parseable yet): the column name is
	// unique among JSON-typed columns of the default schema.
	if jc.Qualifier == "" {
		var found []*database.ColumnDesc
		for _, tbl := range cache.SortedTables() {
			cols, _ := cache.ColumnDescs(tbl)
			for _, c := range cols {
				if strings.EqualFold(c.Name, jc.Column) && strings.Contains(strings.ToLower(c.Type), "json") {
					found = append(found, c)
				}
			}
		}
		if len(found) == 1 {
			c := found[0]
			return jsoncomplete.Target{Schema: c.Schema, Table: c.Table, Column: c.Name}, c.Type, true
		}
	}
	return jsoncomplete.Target{}, "", false
}

// relationColumn resolves qualifier.column (or an unqualified column) of a
// derived table/CTE in scope to the database column it is taken from.
func relationColumn(cache *database.DBCache, rels []*parseutil.SubQueryInfo, qualifier, column string) (jsoncomplete.Target, string, bool) {
	if strings.Contains(qualifier, ".") {
		return jsoncomplete.Target{}, "", false
	}
	for _, r := range rels {
		if qualifier == "" && r.Outer {
			continue
		}
		if qualifier != "" && !strings.EqualFold(r.Name, qualifier) {
			continue
		}
		col := database.FindRelationColumn(r, cache, column)
		if col == nil {
			continue
		}
		if c := col.Origin; c != nil {
			return jsoncomplete.Target{Schema: c.Schema, Table: c.Table, Column: c.Name}, c.Type, true
		}
		return jsoncomplete.Target{}, "", false // an expression: type unknown
	}
	return jsoncomplete.Target{}, "", false
}

func findColumn(cache *database.DBCache, cands []*parseutil.TableInfo, column string) (jsoncomplete.Target, string, bool) {
	for _, t := range cands {
		var cols []*database.ColumnDesc
		if t.DatabaseSchema != "" {
			cols, _ = cache.ColumnDatabase(t.DatabaseSchema, t.Name)
		} else {
			cols, _ = cache.ColumnDescs(t.Name)
		}
		for _, c := range cols {
			if strings.EqualFold(c.Name, column) {
				return jsoncomplete.Target{Schema: c.Schema, Table: c.Table, Column: c.Name}, c.Type, true
			}
		}
	}
	return jsoncomplete.Target{}, "", false
}

// closeStringAt inserts a closing quote at pos unless one follows already.
func closeStringAt(text string, pos lsp.Position) string {
	lines := strings.SplitAfter(text, "\n")
	if pos.Line >= len(lines) {
		return text
	}
	line := []rune(lines[pos.Line])
	ch := min(pos.Character, len(line))
	if ch < len(line) && line[ch] == '\'' {
		return text
	}
	lines[pos.Line] = string(line[:ch]) + "'" + string(line[ch:])
	return strings.Join(lines, "")
}
