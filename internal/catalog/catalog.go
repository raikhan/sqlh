// Package catalog turns schema metadata into the listings shown by the CLI
// pickers (schemas, tables, columns), from the server's cache or, standalone,
// straight from the database.
package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
)

// Source provides metadata.
type Source interface {
	DefaultSchema() string
	Schemas() []string
	// SchemaTables maps schema (database spelling, any case) to its tables.
	SchemaTables() map[string][]string
	// Columns looks a table up case-insensitively.
	Columns(schema, table string) ([]*database.ColumnDesc, bool)
}

// Options control rendering.
type Options struct {
	Driver dialect.DatabaseDriver
	Lower  bool // lowercaseIdentifiers
	All    bool // include system schemas
}

func (o Options) ident(name string) string { return database.QuoteIdent(name, o.Driver, o.Lower) }

// System reports schemas hidden unless All is set.
func System(driver dialect.DatabaseDriver, schema string) bool {
	if driver != dialect.DatabaseDriverPostgreSQL {
		return false // Oracle: the allow-list already limits schemas
	}
	s := strings.ToLower(schema)
	return s == "pg_catalog" || s == "information_schema" || strings.HasPrefix(s, "pg_toast") || strings.HasPrefix(s, "pg_temp")
}

// Schemas lists schemas, the default first.
func Schemas(src Source, opt Options) []control.SchemaItem {
	def := src.DefaultSchema()
	var out []control.SchemaItem
	seen := map[string]bool{}
	add := func(s string) {
		if seen[strings.ToUpper(s)] || (!opt.All && System(opt.Driver, s)) {
			return
		}
		seen[strings.ToUpper(s)] = true
		out = append(out, control.SchemaItem{Name: s, Default: strings.EqualFold(s, def)})
	}
	if def != "" {
		add(def)
	}
	names := src.Schemas()
	sort.Strings(names)
	for _, s := range names {
		add(s)
	}
	return out
}

// Tables lists tables (and views), default schema first; schema filters.
func Tables(src Source, schema string, opt Options) []control.TableItem {
	def := src.DefaultSchema()
	var out []control.TableItem
	for s, tables := range src.SchemaTables() {
		if schema != "" && !strings.EqualFold(s, schema) {
			continue
		}
		if schema == "" && !opt.All && System(opt.Driver, s) {
			continue
		}
		isDef := strings.EqualFold(s, def)
		for _, t := range tables {
			item := control.TableItem{Schema: s, Name: t, Default: isDef, Insert: opt.ident(t)}
			if !isDef && s != "" {
				item.Insert = opt.ident(s) + "." + opt.ident(t)
			}
			if cols, ok := src.Columns(s, t); ok {
				item.Columns = len(cols)
			}
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		if !strings.EqualFold(out[i].Schema, out[j].Schema) {
			return strings.ToLower(out[i].Schema) < strings.ToLower(out[j].Schema)
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Columns lists a table's columns. table may be "t", "s.t" or quoted forms
// (as produced by TableItem.Insert); matching is case-insensitive for
// unquoted parts.
func Columns(src Source, table string, opt Options) ([]control.ColumnItem, error) {
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
	cols, ok := src.Columns(schema, name)
	if !ok {
		return nil, fmt.Errorf("unknown table %q", table)
	}
	out := make([]control.ColumnItem, 0, len(cols))
	for _, c := range cols {
		out = append(out, control.ColumnItem{Name: c.Name, Type: c.Type, Null: c.Null, Key: c.Key, Insert: opt.ident(c.Name)})
	}
	return out, nil
}

// splitName splits a.b / "a"."b c" into unquoted parts.
func splitName(s string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && inQuote && i+1 < len(s) && s[i+1] == '"':
			b.WriteByte('"')
			i++
		case c == '"':
			inQuote = !inQuote
		case c == '.' && !inQuote:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	return append(parts, b.String())
}

// CacheSource adapts the server's schema cache.
type CacheSource struct{ C *database.DBCache }

func (s CacheSource) DefaultSchema() string { return s.C.DefaultSchema() }
func (s CacheSource) Schemas() []string     { return s.C.SortedSchemas() }
func (s CacheSource) SchemaTables() map[string][]string {
	out := map[string][]string{}
	for k, v := range s.C.SchemaTables {
		name := k
		if real, ok := s.C.Schemas[strings.ToUpper(k)]; ok && real != "" {
			name = real
		} else if strings.EqualFold(k, s.C.DefaultSchema()) {
			name = s.C.DefaultSchema()
		}
		out[name] = v
	}
	return out
}
func (s CacheSource) Columns(schema, table string) ([]*database.ColumnDesc, bool) {
	return s.C.ColumnDatabase(schema, table)
}

// Relations returns the cache's relationship graph.
func (s CacheSource) Relations() (*database.RelGraph, error) { return s.C.Relations, nil }
