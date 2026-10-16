package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/raikhan/sqlh/dialect"
)

var (
	ErrNotImplementation error = errors.New("not implementation")
)

const (
	DefaultMaxIdleConns = 10
	DefaultMaxOpenConns = 5
)

type DBRepository interface {
	Driver() dialect.DatabaseDriver
	CurrentDatabase(ctx context.Context) (string, error)
	Databases(ctx context.Context) ([]string, error)
	CurrentSchema(ctx context.Context) (string, error)
	Schemas(ctx context.Context) ([]string, error)
	SchemaTables(ctx context.Context) (map[string][]string, error)
	DescribeDatabaseTable(ctx context.Context) ([]*ColumnDesc, error)
	DescribeDatabaseTableBySchema(ctx context.Context, schemaName string) ([]*ColumnDesc, error)
	Exec(ctx context.Context, query string) (sql.Result, error)
	Query(ctx context.Context, query string) (*sql.Rows, error)
	DescribeForeignKeysBySchema(ctx context.Context, schemaName string) ([]*ForeignKey, error)
}

// SchemaFilterer is implemented by repositories that can restrict metadata
// (schemas, tables, columns) to an allow-list of schemas.
type SchemaFilterer interface {
	SetSchemaFilter(schemas []string)
}

// SynonymLister is implemented by repositories whose tables may also be
// reached through synonyms (Oracle): owner → synonym names, for PUBLIC and
// the allowed schemas.
type SynonymLister interface {
	Synonyms(ctx context.Context) (map[string][]string, error)
}

type DBOption struct {
	MaxIdleConns int
	MaxOpenConns int
}

type ColumnBase struct {
	Schema string
	Table  string
	Name   string
}

type ColumnDesc struct {
	ColumnBase
	Type    string
	Null    string
	Key     string
	Default sql.NullString
	Extra   string
}

type ForeignKey [][2]*ColumnBase

type fkItemDesc struct {
	fkID      string
	schema    string
	table     string
	column    string
	refTable  string
	refColumn string
}

func (cd *ColumnDesc) OnelineDesc() string {
	items := []string{}
	if cd.Type != "" {
		items = append(items, "`"+cd.Type+"`")
	}
	if cd.Key == "YES" {
		items = append(items, "PRIMARY KEY")
	} else if cd.Key != "" && cd.Key != "NO" {
		items = append(items, cd.Key)
	}
	if cd.Extra != "" {
		items = append(items, cd.Extra)
	}
	return strings.Join(items, " ")
}

func ColumnDoc(tableName string, colDesc *ColumnDesc) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s`.`%s` column", tableName, colDesc.Name)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf, colDesc.OnelineDesc())
	return buf.String()
}

func Coalesce(str ...string) string {
	for _, s := range str {
		if s != "" {
			return s
		}
	}
	return ""
}

func TableDoc(tableName string, cols []*ColumnDesc) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s` table", tableName)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	// Rendered as a fenced block with padded columns: clients such as Helix show
	// markdown tables as raw text, so pipe tables do not line up.
	rows := make([][]string, 0, len(cols))
	for _, col := range cols {
		key := col.Key
		if key == "" {
			key = "-"
		}
		rows = append(rows, []string{col.Name, col.Type, key, Coalesce(col.Default.String, "-"), col.Extra})
	}
	fmt.Fprintln(buf, "```")
	fmt.Fprint(buf, alignedTable([]string{"Name", "Type", "PK", "Default", "Extra"}, rows))
	fmt.Fprintln(buf, "```")
	return buf.String()
}

// alignedTable renders rows as space-padded columns; trailing empty columns are dropped.
func alignedTable(headers []string, rows [][]string) string {
	n := len(headers)
	// drop trailing columns that are empty in every row
	for n > 1 {
		empty := true
		for _, r := range rows {
			if n-1 < len(r) && strings.TrimSpace(r[n-1]) != "" {
				empty = false
				break
			}
		}
		if !empty {
			break
		}
		n--
	}
	widths := make([]int, n)
	for i := 0; i < n; i++ {
		widths[i] = utf8.RuneCountInString(headers[i])
	}
	for _, r := range rows {
		for i := 0; i < n && i < len(r); i++ {
			if w := utf8.RuneCountInString(r[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	line := func(cells []string) string {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			if i == n-1 {
				parts[i] = c
			} else {
				parts[i] = c + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c))
			}
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	buf := new(bytes.Buffer)
	fmt.Fprintln(buf, line(headers))
	seps := make([]string, n)
	for i := range seps {
		seps[i] = strings.Repeat("-", widths[i])
	}
	fmt.Fprintln(buf, line(seps))
	for _, r := range rows {
		fmt.Fprintln(buf, line(r))
	}
	return buf.String()
}

func parseForeignKeys(rows *sql.Rows, schemaName string) ([]*ForeignKey, error) {
	var retVal []*ForeignKey
	var prevFk string
	var cur *ForeignKey
	for rows.Next() {
		var fkItem fkItemDesc
		err := rows.Scan(
			&fkItem.fkID,
			&fkItem.table,
			&fkItem.column,
			&fkItem.refTable,
			&fkItem.refColumn,
		)
		if err != nil {
			return nil, err
		}
		var l, r ColumnBase
		l.Schema = schemaName
		l.Table = fkItem.table
		l.Name = fkItem.column
		r.Schema = l.Schema
		r.Table = fkItem.refTable
		r.Name = fkItem.refColumn
		if fkItem.fkID != prevFk {
			if cur != nil {
				retVal = append(retVal, cur)
			}
			cur = new(ForeignKey)
		}
		*cur = append(*cur, [2]*ColumnBase{&l, &r})
		prevFk = fkItem.fkID
	}

	if cur != nil {
		retVal = append(retVal, cur)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return retVal, nil
}

// ViewDef is a view or materialized view of a schema.
type ViewDef struct {
	Schema       string
	Name         string
	Materialized bool
	// Query is the view's SELECT ("" when unknown).
	Query string
	// Uses: the tables and views of the same schema the view reads directly,
	// from the database's dependency catalog. Nil when the database has none
	// (then they are taken from Query).
	Uses []string
	// DDL: the whole CREATE statement when the database keeps it (SQLite);
	// else CreateStatement builds one from Query.
	DDL string
	// Writes: the DML the view accepts, by itself or through INSTEAD OF
	// triggers/rules; nil when unknown.
	Writes *ViewWrites
}

// ViewWrites tells which statements can write through a view.
type ViewWrites struct{ Insert, Update, Delete bool }

// ReadOnly: no INSERT, UPDATE or DELETE.
func (w *ViewWrites) ReadOnly() bool { return w != nil && !w.Insert && !w.Update && !w.Delete }

// Allows reports whether op (INSERT, UPDATE, DELETE; MERGE: any of them) is
// accepted; unknown writes allow everything.
func (w *ViewWrites) Allows(op string) bool {
	if w == nil {
		return true
	}
	switch strings.ToUpper(op) {
	case "INSERT":
		return w.Insert
	case "UPDATE":
		return w.Update
	case "DELETE":
		return w.Delete
	}
	return w.Insert || w.Update || w.Delete
}

// Summary: "read-only", "no INSERT", "only UPDATE" …; "" when every write
// works or it is unknown.
func (w *ViewWrites) Summary() string {
	if w == nil {
		return ""
	}
	var yes, no []string
	for _, op := range []struct {
		name string
		ok   bool
	}{{"INSERT", w.Insert}, {"UPDATE", w.Update}, {"DELETE", w.Delete}} {
		if op.ok {
			yes = append(yes, op.name)
		} else {
			no = append(no, op.name)
		}
	}
	switch {
	case len(no) == 0:
		return ""
	case len(yes) == 0:
		return "read-only"
	case len(no) == 1:
		return "no " + no[0]
	}
	return "only " + yes[0]
}

// CreateStatement is a CREATE statement for the view (its definition as a
// file), ending with ";".
func (v *ViewDef) CreateStatement() string {
	stmt := strings.TrimSpace(v.DDL)
	if stmt == "" {
		name := v.Name
		if v.Schema != "" {
			name = v.Schema + "." + name
		}
		create := "CREATE OR REPLACE VIEW"
		if v.Materialized {
			create = "CREATE MATERIALIZED VIEW"
		}
		q := strings.TrimRight(v.Query, " \t\r\n")
		if strings.TrimSpace(q) == "" {
			q = "  -- the query is not available"
		}
		stmt = create + " " + name + " AS\n" + strings.TrimLeft(q, "\r\n")
	}
	if !strings.HasSuffix(stmt, ";") {
		stmt += ";"
	}
	return stmt + "\n"
}

// ViewDescriber is implemented by repositories that can list a schema's views
// with their definitions and dependencies.
type ViewDescriber interface {
	DescribeViews(ctx context.Context, schemaName string) ([]*ViewDef, error)
}

// IsPrimaryKey reports whether a ColumnDesc.Key marks a primary key column
// (YES: PostgreSQL, Oracle, SQLite; PRI: MySQL).
func IsPrimaryKey(key string) bool {
	k := strings.ToUpper(key)
	return k == "YES" || k == "PRI" || k == "P"
}

// maxDocRelations caps the relationships listed in a table's documentation.
const maxDocRelations = 12

// TableDoc documents a table or view: kind, columns and, when known, its
// relationships (hover, completion).
func (dc *DBCache) TableDoc(tableName string, cols []*ColumnDesc) string {
	var g *RelGraph
	if dc != nil {
		g = dc.Relations
	}
	kind := g.Kind(tableName)
	doc := TableDoc(tableName, cols)
	if kind != RelTable {
		title := string(kind)
		if v := g.View(tableName); v != nil {
			if w := v.Writes.Summary(); w != "" {
				title += " (" + w + ")"
			}
		}
		doc = strings.Replace(doc, "` table", "` "+title, 1)
	}
	if g == nil {
		return doc
	}
	var joins, reads, usedBy []string
	var rows []string
	code := func(s string) string { return "`" + s + "`" } // highlighted like the title
	for _, r := range g.Related(tableName, true) {
		switch {
		case r.Edge.Kind == EdgeUses && r.Out:
			reads = append(reads, r.Other)
		case r.Edge.Kind == EdgeUses:
			usedBy = append(usedBy, r.Other)
		default:
			if len(rows) < maxDocRelations {
				row := r.Marker() + " " + r.ConditionWith(code)
				if n := r.Note(); n != "" {
					row += "  (" + n + ")"
				}
				rows = append(rows, row)
			}
			joins = append(joins, r.Other)
		}
	}
	buf := new(bytes.Buffer)
	buf.WriteString(doc)
	if len(rows) > 0 {
		fmt.Fprintln(buf)
		fmt.Fprintln(buf, "Relationships")
		fmt.Fprintln(buf)
		if n := len(joins) - len(rows); n > 0 {
			rows = append(rows, fmt.Sprintf("… %d more (sqlh describe %s)", n, tableName))
		}
		// one per line: markdown hard breaks (two trailing spaces)
		fmt.Fprintln(buf, strings.Join(rows, "  \n"))
	}
	if len(reads) > 0 {
		fmt.Fprintln(buf)
		fmt.Fprintln(buf, "Reads: "+joinCapped(reads, maxDocRelations))
	}
	if len(usedBy) > 0 {
		fmt.Fprintln(buf)
		fmt.Fprintln(buf, "Used by views: "+joinCapped(usedBy, maxDocRelations))
	}
	return buf.String()
}

// maxDocOrigins caps the base columns listed for a view column.
const maxDocOrigins = 5

// ColumnDoc documents a column; a view's column says where its values come
// from (several base columns for a UNION).
func (dc *DBCache) ColumnDoc(tableName string, col *ColumnDesc) string {
	doc := ColumnDoc(tableName, col)
	if dc == nil {
		return doc
	}
	o := dc.Relations.ColumnOrigins(tableName, col.Name)
	if len(o) == 0 {
		return doc
	}
	return doc + "\nfrom " + OriginList(o) + "\n"
}

// OriginList renders base columns ("table.col") as inline code, capped:
// `a.x`, `b.x`, … (9).
func OriginList(origins []string) string {
	parts := make([]string, 0, maxDocOrigins+1)
	for i, o := range origins {
		if i == maxDocOrigins {
			parts = append(parts, fmt.Sprintf("… (%d)", len(origins)))
			break
		}
		parts = append(parts, "`"+o+"`")
	}
	return strings.Join(parts, ", ")
}

func joinCapped(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(", … %d more", len(names)-n)
}
