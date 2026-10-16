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
	nInfo        nodeKind = iota // a message line (loading…, nothing here)
	nStatement                   // "In this statement"
	nStmtTable                   // a table of the statement at the cursor
	nConnection                  // the current connection
	nConnections                 // "Connections"
	nConnItem                    // a configured connection
	nSchema                      // a schema
	nGroup                       // a group of objects (Tables, Views, Relations, Indexes, …)
	nTable                       // a table, view or materialized view
	nRelation                    // a table reached through a relationship
	nColumn                      // a column
	nObject                      // a sequence, type, index, constraint or trigger
	nHistoryRoot                 // "History"
	nHistory                     // a past run
	nQueryFiles                  // "Query files" (sqlh tui)
	nQueryFile                   // a query file
)

// node is a row of the tree. Children of tables are built lazily (they need
// the server's columns and relationships).
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
	ref       string            // statement tables: alias or name in the statement
	obj       control.CatalogObject
	conn      control.Connection
	groupKind string // groups: what they hold (relations, columns, …)
	run       *history.Entry
	file      string // query files
	version   bool   // history: a version of the statement at the cursor (Enter replaces it)
}

func (n *node) add(c *node) *node {
	c.parent = n
	c.depth = n.depth + 1
	c.id = n.id + "/" + c.label
	if c.kind == nRelation || c.kind == nColumn {
		c.id += "\x00" + c.detail // the same table may be related twice
	}
	n.children = append(n.children, c)
	return c
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

// tableKinds are the kinds listed as tables (expandable).
func isTableKind(k string) bool {
	return k == "" || k == "table" || k == "view" || k == "materialized view"
}

// buildCatalog makes the connection's subtree.
func buildCatalog(cat *control.CatalogResult, ic icons) *node {
	title := cat.Connection
	if cat.Database != "" && !strings.EqualFold(cat.Database, cat.Connection) {
		title += " › " + cat.Database
	}
	root := &node{kind: nConnection, label: title, icon: ic.connection, expanded: true, id: "conn"}
	for _, s := range cat.Schemas {
		label := s.Name
		if label == "" {
			label = "(default)" // SQLite
		}
		sn := root.add(&node{kind: nSchema, label: label, icon: ic.schema, schema: s.Name, expanded: s.Default})
		if s.Default {
			sn.detail = "current"
		}
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
				expanded: s.Default && (g.kind == "table" || g.kind == "")})
			for _, o := range objs {
				if isTableKind(o.Kind) {
					t := gn.add(&node{kind: nTable, label: o.Name, table: o.Insert, tableKind: o.Kind,
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
	return root
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

// buildConnections makes the "Connections" subtree.
func buildConnections(conns []control.Connection, ic icons) *node {
	root := &node{kind: nConnections, label: "Connections", icon: ic.connections, id: "conns"}
	for _, c := range conns {
		n := root.add(&node{kind: nConnItem, label: c.Alias, detail: c.Driver + " " + c.Desc, conn: c, icon: ic.connection})
		if c.Current {
			n.detail = "● " + n.detail
		}
	}
	if len(conns) == 0 {
		root.add(&node{kind: nInfo, label: "no connections configured"})
	}
	return root
}

// fillTable builds a table's children: Relations, Indexes, Constraints,
// Triggers groups, then the columns.
func fillTable(n *node, cols []control.ColumnItem, rel *control.RelationsResult, objs []control.CatalogObject, ic icons) {
	n.children = nil
	if rel != nil {
		var joins, uses []control.RelationItem
		for _, r := range rel.Relations {
			if r.Edge == "uses" && len(r.Cols) == 0 {
				uses = append(uses, r)
			} else {
				joins = append(joins, r)
			}
		}
		if len(joins) > 0 {
			g := n.add(&node{kind: nGroup, label: "Relations", detail: fmt.Sprint(len(joins)), groupKind: "relations", table: n.table})
			for i := range joins {
				addRelation(g, n, joins[i], ic)
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
				addRelation(g, n, r, ic)
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

// addRelation adds a related table under a group of table t.
func addRelation(g, t *node, r control.RelationItem, ic icons) {
	anchor, path := t.table, []control.JoinHop(nil)
	if t.kind == nRelation {
		anchor, path = t.anchor, append([]control.JoinHop(nil), t.path...)
	}
	path = append(path, control.JoinHop{Table: r.Insert, Cols: r.Cols})
	detail := r.Condition
	if detail == "" && r.Note != "" {
		detail = r.Note
	} else if r.Note != "" {
		detail += "  " + r.Note
	}
	rr := r
	g.add(&node{kind: nRelation, label: r.Marker + " " + r.Insert, detail: detail, table: r.Insert, tableKind: r.Kind,
		rel: &rr, anchor: anchor, path: path, lazy: true, icon: ""})
}

// buildStatement makes the "In this statement" subtree: the statement's
// tables (expandable), then the tables related to them that it doesn't use.
func buildStatement(ctx *control.ContextResult, rels map[string]*control.RelationsResult, cur *control.Cursor, ic icons) *node {
	root := &node{kind: nStatement, label: "In this statement", icon: ic.statement, expanded: true, id: "stmt"}
	if cur != nil {
		root.detail = shortPath(cur.File) + fmt.Sprintf(":%d", cur.Line)
		if ctx != nil && ctx.InJoin {
			root.detail += " · after JOIN"
		}
	}
	if ctx == nil || len(ctx.Tables) == 0 {
		root.add(&node{kind: nInfo, label: "no tables at the cursor"})
		return root
	}
	inStmt := map[string]bool{}
	for _, t := range ctx.Tables {
		inStmt[strings.ToUpper(t.Name)] = true
	}
	tables := append([]control.ScopeTable(nil), ctx.Tables...)
	// the focused table first
	for i, t := range tables {
		if ctx.Focus != "" && strings.EqualFold(t.Name, ctx.Focus) && i > 0 {
			tables = append([]control.ScopeTable{t}, append(tables[:i:i], tables[i+1:]...)...)
			break
		}
	}
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
		root.add(&node{kind: nStmtTable, label: label, table: insert, ref: t.Ref, lazy: true, icon: ic.kind("table")})
	}
	seen := map[string]bool{}
	for _, t := range tables {
		name := t.Name
		if t.Schema != "" {
			name = t.Schema + "." + t.Name
		}
		r := rels[strings.ToUpper(name)]
		if r == nil {
			continue
		}
		anchor := &node{kind: nTable, table: r.Insert}
		for _, it := range r.Relations {
			if inStmt[strings.ToUpper(it.Table)] || len(it.Cols) == 0 {
				continue
			}
			key := strings.ToUpper(it.Table) + "\t" + it.Condition
			if seen[key] {
				continue
			}
			seen[key] = true
			n := &node{kind: nRelation, label: it.Marker + " " + it.Insert, table: it.Insert, tableKind: it.Kind,
				anchor: anchor.table, path: []control.JoinHop{{Table: it.Insert, Cols: it.Cols}}, lazy: true}
			rr := it
			n.rel = &rr
			var cond []string
			for _, c := range it.Cols {
				cond = append(cond, t.Ref+"."+c[0])
			}
			n.detail = strings.Join(cond, ", ")
			if it.Note != "" {
				n.detail += "  " + it.Note
			}
			root.add(n)
		}
	}
	return root
}

func shortPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func isPK(key string) bool {
	k := strings.ToUpper(key)
	return k == "YES" || k == "PRI" || k == "P"
}

// searchItem is a search candidate: an object of the catalog.
type searchItem struct {
	text   string // matched: the name
	detail string // schema · kind
	obj    control.CatalogObject
	schema string
}

func searchItems(cat *control.CatalogResult) []searchItem {
	var out []searchItem
	if cat == nil {
		return out
	}
	for _, s := range cat.Schemas {
		for _, o := range s.Objects {
			if o.Table != "" && o.Kind != "index" {
				continue // constraints and triggers: found through their table
			}
			kind := o.Kind
			if kind == "" {
				kind = "table/view"
			}
			out = append(out, searchItem{text: o.Insert, detail: kind, obj: o, schema: s.Name})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].text) < strings.ToLower(out[j].text) })
	return out
}

// buildHistory makes the "History" subtree: the versions of the statement
// at the cursor, then the workspace's recent runs.
func buildHistory(h *control.HistoryResult, ic icons) *node {
	root := &node{kind: nHistoryRoot, label: "History", icon: ic.history, id: "hist", expanded: true}
	if h == nil {
		root.add(&node{kind: nInfo, label: "loading…"})
		return root
	}
	if h.Statement != "" {
		g := root.add(&node{kind: nGroup, label: "This statement", detail: fmt.Sprint(len(h.Versions)), expanded: true, groupKind: "versions"})
		if len(h.Versions) == 0 {
			g.add(&node{kind: nInfo, label: "not run yet"})
		}
		for i := range h.Versions {
			e := h.Versions[i]
			n := g.add(&node{kind: nHistory, label: runLabel(e), detail: firstLine(e.SQL), run: &e, version: true})
			if e.SQL == h.Statement {
				n.detail = "= current · " + n.detail
			}
		}
	}
	g := root.add(&node{kind: nGroup, label: "Recent runs", detail: fmt.Sprint(len(h.Recent)), groupKind: "recent"})
	if len(h.Recent) == 0 {
		g.add(&node{kind: nInfo, label: "nothing run yet"})
	}
	for i := range h.Recent {
		e := h.Recent[i]
		g.add(&node{kind: nHistory, label: runLabel(e), detail: firstLine(e.SQL), run: &e})
	}
	return root
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

// buildQueryFiles makes the "Query files" subtree (sqlh tui).
func buildQueryFiles(files []string, ic icons) *node {
	root := &node{kind: nQueryFiles, label: "Query files", icon: ic.statement, id: "files", expanded: true}
	root.detail = "n: new query"
	for _, f := range files {
		root.add(&node{kind: nQueryFile, label: shortPath(f), file: f})
	}
	if len(files) == 0 {
		root.add(&node{kind: nInfo, label: "none yet: n creates query_1.sql"})
	}
	return root
}
