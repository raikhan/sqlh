package database

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/stdlib"
	"github.com/raikhan/sqlh/dialect"
	"golang.org/x/crypto/ssh"
)

func init() {
	RegisterOpen("postgresql", postgreSQLOpen)
	RegisterFactory("postgresql", NewPostgreSQLDBRepository)
}

func postgreSQLOpen(dbConnCfg *DBConfig) (*DBConnection, error) {
	var (
		conn    *sql.DB
		sshConn *ssh.Client
	)
	dsn, err := genPostgresConfig(dbConnCfg)
	if err != nil {
		return nil, err
	}

	if dbConnCfg.SSHCfg != nil {
		dbConn, dbSSHConn, err := openPostgreSQLViaSSH(dsn, dbConnCfg.SSHCfg)
		if err != nil {
			return nil, err
		}
		conn = dbConn
		sshConn = dbSSHConn
	} else {
		dbConn, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		conn = dbConn
	}
	if err = conn.PingContext(context.Background()); err != nil {
		return nil, err
	}

	conn.SetMaxIdleConns(DefaultMaxIdleConns)
	conn.SetMaxOpenConns(DefaultMaxOpenConns)

	return &DBConnection{
		Conn:    conn,
		SSHConn: sshConn,
	}, nil
}

func openPostgreSQLViaSSH(dsn string, sshCfg *SSHConfig) (*sql.DB, *ssh.Client, error) {
	sshConfig, err := sshCfg.ClientConfig()
	if err != nil {
		return nil, nil, err
	}
	sshConn, err := ssh.Dial("tcp", sshCfg.Endpoint(), sshConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot ssh dial, %w", err)
	}

	conf, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, err
	}
	conf.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return sshConn.Dial(network, addr)
	}

	conn := stdlib.OpenDB(*conf)

	return conn, sshConn, nil
}

type PostgreSQLDBRepository struct {
	Conn *sql.DB
}

func NewPostgreSQLDBRepository(conn *sql.DB) DBRepository {
	return &PostgreSQLDBRepository{Conn: conn}
}

func (db *PostgreSQLDBRepository) Driver() dialect.DatabaseDriver {
	return dialect.DatabaseDriverPostgreSQL
}

func (db *PostgreSQLDBRepository) CurrentDatabase(ctx context.Context) (string, error) {
	row := db.Conn.QueryRowContext(ctx, "SELECT current_database()")
	var database string
	if err := row.Scan(&database); err != nil {
		return "", err
	}
	return database, nil
}

func (db *PostgreSQLDBRepository) Databases(ctx context.Context) ([]string, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
	SELECT datname FROM pg_database
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	databases := []string{}
	for rows.Next() {
		var database string
		if err := rows.Scan(&database); err != nil {
			return nil, err
		}
		databases = append(databases, database)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return databases, nil
}

func (db *PostgreSQLDBRepository) CurrentSchema(ctx context.Context) (string, error) {
	row := db.Conn.QueryRowContext(ctx, "SELECT current_schema()")
	var database sql.NullString
	if err := row.Scan(&database); err != nil {
		return "", err
	}
	if database.Valid {
		return database.String, nil
	}
	return "", nil
}

func (db *PostgreSQLDBRepository) Schemas(ctx context.Context) ([]string, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
	SELECT schema_name FROM information_schema.schemata
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	databases := []string{}
	for rows.Next() {
		var database string
		if err := rows.Scan(&database); err != nil {
			return nil, err
		}
		databases = append(databases, database)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return databases, nil
}

func (db *PostgreSQLDBRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
	SELECT table_schema, table_name FROM information_schema.tables
	UNION
	SELECT schemaname, matviewname FROM pg_matviews -- not in information_schema
	ORDER BY 1, 2
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	databaseTables := map[string][]string{}
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			return nil, err
		}

		if arr, ok := databaseTables[schema]; ok {
			databaseTables[schema] = append(arr, table)
		} else {
			databaseTables[schema] = []string{table}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return databaseTables, nil
}

func (db *PostgreSQLDBRepository) Tables(ctx context.Context) ([]string, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
	SELECT
	  table_name
	FROM
	  information_schema.tables
	WHERE
	  table_type = 'BASE TABLE'
	  AND table_schema NOT IN ('pg_catalog', 'information_schema')
	ORDER BY
	  table_name
	`)
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

func (db *PostgreSQLDBRepository) DescribeDatabaseTable(ctx context.Context) ([]*ColumnDesc, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
	SELECT
		c.table_schema,
		c.table_name,
		c.column_name,
		c.data_type,
		c.is_nullable,
		CASE t.constraint_type
			WHEN 'PRIMARY KEY' THEN 'YES'
			ELSE 'NO'
		END,
		c.column_default,
		''
	FROM (
		SELECT table_schema::text, table_name::text, column_name::text, data_type::text,
		       is_nullable::text, column_default::text, ordinal_position::int
		  FROM information_schema.columns
		UNION ALL -- materialized views are not in information_schema
		SELECT n.nspname::text, m.relname::text, a.attname::text, format_type(a.atttypid, a.atttypmod),
		       CASE WHEN a.attnotnull THEN 'NO' ELSE 'YES' END, NULL, a.attnum::int
		  FROM pg_attribute a
		  JOIN pg_class m ON m.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = m.relnamespace
		 WHERE m.relkind = 'm' AND a.attnum > 0 AND NOT a.attisdropped
	) c
	LEFT JOIN (
		SELECT
			ccu.table_schema as table_schema,
			ccu.table_name as table_name,
			ccu.column_name as column_name,
			tc.constraint_type as constraint_type
		FROM information_schema.constraint_column_usage ccu
		LEFT JOIN information_schema.table_constraints tc ON
			tc.table_schema = ccu.table_schema
			AND tc.table_name = ccu.table_name
			AND tc.constraint_name = ccu.constraint_name
		WHERE
			tc.constraint_type = 'PRIMARY KEY'
	) as t
		ON c.table_schema = t.table_schema
		AND c.table_name = t.table_name
		AND c.column_name = t.column_name
	ORDER BY
		c.table_name,
		c.ordinal_position
	`)
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
			&tableInfo.Default,
			&tableInfo.Extra,
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

func (db *PostgreSQLDBRepository) DescribeDatabaseTableBySchema(ctx context.Context, schemaName string) ([]*ColumnDesc, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
	SELECT
		c.table_schema,
		c.table_name,
		c.column_name,
		c.data_type,
		c.is_nullable,
		CASE t.constraint_type
			WHEN 'PRIMARY KEY' THEN 'YES'
			ELSE 'NO'
		END,
		c.column_default,
		''
	FROM (
		SELECT table_schema::text, table_name::text, column_name::text, data_type::text,
		       is_nullable::text, column_default::text, ordinal_position::int
		  FROM information_schema.columns
		UNION ALL -- materialized views are not in information_schema
		SELECT n.nspname::text, m.relname::text, a.attname::text, format_type(a.atttypid, a.atttypmod),
		       CASE WHEN a.attnotnull THEN 'NO' ELSE 'YES' END, NULL, a.attnum::int
		  FROM pg_attribute a
		  JOIN pg_class m ON m.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = m.relnamespace
		 WHERE m.relkind = 'm' AND a.attnum > 0 AND NOT a.attisdropped
	) c
	LEFT JOIN (
		SELECT
			ccu.table_schema as table_schema,
			ccu.table_name as table_name,
			ccu.column_name as column_name,
			tc.constraint_type as constraint_type
		FROM information_schema.constraint_column_usage ccu
		LEFT JOIN information_schema.table_constraints tc ON
			tc.table_schema = ccu.table_schema
			AND tc.table_name = ccu.table_name
			AND tc.constraint_name = ccu.constraint_name
		WHERE
			ccu.table_schema = $1
			AND tc.constraint_type = 'PRIMARY KEY'
	) as t
		ON c.table_schema = t.table_schema
		AND c.table_name = t.table_name
		AND c.column_name = t.column_name
	WHERE
		c.table_schema = $2
	ORDER BY
		c.table_name,
		c.ordinal_position
	`, schemaName, schemaName)
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
			&tableInfo.Default,
			&tableInfo.Extra,
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

// DescribeForeignKeysBySchema reads foreign keys between tables of the schema
// from pg_constraint (information_schema matches constraints by name, which
// PostgreSQL only makes unique per table). References to other schemas are
// left out: schemas are boundaries.
func (db *PostgreSQLDBRepository) DescribeForeignKeysBySchema(ctx context.Context, schemaName string) ([]*ForeignKey, error) {
	rows, err := db.Conn.QueryContext(
		ctx,
		`
	SELECT con.oid::text,
	       cl.relname,
	       a.attname,
	       rcl.relname,
	       ra.attname
	  FROM pg_constraint con
	  JOIN pg_class cl ON cl.oid = con.conrelid
	  JOIN pg_namespace n ON n.oid = cl.relnamespace
	  JOIN pg_class rcl ON rcl.oid = con.confrelid
	  JOIN pg_namespace rn ON rn.oid = rcl.relnamespace
	 CROSS JOIN LATERAL unnest(con.conkey, con.confkey) WITH ORDINALITY AS k(attnum, refattnum, ord)
	  JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
	  JOIN pg_attribute ra ON ra.attrelid = con.confrelid AND ra.attnum = k.refattnum
	 WHERE con.contype = 'f'
	   AND n.nspname = $1
	   AND rn.nspname = $1
	 ORDER BY cl.relname, con.conname, con.oid, k.ord
		`, schemaName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return parseForeignKeys(rows, schemaName)
}

func (db *PostgreSQLDBRepository) Exec(ctx context.Context, query string) (sql.Result, error) {
	return db.Conn.ExecContext(ctx, query)
}

func (db *PostgreSQLDBRepository) Query(ctx context.Context, query string) (*sql.Rows, error) {
	return db.Conn.QueryContext(ctx, query)
}

func genPostgresConfig(connCfg *DBConfig) (string, error) {
	if connCfg.DataSourceName != "" {
		if connCfg.DBName != "" {
			// a database switch (switchDatabase / sqlh db) on a DSN-configured
			// connection: the DSN's own dbname would otherwise win
			return pgDSNWithDB(connCfg.DataSourceName, connCfg.DBName)
		}
		return connCfg.DataSourceName, nil
	}

	q := url.Values{}
	q.Set("user", quotePostgresParam(connCfg.User))
	q.Set("password", quotePostgresParam(connCfg.Passwd))
	q.Set("dbname", quotePostgresParam(connCfg.DBName))

	switch connCfg.Proto {
	case ProtoTCP, ProtoUDP:
		host, port := connCfg.Host, connCfg.Port
		if host == "" {
			host = "127.0.0.1"
		}
		if port == 0 {
			port = 5432
		}
		q.Set("host", quotePostgresParam(host))
		q.Set("port", strconv.Itoa(port))
	case ProtoUnix:
		q.Set("host", quotePostgresParam(connCfg.Path))
	case ProtoHTTP:
	default:
		return "", fmt.Errorf("default addr for network %s unknown", connCfg.Proto)
	}

	for k, v := range connCfg.Params {
		q.Set(k, quotePostgresParam(v))
	}

	return genOptions(q, "", "=", " ", ",", true), nil
}

// quotePostgresParam single-quotes a keyword/value connection string
// value when it contains characters that would otherwise terminate or
// corrupt the value (libpq requires quoting for spaces and quotes).
func quotePostgresParam(v string) string {
	if !strings.ContainsAny(v, " '\\") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// genOptions takes URL values and generates options, joining together with
// joiner, and separated by sep, with any multi URL values joined by valSep,
// ignoring any values with keys in ignore.
//
// For example, to build a "ODBC" style connection string, use like the following:
//
//	genOptions(u.Query(), "", "=", ";", ",")
func genOptions(q url.Values, joiner, assign, sep, valSep string, skipWhenEmpty bool, ignore ...string) string {
	qlen := len(q)
	if qlen == 0 {
		return ""
	}

	// make ignore map
	ig := make(map[string]bool, len(ignore))
	for _, v := range ignore {
		ig[strings.ToLower(v)] = true
	}

	// sort keys
	s := make([]string, len(q))
	var i int
	for k := range q {
		s[i] = k
		i++
	}
	sort.Strings(s)

	var opts []string
	for _, k := range s {
		if !ig[strings.ToLower(k)] {
			val := strings.Join(q[k], valSep)
			if !skipWhenEmpty || val != "" {
				if val != "" {
					val = assign + val
				}
				opts = append(opts, k+val)
			}
		}
	}

	if len(opts) != 0 {
		return joiner + strings.Join(opts, sep)
	}

	return ""
}

// pgDSNWithDB sets the database of a URL (postgres://…/db) or key=value DSN.
func pgDSNWithDB(dsn, db string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + db
		u.RawPath = ""
		return u.String(), nil
	}
	// key=value: a repeated keyword takes the last value
	return dsn + " dbname=" + quotePostgresParam(db), nil
}

// DescribeViews lists the schema's views and materialized views, their
// definitions (pg_get_viewdef: columns qualified by table) and the relations of
// the same schema they read (pg_depend through the view's rewrite rule).
func (db *PostgreSQLDBRepository) DescribeViews(ctx context.Context, schemaName string) ([]*ViewDef, error) {
	rows, err := db.Conn.QueryContext(ctx, `
	SELECT c.relname, c.relkind = 'm', COALESCE(pg_get_viewdef(c.oid), '')
	  FROM pg_class c
	  JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE n.nspname = $1 AND c.relkind IN ('v', 'm')
	 ORDER BY 1`, schemaName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []*ViewDef
	byName := map[string]*ViewDef{}
	for rows.Next() {
		v := &ViewDef{Schema: schemaName, Uses: []string{}}
		if err := rows.Scan(&v.Name, &v.Materialized, &v.Query); err != nil {
			return nil, err
		}
		views = append(views, v)
		byName[v.Name] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return views, nil
	}
	deps, err := db.Conn.QueryContext(ctx, `
	SELECT DISTINCT v.relname, t.relname
	  FROM pg_rewrite r
	  JOIN pg_class v ON v.oid = r.ev_class
	  JOIN pg_namespace vn ON vn.oid = v.relnamespace
	  JOIN pg_depend d ON d.objid = r.oid
	   AND d.classid = 'pg_rewrite'::regclass AND d.refclassid = 'pg_class'::regclass
	  JOIN pg_class t ON t.oid = d.refobjid
	  JOIN pg_namespace tn ON tn.oid = t.relnamespace
	 WHERE vn.nspname = $1 AND tn.nspname = $1
	   AND v.relkind IN ('v', 'm') AND t.relkind IN ('r', 'p', 'v', 'm', 'f')
	   AND t.oid <> v.oid
	 ORDER BY 1, 2`, schemaName)
	if err != nil {
		return nil, err
	}
	defer deps.Close()
	for deps.Next() {
		var view, table string
		if err := deps.Scan(&view, &table); err != nil {
			return nil, err
		}
		if v, ok := byName[view]; ok {
			v.Uses = append(v.Uses, table)
		}
	}
	return views, deps.Err()
}
