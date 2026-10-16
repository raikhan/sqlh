package handler

import (
	"strings"

	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/ddl"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/token"
)

// statementContaining returns the statement whose text spans pos.
func statementContaining(text string, pos lsp.Position, d sqlsplit.Dialect) (sqlsplit.Statement, bool) {
	cur := sqlsplit.Position{Line: pos.Line, Char: pos.Character}
	for _, st := range sqlsplit.Split(text, d) {
		if !cur.Before(st.Start) && cur.Before(st.End) {
			return st, true
		}
	}
	return sqlsplit.Statement{}, false
}

// toStatementPos converts a document position to the token positions of a
// statement (line relative to its start; the first line's columns too).
func toStatementPos(st sqlsplit.Statement, pos lsp.Position) token.Pos {
	p := token.Pos{Line: pos.Line - st.Start.Line, Col: pos.Character}
	if p.Line == 0 {
		p.Col -= st.Start.Char
	}
	return p
}

func fromStatementPos(st sqlsplit.Statement, p token.Pos) lsp.Position {
	if p.Line == 0 {
		return lsp.Position{Line: st.Start.Line, Character: st.Start.Char + p.Col}
	}
	return lsp.Position{Line: st.Start.Line + p.Line, Character: p.Col}
}

func within(p, from, to token.Pos) bool {
	return token.ComparePos(p, from) >= 0 && token.ComparePos(p, to) < 0
}

// ddlHover: the object under the cursor in a DDL statement or DML tail.
func ddlHover(text string, pos lsp.Position, cache *database.DBCache, d sqlsplit.Dialect) *lsp.Hover {
	st, ok := statementContaining(text, pos, d)
	if !ok {
		return nil
	}
	a, _, ok := ddl.InStatement(st.SQL, d)
	if !ok || a.Kind == "" {
		return nil
	}
	p := toStatementPos(st, pos)
	for _, r := range a.Refs {
		if !within(p, r.Name.From, r.Name.To) {
			continue
		}
		content := refDoc(r, cache)
		if content == "" {
			return nil
		}
		return &lsp.Hover{
			Contents: lsp.MarkupContent{Kind: lsp.Markdown, Value: content},
			Range:    lsp.Range{Start: fromStatementPos(st, r.Name.From), End: fromStatementPos(st, r.Name.To)},
		}
	}
	return nil
}

func columnsOf(cache *database.DBCache, n ddl.Name) ([]*database.ColumnDesc, bool) {
	if n.Schema != "" {
		return cache.ColumnDatabase(n.Schema, n.Name)
	}
	return cache.ColumnDescs(n.Name)
}

var objectKinds = map[ddl.ObjKind]string{
	ddl.ObjIndex:      database.ObjectIndex,
	ddl.ObjSequence:   database.ObjectSequence,
	ddl.ObjTrigger:    database.ObjectTrigger,
	ddl.ObjType:       database.ObjectType,
	ddl.ObjConstraint: database.ObjectConstraint,
}

// refDoc documents an object a statement names ("" when unknown).
func refDoc(r ddl.Ref, cache *database.DBCache) string {
	switch {
	case r.Kind.IsRelation():
		if cols, ok := columnsOf(cache, r.Name); ok {
			return cache.TableDoc(r.Name.Name, cols)
		}
	case r.Kind == ddl.ObjColumn && r.Table != nil && r.Role != ddl.RoleDefined:
		cols, _ := columnsOf(cache, *r.Table)
		for _, c := range cols {
			if strings.EqualFold(c.Name, r.Name.Name) {
				return cache.ColumnDoc(r.Table.Name, c)
			}
		}
	default:
		kind, ok := objectKinds[r.Kind]
		if !ok {
			return ""
		}
		schema := r.Name.Schema
		if schema == "" && r.Table != nil {
			schema = r.Table.Schema
		}
		if schema == "" {
			schema = cache.DefaultSchema()
		}
		for _, x := range cache.Objects.Find(kind, schema, r.Name.Name) {
			if r.Table == nil || strings.EqualFold(x.Table, r.Table.Name) {
				return x.Doc()
			}
		}
	}
	return ""
}
