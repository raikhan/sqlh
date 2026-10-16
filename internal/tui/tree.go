package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

type nodeKind int

const (
	nInfo      nodeKind = iota // a message line (loading…, nothing here)
	nStmtTable                 // a table of the statement at the cursor
	nConnGroup                 // a group of connections
	nConnItem                  // a configured connection
	nSchema                    // another schema of the connection
	nGroup                     // a group of objects (Tables, Views, Relations, Indexes, …)
	nTable                     // a table, view or materialized view
	nRelation                  // a table reached through a relationship
	nColumn                    // a column
	nObject                    // a sequence, type, index, constraint or trigger
	nHistory                   // a run: a query (its latest run, expands to its versions) or a version
	nQueryFile                 // a query file
)

// node is a row of a tab's tree. Children of tables are built lazily (they
// need the server's columns and relationships); so are a query's versions.
type node struct {
	kind     nodeKind
	id       string // stable across reloads: the path of labels
	label    string
	detail   string // dimmed after the label
	icon     string
	depth    int
	parent   *node
	children []*node
	expanded bool
	// lazy: children come from the server (built on expand); loaded once done
	lazy, loaded, loading bool

	// payload
	table     string // tables, relations, columns: the table as typed
	tableKind string // table | view | materialized view | ""
	schema    string
	col       control.ColumnItem
	rel       *control.RelationItem
	anchor    string            // relations: the table the path starts at
	path      []control.JoinHop // relations: the hops from anchor, the last one is this table
	ref       string            // how the table is referred to (statement: its alias)
	obj       control.CatalogObject
	conn      control.Connection
	groupKind string // groups: what they hold (relations, columns, …)
	run       *history.Entry
	head      bool   // history: a query (expands to its versions)
	version   bool   // history: a version of the statement at the cursor
	file      string // query files
}

func (n *node) add(c *node) *node {
	c.parent = n
	c.depth = n.depth + 1
	c.id = n.id + "/" + c.label
	if c.kind == nRelation || c.kind == nColumn || c.kind == nHistory {
		c.id += "\x00" + c.detail // the same table may be related twice
	}
	if c.kind == nHistory && c.run != nil {
		c.id = n.id + "/" + c.run.ID
	}
	n.children = append(n.children, c)
	return c
}

// top makes a top-level node of a tab.
func top(tab string, n *node) *node {
	n.id = tab + "/" + n.label
	if n.kind == nHistory && n.run != nil {
		n.id = tab + "/" + n.run.ID
	}
	return n
}

// expandable reports whether the node can have children.
func (n *node) expandable() bool { return n.lazy || len(n.children) > 0 }

// visible flattens the expanded tree.
func visible(roots []*node) []*node {
	var out []*node
	var walk func(ns []*node)
	walk = func(ns []*node) {
		for _, n := range ns {
			out = append(out, n)
			if n.expanded {
				walk(n.children)
			}
		}
	}
	walk(roots)
	return out
}

// walkAll visits every node built so far.
func walkAll(roots []*node, fn func(*node)) {
	for _, n := range roots {
		fn(n)
		walkAll(n.children, fn)
	}
}

// kindGroups are the groups of a schema, in order.
var kindGroups = []struct{ kind, label string }{
	{"", "Tables & views"},
	{"table", "Tables"},
	{"view", "Views"},
	{"materialized view", "Materialized views"},
	{"sequence", "Sequences"},
	{"type", "Types"},
}

// isTableKind: the kinds listed as tables (expandable).
func isTableKind(k string) bool {
	return k == "" || k == "table" || k == "view" || k == "materialized view"
}

// buildObjects makes the Objects tab: the connection's own schema's groups,
// then its other schemas as folders.
func buildObjects(cat *control.CatalogResult, ic icons) []*node {
	var roots, others []*node
	for _, s := range cat.Schemas {
		if s.Default {
			holder := &node{id: "obj"}
			addSchemaGroups(holder, s, true, ic)
			for _, g := range holder.children {
				g.parent, g.depth = nil, 0
				fixDepth(g)
				roots = append(roots, g)
			}
			continue
		}
		label := s.Name
		if label == "" {
			label = "(default)"
		}
		sn := top("obj", &node{kind: nSchema, label: label, icon: ic.schema, schema: s.Name, detail: "schema"})
		addSchemaGroups(sn, s, false, ic)
		others = append(others, sn)
	}
	if len(roots) == 0 && len(others) == 0 {
		roots = append(roots, top("obj", &node{kind: nInfo, label: "no objects"}))
	}
	return append(roots, others...)
}

// fixDepth recomputes depths below a node that moved.
func fixDepth(n *node) {
	for _, c := range n.children {
		c.depth = n.depth + 1
		fixDepth(c)
	}
}

func addSchemaGroups(sn *node, s control.CatalogSchema, own bool, ic icons) {
	byKind := map[string][]control.CatalogObject{}
	for _, o := range s.Objects {
		if o.Table != "" { // indexes, constraints, triggers: under their table
			continue
		}
		byKind[o.Kind] = append(byKind[o.Kind], o)
	}
	for _, g := range kindGroups {
		objs := byKind[g.kind]
		if len(objs) == 0 {
			continue
		}
		gn := sn.add(&node{kind: nGroup, label: g.label, detail: fmt.Sprint(len(objs)), groupKind: g.kind,
			expanded: own && (g.kind == "table" || g.kind == "")})
		for _, o := range objs {
			if isTableKind(o.Kind) {
				t := gn.add(&node{kind: nTable, label: o.Name, table: o.Insert, tableKind: o.Kind, ref: o.Name,
					schema: s.Name, icon: ic.kind(o.Kind), lazy: true, obj: o})
				if o.Columns > 0 {
					t.detail = fmt.Sprintf("%d cols", o.Columns)
				}
			} else {
				gn.add(&node{kind: nObject, label: o.Name, detail: o.Detail, icon: ic.kind(o.Kind), obj: o, schema: s.Name})
			}
		}
	}
	if len(sn.children) == 0 {
		sn.add(&node{kind: nInfo, label: "no objects"})
	}
}

// tableObjects indexes the indexes, constraints and triggers of tables:
// UPPER schema \t UPPER table → objects.
func tableObjects(cat *control.CatalogResult) map[string][]control.CatalogObject {
	out := map[string][]control.CatalogObject{}
	if cat == nil {
		return out
	}
	for _, s := range cat.Schemas {
		for _, o := range s.Objects {
			if o.Table != "" {
				k := strings.ToUpper(s.Name) + "\t" + strings.ToUpper(o.Table)
				out[k] = append(out[k], o)
			}
		}
	}
	return out
}

// connTarget: "database › schema" of a connection.
func connTarget(c control.Connection) string {
	var parts []string
	for _, p := range []string{c.Database, c.Schema} {
		if p != "" && (len(parts) == 0 || !strings.EqualFold(parts[len(parts)-1], p)) {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " › ")
}

// buildConnections makes the Connections tab: ungrouped connections, then
// the groups (the current connection's open).
func buildConnections(conns []control.Connection, ic icons) []*node {
	var roots []*node
	groups := map[string]*node{}
	var order []string
	item := func(c control.Connection) *node {
		return &node{kind: nConnItem, label: c.Alias, detail: connTarget(c), conn: c, icon: ic.connection}
	}
	for _, c := range conns {
		if c.Group == "" {
			roots = append(roots, top("conns", item(c)))
			continue
		}
		g, ok := groups[c.Group]
		if !ok {
			g = top("conns", &node{kind: nConnGroup, label: c.Group, icon: ic.group})
			groups[c.Group] = g
			order = append(order, c.Group)
		}
		g.add(item(c))
		if c.Current {
			g.expanded = true
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return strings.ToLower(order[i]) < strings.ToLower(order[j]) })
	for _, name := range order {
		g := groups[name]
		g.detail = fmt.Sprint(len(g.children))
		roots = append(roots, g)
	}
	if len(conns) == 0 {
		roots = append(roots, top("conns", &node{kind: nInfo, label: "no connections: C adds one"}))
	}
	return roots
}

// fillTable builds a table's children: Relations, Indexes, Constraints,
// Triggers groups, then the columns.
func fillTable(n *node, cols []control.ColumnItem, rel *control.RelationsResult, objs []control.CatalogObject, ic icons) {
	n.children = nil
	if rel != nil {
		var joins, uses []control.RelationItem
		for _, r := range rel.Relations {
			if onPath(n, r.Insert) {
				continue // a chain of joins never returns to its tables
			}
			if r.Edge == "uses" && len(r.Cols) == 0 {
				uses = append(uses, r)
			} else {
				joins = append(joins, r)
			}
		}
		if len(joins) > 0 {
			g := n.add(&node{kind: nGroup, label: "Relations", detail: fmt.Sprint(len(joins)), groupKind: "relations", table: n.table,
				expanded: n.kind == nStmtTable})
			for i := range joins {
				addRelation(g, n, joins[i])
			}
		}
		if len(uses) > 0 {
			label := "Used by views"
			if n.tableKind == "view" || n.tableKind == "materialized view" {
				label = "Views"
			}
			g := n.add(&node{kind: nGroup, label: label, detail: fmt.Sprint(len(uses)), groupKind: "uses"})
			for i := range uses {
				r := uses[i]
				if r.Out && label == "Used by views" {
					g.label = "Reads / used by"
				}
				addRelation(g, n, r)
			}
		}
	}
	byKind := map[string][]control.CatalogObject{}
	for _, o := range objs {
		byKind[o.Kind] = append(byKind[o.Kind], o)
	}
	for _, k := range []struct{ kind, label string }{{"index", "Indexes"}, {"constraint", "Constraints"}, {"trigger", "Triggers"}} {
		if len(byKind[k.kind]) == 0 {
			continue
		}
		g := n.add(&node{kind: nGroup, label: k.label, detail: fmt.Sprint(len(byKind[k.kind])), groupKind: k.kind})
		for _, o := range byKind[k.kind] {
			g.add(&node{kind: nObject, label: o.Name, detail: o.Detail, icon: ic.kind(o.Kind), obj: o, schema: n.schema})
		}
	}
	for _, c := range cols {
		detail := c.Type
		if isPK(c.Key) {
			detail += "  PK"
		}
		n.add(&node{kind: nColumn, label: c.Insert, detail: detail, col: c, table: n.table, icon: ic.column})
	}
	if len(n.children) == 0 {
		n.add(&node{kind: nInfo, label: "no columns"})
	}
	n.loaded, n.loading = true, false
}

// onPath: table is the anchor of n's chain or one of its hops (n itself
// included).
func onPath(n *node, table string) bool {
	if n.kind != nRelation {
		return false
	}
	same := func(x string) bool { return strings.EqualFold(unquote(lastPart(x)), unquote(lastPart(table))) }
	if same(n.anchor) {
		return true
	}
	for _, h := range n.path {
		if same(h.Table) {
			return true
		}
	}
	return false
}

// refOf: how a table node is referred to in a join condition: the
// statement's alias, else its name.
func refOf(t *node) string {
	if t.ref != "" {
		return t.ref
	}
	return unquote(lastPart(t.table))
}

// addRelation adds a related table under a group of table t. The detail is
// the join column on t's side, as the statement refers to t (sds.input_id).
func addRelation(g, t *node, r control.RelationItem) {
	anchor, path := t.table, []control.JoinHop(nil)
	if t.kind == nRelation {
		anchor, path = t.anchor, append([]control.JoinHop(nil), t.path...)
	}
	path = append(path, control.JoinHop{Table: r.Insert, Cols: r.Cols})
	var cond []string
	for _, c := range r.Cols {
		cond = append(cond, refOf(t)+"."+c[0])
	}
	detail := strings.Join(cond, ", ")
	if r.Note != "" {
		detail = strings.TrimSpace(detail + "  " + r.Note)
	}
	rr := r
	g.add(&node{kind: nRelation, label: r.Marker + " " + r.Insert, detail: detail, table: r.Insert, tableKind: r.Kind,
		rel: &rr, anchor: anchor, path: path, lazy: true})
}

// buildStatement makes the Statement tab: the statement's tables, open,
// their relationships first.
func buildStatement(ctx *control.ContextResult, rels map[string]*control.RelationsResult, cur *control.Cursor, editor, open bool, ic icons) []*node {
	if cur == nil {
		label := "no statement: the cursor of the editor shows here"
		if !editor {
			label = "no editor: open a query (4) or create one (n)"
		}
		return []*node{top("stmt", &node{kind: nInfo, label: label})}
	}
	if ctx == nil || len(ctx.Tables) == 0 {
		return []*node{top("stmt", &node{kind: nInfo, label: "no tables at the cursor"})}
	}
	tables := append([]control.ScopeTable(nil), ctx.Tables...)
	// the focused table first
	for i, t := range tables {
		if ctx.Focus != "" && strings.EqualFold(t.Name, ctx.Focus) && i > 0 {
			tables = append([]control.ScopeTable{t}, append(tables[:i:i], tables[i+1:]...)...)
			break
		}
	}
	var roots []*node
	for _, t := range tables {
		name := t.Name
		if t.Schema != "" {
			name = t.Schema + "." + t.Name
		}
		label := name
		if t.Alias != "" {
			label += " " + t.Alias
		}
		insert := name
		if r := rels[strings.ToUpper(name)]; r != nil {
			insert = r.Insert
		}
		roots = append(roots, top("stmt", &node{kind: nStmtTable, label: label, table: insert, ref: t.Ref, lazy: true,
			icon: ic.kind("table"), expanded: open}))
	}
	return roots
}

// statementTitle: where the statement is.
func statementTitle(ctx *control.ContextResult, cur *control.Cursor) string {
	if cur == nil {
		return ""
	}
	s := shortPath(cur.File) + fmt.Sprintf(":%d", cur.Line)
	if ctx != nil && ctx.InJoin {
		s += " · after JOIN"
	}
	return s
}

func shortPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func lastPart(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func unquote(s string) string { return strings.ReplaceAll(strings.Trim(s, `"`), `""`, `"`) }

func isPK(key string) bool {
	k := strings.ToUpper(key)
	return k == "YES" || k == "PRI" || k == "P"
}

// buildHistory makes the History tab: the versions of the statement at the
// cursor, then the workspace's recent queries (each expands to its versions).
// conn: only the recent runs on this connection ("" = all).
func buildHistory(h *control.HistoryResult, conn string, ic icons) []*node {
	if h == nil {
		return []*node{top("hist", &node{kind: nInfo, label: "loading…"})}
	}
	var roots []*node
	if h.Statement != "" {
		g := top("hist", &node{kind: nGroup, label: "This statement", detail: fmt.Sprint(len(h.Versions)), expanded: true, groupKind: "versions"})
		if len(h.Versions) == 0 {
			g.add(&node{kind: nInfo, label: "not run yet"})
		}
		for i := range h.Versions {
			e := h.Versions[i]
			n := g.add(&node{kind: nHistory, label: runLabel(e), detail: firstLine(e.SQL), run: &e, version: true, icon: ic.history})
			if e.SQL == h.Statement {
				n.detail = "= current · " + n.detail
			}
		}
		roots = append(roots, g)
	}
	seen := map[string]bool{}
	shown := 0
	for i := range h.Recent {
		e := h.Recent[i]
		if conn != "" && e.Conn != conn {
			continue
		}
		shown++
		key := e.Lineage
		if key == "" {
			key = e.ID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		roots = append(roots, top("hist", &node{kind: nHistory, label: runLabel(e), detail: firstLine(e.SQL), run: &e,
			head: e.Lineage != "", lazy: e.Lineage != "", icon: ic.history}))
	}
	switch {
	case len(h.Recent) == 0:
		roots = append(roots, top("hist", &node{kind: nInfo, label: "nothing run yet"}))
	case shown == 0:
		roots = append(roots, top("hist", &node{kind: nInfo, label: "no runs on " + conn + " yet"}))
	}
	return roots
}

// fillVersions puts a query's versions under it.
func fillVersions(n *node, vs []history.Entry, ic icons) {
	n.children = nil
	for i := range vs {
		e := vs[i]
		n.add(&node{kind: nHistory, label: runLabel(e), detail: firstLine(e.SQL), run: &e, icon: ic.history})
	}
	if len(vs) == 0 {
		n.add(&node{kind: nInfo, label: "no versions"})
	}
	n.loaded, n.loading = true, false
}

func runLabel(e history.Entry) string {
	when := e.Time.Local().Format("15:04")
	if e.Time.Local().YearDay() != timeNow().YearDay() {
		when = e.Time.Local().Format("Jan 2 15:04")
	}
	switch e.Status {
	case "error":
		return when + " ✗"
	case "cancelled":
		return when + " ⊘"
	}
	return when
}

// firstLine: the SQL on one line (the row truncates it).
func firstLine(sql string) string { return strings.Join(strings.Fields(sql), " ") }

// timeNow is time.Now (tests may pin it).
var timeNow = time.Now

// buildQueryFiles makes the Queries tab.
func buildQueryFiles(files []string, ic icons) []*node {
	var roots []*node
	for _, f := range files {
		roots = append(roots, top("files", &node{kind: nQueryFile, label: shortPath(f), file: f, icon: ic.file}))
	}
	if len(files) == 0 {
		roots = append(roots, top("files", &node{kind: nInfo, label: "none yet: n creates query_1.sql"}))
	}
	return roots
}
