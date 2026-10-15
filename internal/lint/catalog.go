package lint

import (
	"strings"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// Catalog is what the checks know about the database: an immutable snapshot
// built from the schema cache. Names are matched case-insensitively.
type Catalog struct {
	Dialect sqlsplit.Dialect

	defaultSchema string
	schemas       map[string]bool            // upper
	tables        map[string]map[string]bool // schema → table (upper)
	columns       map[string]map[string]bool // "SCHEMA\tTABLE" → column (upper)
	// writes: "SCHEMA\tTABLE" → what can write through a view
	writes map[string]viewWrites
	// unqualified: further names usable without a schema (Oracle synonyms).
	unqualified map[string]bool
	// unqualifiedComplete: every name usable without a schema is known, so an
	// unknown unqualified table really is unknown.
	unqualifiedComplete bool
	// kinds: "SCHEMA\tTABLE" → "view" / "materialized view" (tables absent);
	// kindsKnown: schemas whose views are all recorded
	kinds      map[string]string
	kindsKnown map[string]bool
	// objects: kind → "SCHEMA\tNAME" → the (upper) tables of the objects of
	// that name ("" for none); objectsKnown: they are loaded
	objects      map[string]map[string][]string
	objectsKnown bool
	// objectsPartial: kind \t SCHEMA whose list may be incomplete
	objectsPartial map[string]bool
}

// CatalogBuilder fills a Catalog.
type CatalogBuilder struct{ c *Catalog }

func NewCatalog(d sqlsplit.Dialect, defaultSchema string) *CatalogBuilder {
	return &CatalogBuilder{c: &Catalog{
		Dialect:        d,
		defaultSchema:  strings.ToUpper(defaultSchema),
		schemas:        map[string]bool{},
		tables:         map[string]map[string]bool{},
		columns:        map[string]map[string]bool{},
		writes:         map[string]viewWrites{},
		unqualified:    map[string]bool{},
		kinds:          map[string]string{},
		kindsKnown:     map[string]bool{},
		objects:        map[string]map[string][]string{},
		objectsPartial: map[string]bool{},
	}}
}

func (b *CatalogBuilder) Schema(name string) *CatalogBuilder {
	b.c.schemas[strings.ToUpper(name)] = true
	return b
}

// Table records a table/view; columns nil = columns not known.
func (b *CatalogBuilder) Table(schema, table string, columns []string) *CatalogBuilder {
	s, t := strings.ToUpper(schema), strings.ToUpper(table)
	if b.c.tables[s] == nil {
		b.c.tables[s] = map[string]bool{}
	}
	b.c.tables[s][t] = true
	b.c.schemas[s] = true
	if columns != nil {
		m := make(map[string]bool, len(columns))
		for _, col := range columns {
			m[strings.ToUpper(col)] = true
		}
		b.c.columns[s+"\t"+t] = m
	}
	return b
}

type viewWrites struct {
	materialized           bool
	insert, update, delete bool
}

// Writes records which statements can write through a view.
func (b *CatalogBuilder) Writes(schema, view string, materialized, insert, update, delete bool) *CatalogBuilder {
	b.c.writes[strings.ToUpper(schema)+"\t"+strings.ToUpper(view)] = viewWrites{materialized, insert, update, delete}
	return b
}

// Unqualified records a name usable without a schema (e.g. a public synonym).
func (b *CatalogBuilder) Unqualified(name string) *CatalogBuilder {
	b.c.unqualified[strings.ToUpper(name)] = true
	return b
}

// Complete declares that all names usable without a schema are recorded.
func (b *CatalogBuilder) Complete(v bool) *CatalogBuilder {
	b.c.unqualifiedComplete = v
	return b
}

// Kind records a relation's kind ("view", "materialized view"; tables need
// no record).
func (b *CatalogBuilder) Kind(schema, table, kind string) *CatalogBuilder {
	if kind != "" && kind != "table" {
		b.c.kinds[strings.ToUpper(schema)+"\t"+strings.ToUpper(table)] = kind
	}
	return b
}

// KindsKnown declares that every view of a schema is recorded with Kind.
func (b *CatalogBuilder) KindsKnown(schema string) *CatalogBuilder {
	b.c.kindsKnown[strings.ToUpper(schema)] = true
	return b
}

// Object records an index, constraint, sequence or trigger (table: the
// table it belongs to, "" for none).
func (b *CatalogBuilder) Object(kind, schema, name, table string) *CatalogBuilder {
	m := b.c.objects[kind]
	if m == nil {
		m = map[string][]string{}
		b.c.objects[kind] = m
	}
	k := strings.ToUpper(schema) + "\t" + strings.ToUpper(name)
	m[k] = append(m[k], strings.ToUpper(table))
	return b
}

// ObjectsPartial declares that the objects of a kind in a schema may be
// incomplete (the user can't see them all): unknown ones aren't reported.
func (b *CatalogBuilder) ObjectsPartial(kind, schema string) *CatalogBuilder {
	b.c.objectsPartial[kind+"\t"+strings.ToUpper(schema)] = true
	return b
}

// ObjectsKnown declares that the objects are loaded.
func (b *CatalogBuilder) ObjectsKnown(v bool) *CatalogBuilder {
	b.c.objectsKnown = v
	return b
}

func (b *CatalogBuilder) Build() *Catalog { return b.c }

// kindOf: a resolved relation's kind, when known.
func (c *Catalog) kindOf(r resolved) (string, bool) {
	if !c.kindsKnown[r.schema] {
		return "", false
	}
	if k, ok := c.kinds[r.schema+"\t"+r.table]; ok {
		return k, true
	}
	return "table", true
}

// findObject reports whether an object exists (sure: it can tell).
func (c *Catalog) findObject(kind, schema, name, table string) (found, sure bool) {
	if !c.objectsKnown {
		return true, false
	}
	n, t := strings.ToUpper(name), strings.ToUpper(table)
	match := func(tables []string) bool {
		if t == "" {
			return true
		}
		for _, x := range tables {
			if x == t {
				return true
			}
		}
		return false
	}
	if schema == "" && !c.unqualifiedComplete {
		return true, false // the current schema isn't cached (Oracle allow-list)
	}
	m := c.objects[kind]
	if c.objectsPartial[kind+"\t"+c.defaultSchema] && schema == "" ||
		c.objectsPartial[kind+"\t"+strings.ToUpper(schema)] {
		return true, false
	}
	if schema != "" {
		s := strings.ToUpper(schema)
		if !c.schemas[s] {
			return true, false // schema not cached
		}
		tables, ok := m[s+"\t"+n]
		return ok && match(tables), true
	}
	if tables, ok := m[c.defaultSchema+"\t"+n]; ok && match(tables) {
		return true, true
	}
	if c.Dialect == sqlsplit.PostgreSQL { // search_path unknown: any schema
		for k, tables := range m {
			if strings.HasSuffix(k, "\t"+n) && match(tables) {
				return true, true
			}
		}
	}
	return false, true
}

// objectNames lists the names of a kind of object in a schema (suggestions).
func (c *Catalog) objectNames(kind, schema string) []string {
	s := c.defaultSchema
	if schema != "" {
		s = strings.ToUpper(schema)
	}
	var out []string
	for k := range c.objects[kind] {
		if strings.HasPrefix(k, s+"\t") {
			out = append(out, k[len(s)+1:])
		}
	}
	return out
}

func (c *Catalog) hasSchema(s string) bool { return c.schemas[strings.ToUpper(s)] }

// resolved is a table a reference points to; schema "" = not resolvable.
type resolved struct{ schema, table string }

// resolve finds a table. found: it exists (or might: unsure); res.schema is
// set only when the exact table is known (for column checks).
func (c *Catalog) resolve(schema, table string) (res resolved, found, sure bool) {
	t := strings.ToUpper(table)
	if schema != "" {
		s := strings.ToUpper(schema)
		tbls, ok := c.tables[s]
		if !ok {
			return res, true, false // schema not cached (allow-list, other db): unsure
		}
		if tbls[t] {
			return resolved{s, t}, true, true
		}
		return res, false, true
	}
	if c.tables[c.defaultSchema][t] {
		return resolved{c.defaultSchema, t}, true, true
	}
	if c.unqualified[t] {
		return res, true, true // synonym: exists, columns unknown
	}
	if c.Dialect == sqlsplit.PostgreSQL {
		// search_path isn't known: accept a table in any one schema.
		var hits []string
		for s, tbls := range c.tables {
			if tbls[t] {
				hits = append(hits, s)
			}
		}
		if len(hits) == 1 {
			return resolved{hits[0], t}, true, true
		}
		if len(hits) > 1 {
			return res, true, true
		}
	}
	if !c.unqualifiedComplete {
		return res, true, false
	}
	return res, false, true
}

// columnsOf returns a resolved table's columns (ok=false: not known).
func (c *Catalog) columnsOf(r resolved) (map[string]bool, bool) {
	if r.schema == "" && r.table == "" {
		return nil, false
	}
	m, ok := c.columns[r.schema+"\t"+r.table]
	return m, ok
}

// tableNames lists the default schema's tables (for suggestions).
func (c *Catalog) tableNames(schema string) []string {
	s := c.defaultSchema
	if schema != "" {
		s = strings.ToUpper(schema)
	}
	var out []string
	for t := range c.tables[s] {
		out = append(out, t)
	}
	return out
}
