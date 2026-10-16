package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
)

// Tree lists the schemas of a cache with their objects for the panel: tables
// and views (told apart in the default schema once the views are loaded),
// then sequences, types, and the indexes, constraints and triggers of the
// tables (Table set).
func Tree(c *database.DBCache, opt Options) []control.CatalogSchema {
	src := CacheSource{C: c}
	def := src.DefaultSchema()
	g := c.Relations
	viewsLoaded := g != nil && g.ViewsLoaded
	byTable := src.SchemaTables()
	out := []control.CatalogSchema{}
	schemas := Schemas(src, opt)
	// SQLite and others may list tables under schemas Schemas() doesn't name
	listed := map[string]bool{}
	for _, s := range schemas {
		listed[strings.ToUpper(s.Name)] = true
	}
	var extra []string
	for name := range byTable {
		if !listed[strings.ToUpper(name)] && (opt.All || !System(opt.Driver, name)) {
			extra = append(extra, name)
			listed[strings.ToUpper(name)] = true
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		schemas = append(schemas, control.SchemaItem{Name: name, Default: strings.EqualFold(name, def) || len(schemas) == 0})
	}
	for _, s := range schemas {
		cs := control.CatalogSchema{Name: s.Name, Default: s.Default, Objects: []control.CatalogObject{}}
		var tables []string
		for name, ts := range byTable {
			if strings.EqualFold(name, s.Name) {
				tables = append(tables, ts...)
			}
		}
		sort.Slice(tables, func(i, j int) bool { return strings.ToLower(tables[i]) < strings.ToLower(tables[j]) })
		isDef := strings.EqualFold(s.Name, def)
		qualify := func(name string) string {
			if isDef || s.Name == "" {
				return opt.ident(name)
			}
			return opt.ident(s.Name) + "." + opt.ident(name)
		}
		for _, t := range tables {
			o := control.CatalogObject{Name: t, Insert: qualify(t)}
			if isDef && viewsLoaded {
				o.Kind = string(g.Kind(t))
			}
			if cols, ok := src.Columns(s.Name, t); ok {
				o.Columns = len(cols)
			}
			cs.Objects = append(cs.Objects, o)
		}
		for _, kind := range []string{database.ObjectSequence, database.ObjectType, database.ObjectIndex, database.ObjectConstraint, database.ObjectTrigger} {
			for _, x := range c.Objects.All(kind, s.Name) {
				o := control.CatalogObject{Kind: kind, Name: x.Name, Insert: qualify(x.Name), Table: x.Table, Detail: x.Detail}
				if len(x.Columns) > 0 {
					d := "(" + strings.Join(x.Columns, ", ") + ")"
					if x.Detail != "" {
						d = x.Detail + " " + d
					}
					if x.RefTable != "" {
						d += " → " + x.RefTable
					}
					o.Detail = d
				}
				cs.Objects = append(cs.Objects, o)
			}
		}
		out = append(out, cs)
	}
	return out
}

// Resolve finds a table as typed ("t", "s.t", quoted forms): its schema and
// name as the database spells them, its columns, and whether it is in the
// default schema.
func Resolve(src Source, table string) (schema, name string, cols []*database.ColumnDesc, isDefault bool, err error) {
	parts := splitName(table)
	schema = src.DefaultSchema()
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		schema, name = parts[0], parts[1]
	default:
		return "", "", nil, false, fmt.Errorf("invalid table name %q", table)
	}
	cols, ok := src.Columns(schema, name)
	if !ok {
		return "", "", nil, false, fmt.Errorf("unknown table %q", table)
	}
	for s, ts := range src.SchemaTables() {
		if !strings.EqualFold(s, schema) {
			continue
		}
		schema = s
		for _, t := range ts {
			if strings.EqualFold(t, name) {
				name = t
				break
			}
		}
	}
	return schema, name, cols, strings.EqualFold(schema, src.DefaultSchema()), nil
}
