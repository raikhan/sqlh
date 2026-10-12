package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/parser/parseutil"
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

func SubqueryDoc(name string, views []*parseutil.SubQueryView, dbCache *DBCache) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s subquery", name)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	for _, view := range views {
		for _, colmun := range view.SubQueryColumns {
			if colmun.ColumnName == "*" {
				tableCols, ok := dbCache.ColumnDescs(colmun.ParentTable.Name)
				if !ok {
					continue
				}
				for _, tableCol := range tableCols {
					fmt.Fprintf(buf, "- %s(%s.%s): %s", tableCol.Name, colmun.ParentTable.Name, tableCol.Name, tableCol.OnelineDesc())
					fmt.Fprintln(buf)
				}
			} else {
				columnDesc, ok := dbCache.Column(colmun.ParentTable.Name, colmun.ColumnName)
				if !ok {
					continue
				}
				fmt.Fprintf(buf, "- %s(%s.%s): %s", colmun.DisplayName(), colmun.ParentTable.Name, colmun.ColumnName, columnDesc.OnelineDesc())
				fmt.Fprintln(buf)

			}
		}
	}
	return buf.String()
}

func SubqueryColumnDoc(identName string, views []*parseutil.SubQueryView, dbCache *DBCache) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s subquery column", identName)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	for _, view := range views {
		for _, colmun := range view.SubQueryColumns {
			if colmun.ColumnName == "*" {
				tableCols, ok := dbCache.ColumnDescs(colmun.ParentTable.Name)
				if !ok {
					continue
				}
				for _, tableCol := range tableCols {
					if identName == tableCol.Name {
						fmt.Fprintf(buf, "- %s(%s.%s): %s", identName, colmun.ParentTable.Name, tableCol.Name, tableCol.OnelineDesc())
						fmt.Fprintln(buf)
						continue
					}
				}
			} else {
				if identName != colmun.ColumnName && identName != colmun.AliasName {
					continue
				}
				columnDesc, ok := dbCache.Column(colmun.ParentTable.Name, colmun.ColumnName)
				if !ok {
					continue
				}
				fmt.Fprintf(buf, "- %s(%s.%s): %s", identName, colmun.ParentTable.Name, colmun.ColumnName, columnDesc.OnelineDesc())
				fmt.Fprintln(buf)
			}
		}
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
