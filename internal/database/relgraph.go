package database

// The relationship graph of a schema: which tables and views can be joined,
// and how. Built from the schema cache (no queries of its own):
//
//   - declared foreign keys (EdgeFK);
//   - view lineage (EdgeViaView): a view column that is a plain reference to a
//     base table column inherits that column's foreign keys, so a view exposing
//     orders.customer_id joins customer like orders does (views built on views
//     are followed down to the base tables);
//   - guesses from column names (EdgeInferred, shown as "maybe"): orders.customer_id
//     → customer.id, only when exactly one table matches and the types agree;
//   - view dependencies (EdgeUses): the tables and views a view reads; they
//     carry no join columns.
//
// Schemas are boundaries: only relations of one schema (the connection's
// default) are included.

import (
	"log"
	"sort"
	"strings"

	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
)

// RelKind is the kind of a relation.
type RelKind string

const (
	RelTable   RelKind = "table"
	RelView    RelKind = "view"
	RelMatView RelKind = "materialized view"
)

// EdgeKind is how two relations are connected.
type EdgeKind string

const (
	EdgeFK       EdgeKind = "fk"    // declared foreign key
	EdgeViaView  EdgeKind = "view"  // a view exposes the columns of a base table's relationship
	EdgeInferred EdgeKind = "maybe" // guessed from column names and types
	EdgeUses     EdgeKind = "uses"  // From (a view) reads To; no join columns
)

// ColPair is one column of a join condition: From.col = To.col.
type ColPair struct{ From, To string }

// Edge connects two relations. For joinable edges From holds the referencing
// columns (the child side) and To the referenced ones.
type Edge struct {
	From, To string // cached spelling
	Kind     EdgeKind
	Cols     []ColPair
	// Nullable: a From column allows NULL (some rows have no match).
	Nullable bool
	// Unique: the From columns are From's primary key (one-to-one).
	Unique bool
	// Via: EdgeViaView, the base table whose relationship the view exposes.
	Via string
	// Inferred: guessed (EdgeInferred, or a view exposing a guessed one).
	Inferred bool
	// ViewOnly: listed only for the view end (EdgeViaView). Otherwise a table
	// referenced by many views would list every view exposing its key, and its
	// children every such view too; from the table, Edges shows the views that
	// read it (EdgeUses) instead.
	ViewOnly bool
}

// viewEnd returns the end of a ViewOnly edge that is the view (the other end
// is always a base table).
func (g *RelGraph) viewEnd(e *Edge) string {
	if g.views[strings.ToUpper(e.From)] != nil {
		return e.From
	}
	return e.To
}

// Joinable reports whether the edge has join columns.
func (e *Edge) Joinable() bool { return e.Kind != EdgeUses && len(e.Cols) > 0 }

// Other returns the relation at the other end from name, and whether the edge
// goes out of name (name is From: it references the other / reads it).
func (e *Edge) Other(name string) (string, bool) {
	if strings.EqualFold(e.From, name) {
		return e.To, true
	}
	return e.From, false
}

func (e *Edge) key() string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(e.From) + "\x00" + strings.ToUpper(e.To))
	if e.Kind == EdgeUses {
		b.WriteString("\x00uses")
	}
	for _, c := range e.Cols {
		b.WriteString("\x00" + strings.ToUpper(c.From) + "=" + strings.ToUpper(c.To))
	}
	return b.String()
}

func (e *Edge) rank() int {
	switch {
	case e.Kind == EdgeFK:
		return 0
	case e.Kind == EdgeViaView && !e.Inferred:
		return 1
	case e.Kind == EdgeUses:
		return 3
	default: // guesses
		return 2
	}
}

// AsForeignKey renders a joinable edge in the form used by join completion.
func (e *Edge) AsForeignKey(schema string) *ForeignKey {
	fk := ForeignKey{}
	for _, c := range e.Cols {
		fk = append(fk, [2]*ColumnBase{
			{Schema: schema, Table: e.From, Name: c.From},
			{Schema: schema, Table: e.To, Name: c.To},
		})
	}
	return &fk
}

// RelGraph is the relationship graph of one schema.
type RelGraph struct {
	schema string
	names  map[string]string  // UPPER → cached spelling
	kinds  map[string]RelKind // UPPER → kind (absent: table)
	views  map[string]*ViewDef
	byRel  map[string][]*Edge // UPPER → edges touching it, sorted
	edges  []*Edge
	// ViewsLoaded: view kinds, dependencies and lineage are included (they come
	// with the background load).
	ViewsLoaded bool

	joinKeys map[string]map[string][]*ForeignKey
	fkEdge   map[*ForeignKey]*Edge
}

// RelGraphInput is what a graph is built from.
type RelGraphInput struct {
	Schema      string
	Tables      []string // all relations (tables and views) of the schema
	Columns     func(table string) []*ColumnDesc
	ForeignKeys []*ForeignKey
	Views       []*ViewDef // nil: not loaded (yet)
}

// Schema is the graph's schema.
func (g *RelGraph) Schema() string { return g.schema }

// Canonical returns the cached spelling of a relation name.
func (g *RelGraph) Canonical(name string) (string, bool) {
	if g == nil {
		return "", false
	}
	n, ok := g.names[strings.ToUpper(name)]
	return n, ok
}

// Kind returns the kind of a relation (table when unknown).
func (g *RelGraph) Kind(name string) RelKind {
	if g != nil {
		if k, ok := g.kinds[strings.ToUpper(name)]; ok {
			return k
		}
	}
	return RelTable
}

// View returns a view's definition, nil for tables.
func (g *RelGraph) View(name string) *ViewDef {
	if g == nil {
		return nil
	}
	return g.views[strings.ToUpper(name)]
}

// Edges returns the edges listed for a relation: declared foreign keys first,
// then those exposed by views (for views), guesses, and view dependencies.
func (g *RelGraph) Edges(name string) []*Edge {
	if g == nil {
		return nil
	}
	return g.byRel[strings.ToUpper(name)]
}

// Between returns all edges between two relations (ViewOnly ones included).
func (g *RelGraph) Between(a, b string) []*Edge {
	if g == nil {
		return nil
	}
	var out []*Edge
	for _, e := range g.edges {
		if (strings.EqualFold(e.From, a) && strings.EqualFold(e.To, b)) || (strings.EqualFold(e.From, b) && strings.EqualFold(e.To, a)) {
			out = append(out, e)
		}
	}
	return out
}

// AllEdges returns every edge.
func (g *RelGraph) AllEdges() []*Edge {
	if g == nil {
		return nil
	}
	return g.edges
}

// JoinKeys maps table → related table → join conditions (as foreign keys),
// for join completion. Guesses are included only with maybe.
func (g *RelGraph) JoinKeys(maybe bool) map[string]map[string][]*ForeignKey {
	if maybe {
		return g.joinKeys
	}
	out := map[string]map[string][]*ForeignKey{}
	for t, m := range g.joinKeys {
		for o, fks := range m {
			for _, fk := range fks {
				if e := g.fkEdge[fk]; e != nil && e.Inferred {
					continue
				}
				if out[t] == nil {
					out[t] = map[string][]*ForeignKey{}
				}
				out[t][o] = append(out[t][o], fk)
			}
		}
	}
	return out
}

// EdgeOf returns the edge a JoinKeys entry was made from.
func (g *RelGraph) EdgeOf(fk *ForeignKey) *Edge {
	if g == nil {
		return nil
	}
	return g.fkEdge[fk]
}

// BuildRelGraph builds the graph of a schema.
func BuildRelGraph(in RelGraphInput) *RelGraph {
	g := &RelGraph{
		schema:      in.Schema,
		names:       map[string]string{},
		kinds:       map[string]RelKind{},
		views:       map[string]*ViewDef{},
		byRel:       map[string][]*Edge{},
		ViewsLoaded: in.Views != nil,
		joinKeys:    map[string]map[string][]*ForeignKey{},
		fkEdge:      map[*ForeignKey]*Edge{},
	}
	for _, t := range in.Tables {
		g.names[strings.ToUpper(t)] = t
	}
	for _, v := range in.Views {
		u := strings.ToUpper(v.Name)
		if _, ok := g.names[u]; !ok {
			g.names[u] = v.Name
		}
		g.views[u] = v
		if v.Materialized {
			g.kinds[u] = RelMatView
		} else {
			g.kinds[u] = RelView
		}
	}
	cols := func(t string) []*ColumnDesc {
		if in.Columns == nil {
			return nil
		}
		return in.Columns(t)
	}
	b := &graphBuilder{g: g, cols: cols, seen: map[string]*Edge{}, compound: map[string]bool{}}

	// declared foreign keys (the original values, so join completion is unchanged)
	fkCols := map[string]bool{} // UPPER table \x00 UPPER column: part of a declared FK
	for _, fk := range in.ForeignKeys {
		if fk == nil || len(*fk) == 0 {
			continue
		}
		from, ok1 := g.Canonical((*fk)[0][0].Table)
		to, ok2 := g.Canonical((*fk)[0][1].Table)
		if !ok1 || !ok2 {
			continue // a table outside the schema (or not visible)
		}
		e := &Edge{From: from, To: to, Kind: EdgeFK}
		for _, p := range *fk {
			e.Cols = append(e.Cols, ColPair{From: p[0].Name, To: p[1].Name})
			fkCols[strings.ToUpper(from)+"\x00"+strings.ToUpper(p[0].Name)] = true
		}
		b.annotate(e)
		if b.add(e) {
			g.fkEdge[fk] = e
		}
	}

	b.inferEdges(fkCols, false)
	if in.Views != nil {
		b.viewEdges(in.Views)
		// after the edges views inherit, which take precedence
		b.inferEdges(fkCols, true)
	}

	// index
	for _, e := range g.edges {
		if e.ViewOnly {
			v := strings.ToUpper(g.viewEnd(e))
			g.byRel[v] = append(g.byRel[v], e)
			continue
		}
		fu, tu := strings.ToUpper(e.From), strings.ToUpper(e.To)
		g.byRel[fu] = append(g.byRel[fu], e)
		if tu != fu {
			g.byRel[tu] = append(g.byRel[tu], e)
		}
	}
	for k, es := range g.byRel {
		sort.SliceStable(es, func(i, j int) bool {
			if es[i].rank() != es[j].rank() {
				return es[i].rank() < es[j].rank()
			}
			oi, _ := es[i].Other(k)
			oj, _ := es[j].Other(k)
			return strings.ToLower(oi) < strings.ToLower(oj)
		})
	}
	// join completion: table → other → conditions (both directions)
	declared := make(map[*Edge]*ForeignKey, len(g.fkEdge))
	for f, e := range g.fkEdge {
		declared[e] = f
	}
	for _, e := range g.edges {
		if !e.Joinable() {
			continue
		}
		fk := declared[e]
		if fk == nil {
			fk = e.AsForeignKey(g.schema)
			g.fkEdge[fk] = e
		}
		if e.ViewOnly {
			v := g.viewEnd(e)
			o, _ := e.Other(v)
			g.addJoinKey(v, o, fk)
			continue
		}
		g.addJoinKey(e.From, e.To, fk)
		if !strings.EqualFold(e.From, e.To) {
			g.addJoinKey(e.To, e.From, fk)
		}
	}
	return g
}

func (g *RelGraph) addJoinKey(a, b string, fk *ForeignKey) {
	m := g.joinKeys[a]
	if m == nil {
		m = map[string][]*ForeignKey{}
		g.joinKeys[a] = m
	}
	m[b] = append(m[b], fk)
}

type graphBuilder struct {
	g    *RelGraph
	cols func(string) []*ColumnDesc
	seen map[string]*Edge
	// compound: views with set operators (UNION, …): no guesses for their columns
	compound map[string]bool
}

func (b *graphBuilder) add(e *Edge) bool {
	k := e.key()
	if _, dup := b.seen[k]; dup {
		return false
	}
	b.seen[k] = e
	b.g.edges = append(b.g.edges, e)
	return true
}

func (b *graphBuilder) column(table, name string) *ColumnDesc {
	for _, c := range b.cols(table) {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}

// annotate sets Nullable and Unique from the From side's columns.
func (b *graphBuilder) annotate(e *Edge) {
	var pk []string
	for _, c := range b.cols(e.From) {
		if IsPrimaryKey(c.Key) {
			pk = append(pk, strings.ToUpper(c.Name))
		}
	}
	matched := 0
	for _, p := range e.Cols {
		if c := b.column(e.From, p.From); c != nil {
			if strings.EqualFold(c.Null, "YES") || strings.EqualFold(c.Null, "Y") {
				e.Nullable = true
			}
			for _, k := range pk {
				if k == strings.ToUpper(p.From) {
					matched++
				}
			}
		}
	}
	e.Unique = len(pk) > 0 && matched == len(pk) && len(e.Cols) == len(pk)
}

// ---- guesses ----------------------------------------------------------------

// inferEdges guesses relationships from names: a column named like another
// table's single-column primary key — customer_id for customer.id, or for a
// key that is itself named after its table (customer.customer_id) the same
// name. Skipped when the column is part of a declared foreign key, when more
// than one table matches, or when the types differ. views: guess for the
// columns of views (else of tables).
func (b *graphBuilder) inferEdges(fkCols map[string]bool, views bool) {
	g := b.g
	type target struct{ table, pk, typ string }
	byName := map[string][]target{}
	for u, t := range g.names {
		if g.kinds[u] == RelView {
			continue // no keys
		}
		var pk []*ColumnDesc
		for _, c := range b.cols(t) {
			if IsPrimaryKey(c.Key) {
				pk = append(pk, c)
			}
		}
		if len(pk) != 1 {
			continue
		}
		p := strings.ToLower(pk[0].Name)
		names := map[string]bool{}
		for _, stem := range stems(t) {
			if p == "id" {
				names[stem+"_id"] = true
				names[stem+"id"] = true
			} else if strings.HasPrefix(p, stem) && len(p) > len(stem) {
				names[p] = true
			}
		}
		for n := range names {
			byName[n] = append(byName[n], target{t, pk[0].Name, pk[0].Type})
		}
	}
	for u, s := range g.names {
		if _, isView := g.views[u]; isView != views || b.compound[u] {
			continue
		}
		for _, c := range b.cols(s) {
			if fkCols[u+"\x00"+strings.ToUpper(c.Name)] {
				continue
			}
			ts := byName[strings.ToLower(c.Name)]
			distinct := map[string]bool{}
			for _, t := range ts {
				distinct[strings.ToUpper(t.table)] = true
			}
			if len(distinct) != 1 {
				continue
			}
			t := ts[0]
			if strings.EqualFold(t.table, s) || !sameTypeFamily(c.Type, t.typ) {
				continue
			}
			e := &Edge{From: s, To: t.table, Kind: EdgeInferred, Inferred: true,
				Cols: []ColPair{{From: c.Name, To: t.pk}}}
			b.annotate(e)
			b.add(e)
		}
	}
}

// stems: a table name and its singular forms, lower case.
func stems(table string) []string {
	t := strings.ToLower(table)
	out := []string{t}
	switch {
	case strings.HasSuffix(t, "ies") && len(t) > 3:
		out = append(out, t[:len(t)-3]+"y")
	case strings.HasSuffix(t, "sses"), strings.HasSuffix(t, "xes"), strings.HasSuffix(t, "ches"), strings.HasSuffix(t, "shes"):
		out = append(out, t[:len(t)-2])
	case strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") && len(t) > 1:
		out = append(out, t[:len(t)-1])
	}
	return out
}

func typeFamily(typ string) string {
	t := strings.ToLower(typ)
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	switch {
	case t == "":
		return ""
	case strings.Contains(t, "int"), strings.Contains(t, "num"), strings.Contains(t, "dec"),
		strings.Contains(t, "serial"), strings.Contains(t, "real"), strings.Contains(t, "float"),
		strings.Contains(t, "double"):
		return "number"
	case strings.Contains(t, "char"), strings.Contains(t, "text"), strings.Contains(t, "string"):
		return "text"
	case strings.Contains(t, "uuid"), strings.Contains(t, "uniqueidentifier"):
		return "uuid"
	case strings.Contains(t, "date"), strings.Contains(t, "time"):
		return "time"
	}
	return t
}

func sameTypeFamily(a, b string) bool {
	fa, fb := typeFamily(a), typeFamily(b)
	return fa == "" || fb == "" || fa == fb
}

// ---- views ------------------------------------------------------------------

// baseCol is the base table column a view column comes from. inst tells apart
// two references to the same table in a view (aliases).
type baseCol struct{ table, col, inst string }

type viewInfo struct {
	def  *ViewDef
	out  map[string]*baseCol // UPPER output column → origin (nil: unknown)
	cols []string            // output column names in order (resolved ones)
	uses []string
}

func (b *graphBuilder) viewEdges(views []*ViewDef) {
	g := b.g
	infos := map[string]*viewInfo{}
	for _, v := range views {
		infos[strings.ToUpper(v.Name)] = b.analyseView(v)
	}

	// dependencies
	for _, v := range views {
		vi := infos[strings.ToUpper(v.Name)]
		name, _ := g.Canonical(v.Name)
		for _, u := range vi.uses {
			if t, ok := g.Canonical(u); ok && !strings.EqualFold(t, name) {
				b.add(&Edge{From: name, To: t, Kind: EdgeUses})
			}
		}
	}

	// resolve view columns down to base tables (views on views)
	var resolve func(view, col string, depth int) *baseCol
	resolve = func(view, col string, depth int) *baseCol {
		vi := infos[strings.ToUpper(view)]
		if vi == nil || depth > 16 {
			return nil
		}
		bc := vi.out[strings.ToUpper(col)]
		if bc == nil {
			return nil
		}
		if infos[strings.ToUpper(bc.table)] != nil {
			inner := resolve(bc.table, bc.col, depth+1)
			if inner == nil {
				return nil
			}
			return &baseCol{table: inner.table, col: inner.col, inst: bc.inst + "/" + inner.inst}
		}
		return bc
	}

	// the relationships between base tables, which views inherit
	byTable := map[string][]*Edge{}
	for _, e := range g.edges {
		if !e.Joinable() || g.views[strings.ToUpper(e.From)] != nil || g.views[strings.ToUpper(e.To)] != nil {
			continue
		}
		byTable[strings.ToUpper(e.From)] = append(byTable[strings.ToUpper(e.From)], e)
		if !strings.EqualFold(e.From, e.To) {
			byTable[strings.ToUpper(e.To)] = append(byTable[strings.ToUpper(e.To)], e)
		}
	}

	for _, v := range views {
		vi := infos[strings.ToUpper(v.Name)]
		view, _ := g.Canonical(v.Name)
		// base table → instance → base column → view column
		exposed := map[string]map[string]map[string]string{}
		for _, oc := range vi.cols {
			bc := resolve(v.Name, oc, 0)
			if bc == nil {
				continue
			}
			tu := strings.ToUpper(bc.table)
			if exposed[tu] == nil {
				exposed[tu] = map[string]map[string]string{}
			}
			if exposed[tu][bc.inst] == nil {
				exposed[tu][bc.inst] = map[string]string{}
			}
			if _, dup := exposed[tu][bc.inst][strings.ToUpper(bc.col)]; !dup {
				exposed[tu][bc.inst][strings.ToUpper(bc.col)] = oc
			}
		}
		tables := make([]string, 0, len(exposed))
		for tu := range exposed {
			tables = append(tables, tu)
		}
		sort.Strings(tables)
		for _, tu := range tables {
			insts := make([]string, 0, len(exposed[tu]))
			for inst := range exposed[tu] {
				insts = append(insts, inst)
			}
			sort.Strings(insts)
			for _, inst := range insts {
				m := exposed[tu][inst]
				// the view exposes the base table's primary key: its rows join back
				if table, ok := g.Canonical(tu); ok && g.views[tu] == nil {
					var pk []ColPair
					for _, c := range b.cols(table) {
						if IsPrimaryKey(c.Key) {
							pk = append(pk, ColPair{From: c.Name, To: c.Name})
						}
					}
					if pairs, ok := mapCols(pk, m, true); ok && len(pk) > 0 {
						for i := range pairs {
							pairs[i].To = pk[i].To
						}
						b.add(&Edge{From: view, To: table, Kind: EdgeViaView, Cols: pairs, Via: table, ViewOnly: true})
					}
				}
				for _, e := range byTable[tu] {
					if strings.EqualFold(e.From, tu) {
						if pairs, ok := mapCols(e.Cols, m, true); ok {
							// nullability as in the base table: catalogs report view columns as nullable
							b.add(&Edge{From: view, To: e.To, Kind: EdgeViaView, Cols: pairs, Via: e.From, Inferred: e.Inferred, Nullable: e.Nullable, ViewOnly: true})
						}
					}
					if strings.EqualFold(e.To, tu) {
						if pairs, ok := mapCols(e.Cols, m, false); ok {
							b.add(&Edge{From: e.From, To: view, Kind: EdgeViaView, Cols: pairs, Via: e.To, Inferred: e.Inferred, Nullable: e.Nullable, ViewOnly: true})
						}
					}
				}
			}
		}
	}
}

// mapCols replaces the base table side of cols with the view's columns
// (fromSide: the base table is From). All columns must be exposed.
func mapCols(cols []ColPair, exposed map[string]string, fromSide bool) ([]ColPair, bool) {
	out := make([]ColPair, 0, len(cols))
	for _, c := range cols {
		if fromSide {
			vc, ok := exposed[strings.ToUpper(c.From)]
			if !ok {
				return nil, false
			}
			out = append(out, ColPair{From: vc, To: c.To})
		} else {
			vc, ok := exposed[strings.ToUpper(c.To)]
			if !ok {
				return nil, false
			}
			out = append(out, ColPair{From: c.From, To: vc})
		}
	}
	return out, true
}

// analyseView parses a view's query: its dependencies (when the database gave
// none) and where each output column comes from. A view with set operators
// (UNION, …) gets no lineage: its rows may come from other tables.
func (b *graphBuilder) analyseView(v *ViewDef) (vi *viewInfo) {
	vi = &viewInfo{def: v, out: map[string]*baseCol{}, uses: v.Uses}
	if strings.TrimSpace(v.Query) == "" {
		return vi
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("view %s: cannot analyse the definition: %v", v.Name, r)
		}
	}()
	parsed, err := parser.Parse(v.Query)
	if err != nil {
		return vi
	}
	lin := parseutil.ExtractQueryLineage(parsed)
	if vi.uses == nil {
		for _, t := range lin.Tables {
			if t.DatabaseSchema == "" || strings.EqualFold(unquote(t.DatabaseSchema), b.g.schema) {
				vi.uses = append(vi.uses, t.Name)
			}
		}
	}
	if lin.Compound {
		b.compound[strings.ToUpper(v.Name)] = true
		return vi
	}
	// the view's own column names (the catalog's spelling)
	viewCols := map[string]string{}
	for _, c := range b.cols(v.Name) {
		viewCols[strings.ToUpper(c.Name)] = c.Name
	}
	set := func(name string, bc *baseCol) {
		u := strings.ToUpper(name)
		real, ok := viewCols[u]
		if !ok {
			if len(viewCols) > 0 {
				return // not a column of the view (renamed by a column list)
			}
			real = name
		}
		if _, dup := vi.out[u]; dup {
			vi.out[u] = nil // the same name twice: ambiguous
			return
		}
		vi.out[u] = bc
		if bc != nil {
			vi.cols = append(vi.cols, real)
		}
	}
	for _, c := range lin.Columns {
		t := c.ParentTable
		if t == nil {
			set(c.DisplayName(), nil)
			continue
		}
		if t.DatabaseSchema != "" && !strings.EqualFold(unquote(t.DatabaseSchema), b.g.schema) {
			set(c.DisplayName(), nil)
			continue
		}
		table, ok := b.g.Canonical(t.Name)
		if !ok {
			set(c.DisplayName(), nil)
			continue
		}
		inst := strings.ToUpper(t.Alias)
		if inst == "" {
			inst = strings.ToUpper(t.Name)
		}
		if c.ColumnName == "*" {
			for _, tc := range b.cols(table) {
				set(tc.Name, &baseCol{table: table, col: tc.Name, inst: inst})
			}
			continue
		}
		set(c.DisplayName(), &baseCol{table: table, col: c.ColumnName, inst: inst})
	}
	// drop names that turned out ambiguous
	cols := vi.cols[:0]
	for _, n := range vi.cols {
		if vi.out[strings.ToUpper(n)] != nil {
			cols = append(cols, n)
		}
	}
	vi.cols = cols
	return vi
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '`' || s[0] == '[') {
		return s[1 : len(s)-1]
	}
	return s
}

// ---- views of the graph from one relation -------------------------------------

// Related is an edge seen from one of its ends (the anchor).
type Related struct {
	Edge  *Edge
	Other string
	// Out: the anchor holds the referencing columns (or, for EdgeUses, reads Other).
	Out bool
	// Cols: From is the anchor's column, To the other's.
	Cols []ColPair
}

// Related lists the edges of a relation as seen from it; guesses only with maybe.
func (g *RelGraph) Related(name string, maybe bool) []Related {
	var out []Related
	for _, e := range g.Edges(name) {
		if e.Inferred && !maybe {
			continue
		}
		out = append(out, seenFrom(e, name))
	}
	return out
}

func seenFrom(e *Edge, name string) Related {
	other, isOut := e.Other(name)
	r := Related{Edge: e, Other: other, Out: isOut}
	for _, c := range e.Cols {
		if isOut {
			r.Cols = append(r.Cols, c)
		} else {
			r.Cols = append(r.Cols, ColPair{From: c.To, To: c.From})
		}
	}
	return r
}

// JoinVia returns the join columns between a view and a table it reads, seen
// from anchor, when the view exposes the table's primary key (nil otherwise).
func (g *RelGraph) JoinVia(anchor, other string) []ColPair {
	view, table := anchor, other
	if g.View(anchor) == nil {
		view, table = other, anchor
	}
	for _, e := range g.Between(view, table) {
		if e.Kind == EdgeViaView && strings.EqualFold(e.From, view) && strings.EqualFold(e.To, table) && strings.EqualFold(e.Via, table) {
			return seenFrom(e, anchor).Cols
		}
	}
	return nil
}

// Marker: → the anchor references Other, ← Other references the anchor,
// ◇ view dependency.
func (r Related) Marker() string {
	switch {
	case r.Edge.Kind == EdgeUses:
		return "◇"
	case r.Out:
		return "→"
	default:
		return "←"
	}
}

// Condition renders the join columns as other.col = anchorCol [and …].
func (r Related) Condition() string {
	return r.ConditionWith(func(s string) string { return s })
}

// ConditionWith renders the join columns with the related table first, as
// other.col = anchorCol [and …]; name formats the related table's name.
func (r Related) ConditionWith(name func(string) string) string {
	parts := make([]string, 0, len(r.Cols))
	for _, c := range r.Cols {
		parts = append(parts, name(r.Other)+"."+c.To+" = "+c.From)
	}
	return strings.Join(parts, " and ")
}

// Note describes the edge briefly: "maybe", "via orders", "reads", ….
func (r Related) Note() string {
	var notes []string
	e := r.Edge
	switch e.Kind {
	case EdgeUses:
		if r.Out {
			return "reads"
		}
		return "view reads this"
	case EdgeViaView:
		notes = append(notes, "via "+e.Via)
	case EdgeFK, EdgeInferred:
	}
	if e.Inferred {
		notes = append(notes, "maybe")
	}
	if e.Unique {
		notes = append(notes, "1:1")
	}
	if e.Nullable && r.Out {
		notes = append(notes, "nullable")
	}
	return strings.Join(notes, ", ")
}
