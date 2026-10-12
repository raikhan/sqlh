package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/raikhan/sqlh/dialect"
	go_ora "github.com/sijms/go-ora/v2"
)

func init() {
	RegisterOpen("oracle", oracleOpen)
	RegisterFactory("oracle", NewOracleDBRepository)
}

func oracleOpen(dbConnCfg *DBConfig) (*DBConnection, error) {
	DSName, err := genOracleConfig(dbConnCfg)
	if err != nil {
		return nil, err
	}

	// go-ora is a pure Go driver: no Oracle Instant Client needed.
	conn, err := sql.Open("oracle", DSName)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	conn.SetMaxIdleConns(DefaultMaxIdleConns)
	conn.SetMaxOpenConns(DefaultMaxOpenConns)

	return &DBConnection{
		Conn:   conn,
		Driver: dialect.DatabaseDriverOracle,
	}, nil
}

// genOracleConfig returns a go-ora URL: oracle://user:pass@host:port/service?opts.
// dataSourceName is used verbatim when set. Params are passed as URL options
// (e.g. TRACE FILE, SSL, "connStr" for a full TNS descriptor).
func genOracleConfig(connCfg *DBConfig) (string, error) {
	if connCfg.DataSourceName != "" {
		return connCfg.DataSourceName, nil
	}

	host, port := connCfg.Host, connCfg.Port
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 1521
	}
	return go_ora.BuildUrl(host, port, connCfg.DBName, connCfg.User, connCfg.Passwd, connCfg.Params), nil
}

type OracleDBRepository struct {
	Conn *sql.DB
	// schemas is the metadata allow-list (upper-cased). Empty means the
	// login/current schema only: an unfiltered ALL_TAB_COLUMNS is ~100k rows on
	// a typical instance and full of SYS/XDB noise.
	schemas []string
}

func NewOracleDBRepository(conn *sql.DB) DBRepository {
	return &OracleDBRepository{Conn: conn}
}

// SetSchemaFilter implements SchemaFilterer.
func (db *OracleDBRepository) SetSchemaFilter(schemas []string) {
	db.schemas = nil
	for _, s := range schemas {
		if s = strings.TrimSpace(s); s != "" {
			// unquoted Oracle identifiers are stored upper-case
			db.schemas = append(db.schemas, strings.ToUpper(s))
		}
	}
}

// ownerFilter returns a predicate on the given OWNER column plus bind args.
func (db *OracleDBRepository) ownerFilter(col string) (string, []interface{}) {
	if len(db.schemas) == 0 {
		return col + " = SYS_CONTEXT('USERENV','CURRENT_SCHEMA')", nil
	}
	binds := make([]string, len(db.schemas))
	args := make([]interface{}, len(db.schemas))
	for i, s := range db.schemas {
		binds[i] = fmt.Sprintf(":%d", i+1)
		args[i] = s
	}
	return col + " IN (" + strings.Join(binds, ", ") + ")", args
}

func (db *OracleDBRepository) Driver() dialect.DatabaseDriver {
	return dialect.DatabaseDriverOracle
}

func (db *OracleDBRepository) CurrentDatabase(ctx context.Context) (string, error) {
	row := db.Conn.QueryRowContext(ctx, "SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL")
	var database string
	if err := row.Scan(&database); err != nil {
		return "", err
	}
	return database, nil
}

// Databases lists schemas (Oracle has one database per connection; users are
// schemas). Restricted to the allow-list so schema completion matches the cache.
func (db *OracleDBRepository) Databases(ctx context.Context) ([]string, error) {
	cur, err := db.CurrentDatabase(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{cur: true}
	out := []string{cur}
	for _, s := range db.schemas {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func (db *OracleDBRepository) CurrentSchema(ctx context.Context) (string, error) {
	return db.CurrentDatabase(ctx)
}

func (db *OracleDBRepository) Schemas(ctx context.Context) ([]string, error) {
	return db.Databases(ctx)
}

// SchemaTables returns tables and views per owner.
func (db *OracleDBRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	pred, args := db.ownerFilter("OWNER")
	query := `
SELECT OWNER, OBJECT_NAME
  FROM ALL_OBJECTS
 WHERE OBJECT_TYPE IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW')
   AND OBJECT_NAME NOT LIKE 'BIN$%'
   AND ` + pred + `
 ORDER BY OWNER, OBJECT_NAME`
	rows, err := db.Conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	databaseTables := map[string][]string{}
	seen := map[string]bool{}
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			return nil, err
		}
		// a materialized view also appears as a TABLE object
		if k := schema + "." + table; seen[k] {
			continue
		} else {
			seen[k] = true
		}
		databaseTables[schema] = append(databaseTables[schema], table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return databaseTables, nil
}

func (db *OracleDBRepository) Tables(ctx context.Context) ([]string, error) {
	rows, err := db.Conn.QueryContext(ctx, "SELECT TABLE_NAME FROM USER_TABLES UNION SELECT VIEW_NAME FROM USER_VIEWS ORDER BY 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := []string{}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

// oracleColumnsQuery describes columns of tables and views. The type is
// rendered with length/precision (VARCHAR2(100), NUMBER(10,2)); Key is YES for
// primary key columns. DATA_DEFAULT (a LONG) is not selected: LONG columns
// cannot be used in expressions and are slow to fetch, and defaults are rarely
// useful for completion.
const oracleColumnsQuery = `
SELECT c.OWNER,
       c.TABLE_NAME,
       c.COLUMN_NAME,
       CASE
         WHEN c.DATA_TYPE IN ('VARCHAR2', 'NVARCHAR2', 'CHAR', 'NCHAR', 'RAW')
           THEN c.DATA_TYPE || '(' || NVL(NULLIF(c.CHAR_LENGTH, 0), c.DATA_LENGTH) || ')'
         WHEN c.DATA_TYPE = 'NUMBER' AND c.DATA_PRECISION IS NOT NULL AND NVL(c.DATA_SCALE, 0) > 0
           THEN 'NUMBER(' || c.DATA_PRECISION || ',' || c.DATA_SCALE || ')'
         WHEN c.DATA_TYPE = 'NUMBER' AND c.DATA_PRECISION IS NOT NULL
           THEN 'NUMBER(' || c.DATA_PRECISION || ')'
         ELSE c.DATA_TYPE
       END,
       CASE c.NULLABLE WHEN 'Y' THEN 'YES' ELSE 'NO' END,
       CASE WHEN pk.COLUMN_NAME IS NOT NULL THEN 'YES' ELSE 'NO' END
  FROM ALL_TAB_COLUMNS c
  LEFT JOIN (
        SELECT cc.OWNER, cc.TABLE_NAME, cc.COLUMN_NAME
          FROM ALL_CONSTRAINTS k
          JOIN ALL_CONS_COLUMNS cc
            ON cc.OWNER = k.OWNER AND cc.CONSTRAINT_NAME = k.CONSTRAINT_NAME
         WHERE k.CONSTRAINT_TYPE = 'P' AND %[1]s
       ) pk
    ON pk.OWNER = c.OWNER AND pk.TABLE_NAME = c.TABLE_NAME AND pk.COLUMN_NAME = c.COLUMN_NAME
 WHERE %[2]s
   AND c.TABLE_NAME NOT LIKE 'BIN$%%'
 ORDER BY c.OWNER, c.TABLE_NAME, c.COLUMN_ID`

func (db *OracleDBRepository) describeColumns(ctx context.Context, query string, args ...interface{}) ([]*ColumnDesc, error) {
	rows, err := db.Conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tableInfos := []*ColumnDesc{}
	for rows.Next() {
		var tableInfo ColumnDesc
		err := rows.Scan(
			&tableInfo.Schema,
			&tableInfo.Table,
			&tableInfo.Name,
			&tableInfo.Type,
			&tableInfo.Null,
			&tableInfo.Key,
		)
		if err != nil {
			return nil, err
		}
		tableInfos = append(tableInfos, &tableInfo)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tableInfos, nil
}

// DescribeDatabaseTable describes all tables in the allow-listed schemas.
func (db *OracleDBRepository) DescribeDatabaseTable(ctx context.Context) ([]*ColumnDesc, error) {
	if len(db.schemas) == 0 {
		pred := "c.OWNER = SYS_CONTEXT('USERENV','CURRENT_SCHEMA')"
		pkPred := "k.OWNER = SYS_CONTEXT('USERENV','CURRENT_SCHEMA')"
		return db.describeColumns(ctx, fmt.Sprintf(oracleColumnsQuery, pkPred, pred))
	}
	// The same owner list is needed twice; go-ora binds positionally, so
	// number the second set after the first.
	n := len(db.schemas)
	b1, b2 := make([]string, n), make([]string, n)
	args := make([]interface{}, 0, 2*n)
	for i, s := range db.schemas {
		b1[i] = fmt.Sprintf(":%d", i+1)
		b2[i] = fmt.Sprintf(":%d", n+i+1)
		args = append(args, s)
	}
	for _, s := range db.schemas {
		args = append(args, s)
	}
	query := fmt.Sprintf(oracleColumnsQuery,
		"k.OWNER IN ("+strings.Join(b1, ", ")+")",
		"c.OWNER IN ("+strings.Join(b2, ", ")+")")
	return db.describeColumns(ctx, query, args...)
}

func (db *OracleDBRepository) DescribeDatabaseTableBySchema(ctx context.Context, schemaName string) ([]*ColumnDesc, error) {
	query := fmt.Sprintf(oracleColumnsQuery, "k.OWNER = :1", "c.OWNER = :2")
	cols, err := db.describeColumns(ctx, query, schemaName, schemaName)
	if err != nil {
		log.Println("schema", schemaName, err.Error())
	}
	return cols, err
}

func (db *OracleDBRepository) DescribeForeignKeysBySchema(ctx context.Context, schemaName string) ([]*ForeignKey, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
		SELECT a.CONSTRAINT_NAME,
		   a.TABLE_NAME,
		   a.COLUMN_NAME,
		   b.TABLE_NAME,
		   b.COLUMN_NAME
	FROM ALL_CONS_COLUMNS a
			 JOIN ALL_CONSTRAINTS c ON a.OWNER = c.OWNER
		AND a.CONSTRAINT_NAME = c.CONSTRAINT_NAME
			 JOIN ALL_CONSTRAINTS c_pk ON c.R_OWNER = c_pk.OWNER
		AND c.R_CONSTRAINT_NAME = c_pk.CONSTRAINT_NAME
			 JOIN ALL_CONS_COLUMNS b ON b.OWNER = c_pk.OWNER
		AND b.CONSTRAINT_NAME = c_pk.CONSTRAINT_NAME
		AND b.POSITION = a.POSITION
	WHERE c.CONSTRAINT_TYPE = 'R'
	  AND a.OWNER = :1
	ORDER BY a.CONSTRAINT_NAME, a.POSITION
		`, schemaName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return parseForeignKeys(rows, schemaName)
}

func (db *OracleDBRepository) Exec(ctx context.Context, query string) (sql.Result, error) {
	return db.Conn.ExecContext(ctx, query)
}

func (db *OracleDBRepository) Query(ctx context.Context, query string) (*sql.Rows, error) {
	return db.Conn.QueryContext(ctx, query)
}
