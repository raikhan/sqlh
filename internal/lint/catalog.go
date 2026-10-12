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
	// unqualified: further names usable without a schema (Oracle synonyms).
	unqualified map[string]bool
	// unqualifiedComplete: every name usable without a schema is known, so an
	// unknown unqualified table really is unknown.
	unqualifiedComplete bool
}

// CatalogBuilder fills a Catalog.
type CatalogBuilder struct{ c *Catalog }

func NewCatalog(d sqlsplit.Dialect, defaultSchema string) *CatalogBuilder {
	return &CatalogBuilder{c: &Catalog{
		Dialect:       d,
		defaultSchema: strings.ToUpper(defaultSchema),
		schemas:       map[string]bool{},
		tables:        map[string]map[string]bool{},
		columns:       map[string]map[string]bool{},
		unqualified:   map[string]bool{},
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

func (b *CatalogBuilder) Build() *Catalog { return b.c }

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
