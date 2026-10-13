package catalog

import (
	"fmt"
	"strings"

	"github.com/raikhan/sqlh/internal/completer"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
)

// Grapher is implemented by sources that have the relationship graph.
type Grapher interface {
	Relations() (*database.RelGraph, error)
}

// Relations describes a table or view and its relationships. table may be
// "t", "s.t" or quoted forms; only the default schema's are known.
func Relations(src Source, table string, opt Options, maybe bool) (*control.RelationsResult, error) {
	parts := splitName(table)
	schema, name := src.DefaultSchema(), ""
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		schema, name = parts[0], parts[1]
	default:
		return nil, fmt.Errorf("invalid table name %q", table)
	}
	res := &control.RelationsResult{Table: name, Schema: schema, Insert: table, Kind: string(database.RelTable),
		Relations: []control.RelationItem{}}
	if _, ok := src.Columns(schema, name); !ok {
		return nil, fmt.Errorf("unknown table %q", table)
	}
	if !strings.EqualFold(schema, src.DefaultSchema()) {
		res.Note = "relationships are only known for the default schema (" + src.DefaultSchema() + ")"
		return res, nil
	}
	gr, ok := src.(Grapher)
	if !ok {
		res.Note = "relationships unavailable"
		return res, nil
	}
	g, err := gr.Relations()
	if err != nil {
		return nil, err
	}
	if g == nil {
		res.Note = "relationships not loaded yet"
		return res, nil
	}
	if c, ok := g.Canonical(name); ok {
		name = c
	}
	res.Table = name
	res.Insert = opt.ident(name)
	res.Kind = string(g.Kind(name))
	res.ViewsLoaded = g.ViewsLoaded
	if v := g.View(name); v != nil {
		res.Definition = strings.TrimSpace(v.Query)
		res.Writes = v.Writes.Summary()
	}
	for _, r := range g.Related(name, maybe) {
		item := control.RelationItem{
			Table:  r.Other,
			Insert: opt.ident(r.Other),
			Kind:   string(g.Kind(r.Other)),
			Edge:   string(r.Edge.Kind),
			Marker: r.Marker(),
			Out:    r.Out,
			Maybe:  r.Edge.Inferred,
			Note:   r.Note(),
		}
		if v := r.Edge.Via; v != "" {
			item.Note = strings.Replace(item.Note, "via "+v, "via "+opt.ident(v), 1)
		}
		cols := r.Cols
		if r.Edge.Kind == database.EdgeUses && !r.Out {
			// a view reading this table: joinable when it exposes the table's key
			// (from the view's side that edge is listed by itself)
			cols = g.JoinVia(name, r.Other)
		}
		var cond []string
		for _, c := range cols {
			a, b := opt.ident(c.From), opt.ident(c.To)
			item.Cols = append(item.Cols, [2]string{a, b})
			cond = append(cond, item.Insert+"."+b+" = "+a)
		}
		item.Condition = strings.Join(cond, " and ")
		res.Relations = append(res.Relations, item)
	}
	return res, nil
}

// Context describes the statement at a 1-based line and column of text.
func Context(text string, line, col int) control.ContextResult {
	sc := completer.CursorContext(text, line-1, max(col-1, 0))
	res := control.ContextResult{InJoin: sc.InJoin, NeedSpace: sc.NeedSpace, Indent: sc.Indent, Tables: []control.ScopeTable{}}
	for _, t := range sc.Tables {
		st := control.ScopeTable{Name: t.Name, Schema: t.DatabaseSchema, Alias: t.Alias, Ref: t.Alias}
		if st.Ref == "" {
			st.Ref = t.Name
			if t.DatabaseSchema != "" {
				st.Ref = t.DatabaseSchema + "." + t.Name
			}
		}
		res.Tables = append(res.Tables, st)
	}
	if sc.Focus != nil {
		res.Focus = sc.Focus.Name
	}
	return res
}

// aliasKeywords are short words an alias must not be.
var aliasKeywords = map[string]bool{
	"as": true, "at": true, "by": true, "do": true, "go": true, "if": true, "in": true, "is": true,
	"no": true, "of": true, "on": true, "or": true, "to": true, "all": true, "and": true, "any": true,
	"asc": true, "end": true, "for": true, "key": true, "not": true, "set": true, "use": true,
}

// Alias proposes a table alias: the initials of its words (order_line → ol,
// customer → c), numbered when taken. taken holds lower-case names in use.
func Alias(table string, taken map[string]bool) string {
	name := strings.ToLower(table)
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Trim(name, `"`)
	var b strings.Builder
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == ' ' || r == '$' || r == '#' }) {
		r := []rune(w)
		if len(r) > 0 && (r[0] >= 'a' && r[0] <= 'z') {
			b.WriteRune(r[0])
		}
	}
	base := b.String()
	if base == "" {
		base = "t"
	}
	alias := base
	for i := 2; taken[alias] || aliasKeywords[alias]; i++ {
		alias = fmt.Sprintf("%s%d", base, i)
	}
	return alias
}

// JoinStep is one table of a join chain: Table is joined to Anchor on Cols
// (Anchor's column, Table's column).
type JoinStep struct {
	Insert string      // the table, as typed
	Cols   [][2]string // empty: no condition known
	// AnchorRef: how to refer to the anchor; empty for the previous step's table.
	AnchorRef string
}

// ComposeJoin renders a join chain for insertion after a JOIN keyword:
//
//	customer c ON c.id = o.customer_id
//	JOIN region r ON r.id = c.region_id
//
// taken: lower-case aliases and names already used by the statement.
func ComposeJoin(steps []JoinStep, taken map[string]bool, lowerKw bool, indent string) string {
	kw := func(s string) string {
		if lowerKw {
			return strings.ToLower(s)
		}
		return s
	}
	used := map[string]bool{}
	for k := range taken {
		used[k] = true
	}
	var b strings.Builder
	prev := ""
	for i, st := range steps {
		alias := Alias(st.Insert, used)
		used[alias] = true
		if i > 0 {
			b.WriteString("\n" + indent + kw("JOIN") + " ")
		}
		b.WriteString(st.Insert + " " + alias)
		anchor := st.AnchorRef
		if anchor == "" {
			anchor = prev
		}
		if len(st.Cols) > 0 && anchor != "" {
			b.WriteString(" " + kw("ON") + " ")
			for j, c := range st.Cols {
				if j > 0 {
					b.WriteString(" " + kw("AND") + " ")
				}
				b.WriteString(alias + "." + c[1] + " = " + anchor + "." + c[0])
			}
		}
		prev = alias
	}
	return b.String()
}
