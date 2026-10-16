package database

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/raikhan/sqlh/parser/parseutil"
)

// RelationColumn is an output column of a derived table or CTE, with the
// database column it comes from when that is known.
type RelationColumn struct {
	Name        string
	OriginTable string      // table the column is taken from, "" for expressions
	Origin      *ColumnDesc // nil when unknown
}

// RelationColumns lists the columns of a derived table/CTE, expanding star
// columns of database tables against the cache.
func RelationColumns(info *parseutil.SubQueryInfo, dbCache *DBCache) []*RelationColumn {
	var cols []*RelationColumn
	seen := map[string]bool{}
	add := func(c *RelationColumn) {
		key := strings.ToUpper(c.Name)
		if c.Name == "" || seen[key] {
			return
		}
		seen[key] = true
		cols = append(cols, c)
	}
	for _, view := range info.Views {
		for _, sc := range view.SubQueryColumns {
			if sc.ColumnName == "*" {
				if sc.ParentTable == nil || dbCache == nil {
					continue
				}
				tableCols, _ := tableColumns(dbCache, sc.ParentTable)
				for _, tc := range tableCols {
					if sc.NameOnly { // a UNION branch's star: the names, not the values
						add(&RelationColumn{Name: tc.Name})
						continue
					}
					add(&RelationColumn{Name: tc.Name, OriginTable: sc.ParentTable.Name, Origin: tc})
				}
				continue
			}
			rc := &RelationColumn{Name: sc.DisplayName()}
			if sc.ParentTable != nil {
				rc.OriginTable = sc.ParentTable.Name
				if dbCache != nil {
					rc.Origin = lookupColumn(dbCache, sc.ParentTable, sc.ColumnName)
				}
			}
			add(rc)
		}
	}
	return cols
}

func tableColumns(dbCache *DBCache, t *parseutil.TableInfo) ([]*ColumnDesc, bool) {
	if t.DatabaseSchema != "" {
		return dbCache.ColumnDatabase(t.DatabaseSchema, t.Name)
	}
	return dbCache.ColumnDescs(t.Name)
}

func lookupColumn(dbCache *DBCache, t *parseutil.TableInfo, name string) *ColumnDesc {
	cols, ok := tableColumns(dbCache, t)
	if !ok {
		return nil
	}
	for _, c := range cols {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}

func relationKind(info *parseutil.SubQueryInfo) string {
	if info.Kind == parseutil.SubQueryCTE {
		if info.CTE != "" && !strings.EqualFold(info.CTE, info.Name) {
			return "CTE " + info.CTE
		}
		return "CTE"
	}
	return "subquery"
}

// RelationDoc documents a derived table/CTE: its columns with types and origin.
func RelationDoc(info *parseutil.SubQueryInfo, dbCache *DBCache) string {
	buf := new(bytes.Buffer)
	name := info.Name
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Fprintf(buf, "`%s` %s\n\n", name, relationKind(info))
	cols := RelationColumns(info, dbCache)
	if len(cols) == 0 {
		fmt.Fprintln(buf, "columns unknown")
		return buf.String()
	}
	rows := make([][]string, 0, len(cols))
	for _, c := range cols {
		typ, from := "-", "-"
		if c.Origin != nil {
			typ = c.Origin.Type
			from = c.OriginTable + "." + c.Origin.Name
		} else if c.OriginTable != "" {
			from = c.OriginTable
		}
		rows = append(rows, []string{c.Name, typ, from})
	}
	fmt.Fprintln(buf, "```")
	fmt.Fprint(buf, alignedTable([]string{"Name", "Type", "From"}, rows))
	fmt.Fprintln(buf, "```")
	return buf.String()
}

// RelationColumnDoc documents one column of a derived table/CTE.
func RelationColumnDoc(info *parseutil.SubQueryInfo, col *RelationColumn) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s`.`%s` column (%s)\n\n", info.Name, col.Name, relationKind(info))
	switch {
	case col.Origin != nil:
		fmt.Fprintln(buf, col.Origin.OnelineDesc())
		fmt.Fprintln(buf)
		fmt.Fprintf(buf, "from `%s`.`%s`\n", col.OriginTable, col.Origin.Name)
	case col.OriginTable != "":
		fmt.Fprintf(buf, "from `%s`\n", col.OriginTable)
	default:
		fmt.Fprintln(buf, "expression")
	}
	return buf.String()
}

// FindRelationColumn returns the column of a derived table/CTE named name.
func FindRelationColumn(info *parseutil.SubQueryInfo, dbCache *DBCache, name string) *RelationColumn {
	for _, c := range RelationColumns(info, dbCache) {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}
