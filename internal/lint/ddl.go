package lint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/raikhan/sqlh/internal/ddl"
	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/token"
)

// ddlState: what earlier statements of the document did, in order.
type ddlState struct {
	// tables created earlier (upper name); columns known when created with
	// an explicit column list
	tables  map[string]*docTable
	dropped map[string]bool
	// columns: TABLE → COLUMN → added (true) or dropped (false) earlier
	columns map[string]map[string]bool
	// objects: kind \t NAME created (true) or dropped (false) earlier
	objects map[string]bool
}

type docTable struct {
	cols  map[string]bool
	known bool
}

func newDDLState() *ddlState {
	return &ddlState{tables: map[string]*docTable{}, dropped: map[string]bool{}, columns: map[string]map[string]bool{}, objects: map[string]bool{}}
}

var objectKinds = map[ddl.ObjKind]string{
	ddl.ObjIndex:      "index",
	ddl.ObjSequence:   "sequence",
	ddl.ObjTrigger:    "trigger",
	ddl.ObjConstraint: "constraint",
}

func diag(st sqlsplit.Statement, from, to token.Pos, format string, args ...interface{}) Diagnostic {
	pos := func(p token.Pos) sqlsplit.Position {
		if p.Line == 0 {
			return sqlsplit.Position{Line: st.Start.Line, Char: st.Start.Char + p.Col}
		}
		return sqlsplit.Position{Line: st.Start.Line + p.Line, Char: p.Col}
	}
	return Diagnostic{Start: pos(from), End: pos(to), Message: fmt.Sprintf(format, args...)}
}

// kindWord names what a reference expects.
func kindWord(kinds []ddl.ObjKind) string {
	hasView := false
	for _, k := range kinds {
		if k == ddl.ObjView {
			hasView = true
		}
	}
	switch {
	case len(kinds) == 1:
		return kinds[0].String()
	case hasView && len(kinds) > 1:
		return "table or view"
	}
	return "table"
}

// implicitName: names available without being in the cache.
func (c *Catalog) implicitName(upper string) bool {
	switch c.Dialect {
	case sqlsplit.PostgreSQL:
		return strings.HasPrefix(upper, "PG_")
	case sqlsplit.SQLite:
		return strings.HasPrefix(upper, "SQLITE_")
	case sqlsplit.Oracle:
		return upper == "DUAL"
	}
	return false
}

// checkDDL checks the objects a DDL statement (or a DML tail) names. In a
// DML statement only the tails' columns are checked: the statement's tables
// are the query checks'.
func checkDDL(st sqlsplit.Statement, a *ddl.Analysis, cat *Catalog, state *ddlState) []Diagnostic {
	createdHere := map[string]bool{}
	for _, r := range a.Refs {
		if r.Role == ddl.RoleCreate && r.Kind.IsRelation() {
			createdHere[r.Name.Upper()] = true
		}
	}
	var out []Diagnostic
	for _, r := range a.Refs {
		switch {
		case r.Kind.IsRelation():
			if !a.DML && (r.Role == ddl.RoleUse || r.Role == ddl.RoleDrop) {
				out = append(out, cat.checkRelation(st, r, state, createdHere)...)
			}
		case r.Kind == ddl.ObjColumn:
			out = append(out, cat.checkColumn(st, a, r, state, createdHere)...)
		default:
			if _, ok := objectKinds[r.Kind]; ok && (r.Role == ddl.RoleUse || r.Role == ddl.RoleDrop) {
				out = append(out, cat.checkObject(st, r, state)...)
			}
		}
	}
	return out
}

func (c *Catalog) checkRelation(st sqlsplit.Statement, r ddl.Ref, state *ddlState, createdHere map[string]bool) []Diagnostic {
	name := r.Name.Upper()
	if r.Soft || state.tables[name] != nil || createdHere[name] || c.implicitName(name) || strings.Contains(name, "@") {
		return nil
	}
	if state.dropped[name] {
		if r.Role == ddl.RoleUse && !r.IfExists {
			return []Diagnostic{diag(st, r.Name.From, r.Name.To, "%s %s is dropped by an earlier statement", kindWord(r.Kinds), r.Name.String())}
		}
		return nil
	}
	res, found, sure := c.resolve(r.Name.Schema, r.Name.Name)
	if !found && sure {
		if r.IfExists {
			return nil
		}
		msg := fmt.Sprintf("unknown %s %s", kindWord(r.Kinds), r.Name.String())
		if s := suggest(name, c.tableNames(r.Name.Schema)); s != "" {
			msg += fmt.Sprintf(" (did you mean %s?)", strings.ToLower(s))
		}
		return []Diagnostic{diag(st, r.Name.From, r.Name.To, "%s", msg)}
	}
	if found && sure && r.StrictKind && res.schema != "" {
		if k, ok := c.kindOf(res); ok && k != r.Kind.String() {
			return []Diagnostic{diag(st, r.Name.From, r.Name.To, "%s is a %s, not a %s: use DROP %s",
				strings.ToLower(r.Name.Name), k, r.Kind.String(), strings.ToUpper(k))}
		}
	}
	return nil
}

// tableColumns: the columns of a referenced table as the document has them
// so far (ok false: not known).
func (c *Catalog) tableColumns(t ddl.Name, state *ddlState, createdHere map[string]bool) (cols map[string]bool, display string, ok bool) {
	name := t.Upper()
	if dt := state.tables[name]; dt != nil {
		return dt.cols, strings.ToLower(t.Name), dt.known
	}
	if createdHere[name] {
		return nil, "", false
	}
	res, found, sure := c.resolve(t.Schema, t.Name)
	if !found || !sure || res.schema == "" && res.table == "" {
		return nil, "", false
	}
	cols, ok = c.columnsOf(res)
	return cols, strings.ToLower(res.table), ok
}

func (c *Catalog) checkColumn(st sqlsplit.Statement, a *ddl.Analysis, r ddl.Ref, state *ddlState, createdHere map[string]bool) []Diagnostic {
	if r.Table == nil {
		return nil
	}
	col := r.Name.Upper()
	if r.Role == ddl.RoleDefined {
		if a.HasLike {
			return nil
		}
		for _, d := range a.Defined {
			if d.Upper() == col {
				return nil
			}
		}
		return []Diagnostic{diag(st, r.Name.From, r.Name.To, "column %s is not defined in this table", r.Name.Name)}
	}
	cols, table, ok := c.tableColumns(*r.Table, state, createdHere)
	if !ok {
		return nil
	}
	has := cols[col] || contains(pseudoColumns[c.Dialect], col)
	delta, changed := state.columns[r.Table.Upper()][col]
	if changed {
		has = delta
	}
	switch r.Role {
	case ddl.RoleUse, ddl.RoleDrop:
		if has || r.IfExists || (changed && r.Role == ddl.RoleDrop) {
			return nil
		}
		names := make([]string, 0, len(cols))
		for n := range cols {
			if v, ok := state.columns[r.Table.Upper()][n]; n != col && (!ok || v) {
				names = append(names, n)
			}
		}
		for n, v := range state.columns[r.Table.Upper()] {
			if v && n != col {
				names = append(names, n)
			}
		}
		msg := fmt.Sprintf("%s has no column %s", table, r.Name.Name)
		if s := suggest(col, names); s != "" {
			msg += fmt.Sprintf(" (did you mean %s?)", strings.ToLower(s))
		}
		return []Diagnostic{diag(st, r.Name.From, r.Name.To, "%s", msg)}
	case ddl.RoleCreate:
		if has && !r.IfExists {
			return []Diagnostic{diag(st, r.Name.From, r.Name.To, "%s already has a column %s", table, r.Name.Name)}
		}
	}
	return nil
}

func (c *Catalog) checkObject(st sqlsplit.Statement, r ddl.Ref, state *ddlState) []Diagnostic {
	kind := objectKinds[r.Kind]
	name := r.Name.Upper()
	if r.IfExists || !c.objectsKnown {
		return nil
	}
	if _, seen := state.objects[kind+"\t"+name]; seen {
		return nil // created or dropped earlier in the document
	}
	if c.Dialect == sqlsplit.Oracle && strings.HasPrefix(name, "SYS_") {
		return nil // system-named (NOT NULL checks are not loaded)
	}
	schema, table := r.Name.Schema, ""
	if r.Table != nil {
		if state.tables[r.Table.Upper()] != nil {
			return nil // a table of this document: its objects are unknown
		}
		res, found, sure := c.resolve(r.Table.Schema, r.Table.Name)
		if !found || !sure {
			return nil
		}
		table = r.Table.Name
		if res.schema != "" {
			schema = res.schema
		}
	}
	found, sure := c.findObject(kind, schema, name, table)
	if found || !sure {
		return nil
	}
	msg := fmt.Sprintf("unknown %s %s", kind, r.Name.String())
	if table != "" {
		msg += " on " + strings.ToLower(table)
	}
	if s := suggest(name, c.objectNames(kind, schema)); s != "" && s != name {
		msg += fmt.Sprintf(" (did you mean %s?)", strings.ToLower(s))
	}
	return []Diagnostic{diag(st, r.Name.From, r.Name.To, "%s", msg)}
}

// apply records what a statement did, for the statements after it.
func (s *ddlState) apply(a *ddl.Analysis) {
	for _, r := range a.Refs {
		name := r.Name.Upper()
		switch {
		case r.Kind.IsRelation() && r.Role == ddl.RoleCreate:
			dt := &docTable{cols: map[string]bool{}}
			if r.Renamed != nil {
				old := r.Renamed.Upper()
				if prev := s.tables[old]; prev != nil {
					dt = prev
				}
				delete(s.tables, old)
				s.dropped[old] = true
				s.columns[name] = s.columns[old]
			} else if a.Kind == "CREATE TABLE" && !a.HasLike && a.Body < 0 {
				dt.known = true
				for _, d := range a.Defined {
					dt.cols[d.Upper()] = true
				}
				delete(s.columns, name)
			}
			s.tables[name] = dt
			delete(s.dropped, name)
		case r.Kind.IsRelation() && r.Role == ddl.RoleDrop:
			delete(s.tables, name)
			s.dropped[name] = true
		case r.Kind == ddl.ObjColumn && r.Table != nil && (r.Role == ddl.RoleCreate || r.Role == ddl.RoleDrop):
			t := r.Table.Upper()
			if s.columns[t] == nil {
				s.columns[t] = map[string]bool{}
			}
			s.columns[t][name] = r.Role == ddl.RoleCreate
			if r.Renamed != nil {
				s.columns[t][r.Renamed.Upper()] = false
			}
		default:
			kind, ok := objectKinds[r.Kind]
			if !ok {
				continue
			}
			switch r.Role {
			case ddl.RoleCreate:
				s.objects[kind+"\t"+name] = true
				if r.Renamed != nil {
					s.objects[kind+"\t"+r.Renamed.Upper()] = false
				}
			case ddl.RoleDrop:
				s.objects[kind+"\t"+name] = false
			}
		}
	}
}

// bodyStatement: the query of CREATE VIEW/TABLE … AS, as a statement of its
// own (from the token at the query's start).
func bodyStatement(st sqlsplit.Statement, from token.Pos) sqlsplit.Statement {
	off, line, col := 0, 0, 0
	for i, r := range st.SQL {
		if line == from.Line && col == from.Col {
			off = i
			break
		}
		if r == '\n' {
			line++
			col = 0
		} else {
			col++
		}
	}
	start := sqlsplit.Position{Line: st.Start.Line + from.Line, Char: from.Col}
	if from.Line == 0 {
		start.Char = st.Start.Char + from.Col
	}
	return sqlsplit.Statement{SQL: st.SQL[off:], Start: start, End: st.End}
}

var dynamicRe = regexp.MustCompile(`(?i)\bexecute\s+(immediate\s+)?`)

// dynamicSQL extracts the string literals run by EXECUTE [IMMEDIATE] in a
// PL/SQL or PL/pgSQL block ('…' and Oracle q'[…]').
func dynamicSQL(sql string) []string {
	var out []string
	for _, m := range dynamicRe.FindAllStringIndex(sql, -1) {
		if s, ok := stringLiteral(sql[m[1]:]); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringLiteral(s string) (string, bool) {
	switch {
	case len(s) >= 3 && (s[0] == 'q' || s[0] == 'Q') && s[1] == '\'':
		closing := map[byte]byte{'[': ']', '{': '}', '(': ')', '<': '>'}[s[2]]
		if closing == 0 {
			closing = s[2]
		}
		end := strings.Index(s[3:], string(closing)+"'")
		if end < 0 {
			return "", false
		}
		return s[3 : 3+end], true
	case len(s) >= 1 && s[0] == '\'':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				return b.String(), true
			}
			b.WriteByte(s[i])
		}
	}
	return "", false
}

// applyDynamic records the DDL run as dynamic SQL in a block the checks skip.
func (s *ddlState) applyDynamic(sql string, d sqlsplit.Dialect) {
	for _, dyn := range dynamicSQL(sql) {
		if a, _, ok := ddl.InStatement(dyn, d); ok && a.Kind != "" && !a.DML {
			s.apply(a)
		}
	}
}
