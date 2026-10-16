package database

// Definitions of tables and views as CREATE statements, for go to definition
// and `sqlh ddl`. Fetched from the database when asked for (not cached):
//
//   - Oracle: DBMS_METADATA.GET_DDL when the login may use it (the owner, or
//     SELECT_CATALOG_ROLE; else ORA-31603), like DBeaver; otherwise generated
//     from the ALL_* dictionary views (columns, constraints with CHECK
//     conditions, indexes incl. expressions, comments).
//   - PostgreSQL: assembled from pg_catalog (format_type, pg_get_expr,
//     pg_get_constraintdef, pg_get_indexdef, comments): exact piece by piece.
//   - SQLite: sqlite_master.sql, as written.
//
// GenerateDefinition makes an approximate one from the schema cache when the
// database can't be asked.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/raikhan/sqlh/dialect"
)

// Definition is a table's or view's CREATE statement(s).
type Definition struct {
	Schema, Name string // the database's spelling
	Kind         RelKind
	// DDL: the statements, each ending with ";", newline-separated.
	DDL string
	// Source: where the DDL comes from ("DBMS_METADATA", "pg_catalog", …).
	Source string
	// Note: what it leaves out (e.g. partitioning of a generated table).
	Note string
	// View: a view's definition (updatability included when known).
	View *ViewDef
	// MetadataFailed: Oracle DBMS_METADATA was refused; don't try it again on
	// this connection.
	MetadataFailed bool
}

// oracleMetadataTimeout: how long DBMS_METADATA may take before the DDL is
// generated from the dictionary instead.
const oracleMetadataTimeout = 2 * time.Second

// ErrNoObject: no table or view of that name that the login can see.
var ErrNoObject = errors.New("no such table or view")

// DefinitionOptions tune a fetch.
type DefinitionOptions struct {
	// SkipMetadata: Oracle, don't try DBMS_METADATA (it failed before).
	SkipMetadata bool
}

// DefinitionFetcher is implemented by repositories that can produce a
// table's or view's CREATE statement.
type DefinitionFetcher interface {
	Definition(ctx context.Context, schema, name string, opt DefinitionOptions) (*Definition, error)
}

// ddlWriter accumulates statements.
type ddlWriter struct {
	b strings.Builder
}

func (w *ddlWriter) stmt(s string) {
	if w.b.Len() > 0 {
		w.b.WriteString("\n")
	}
	s = strings.TrimRight(strings.TrimSpace(s), ";")
	w.b.WriteString(s + ";\n")
}

func (w *ddlWriter) String() string { return w.b.String() }

// createTable renders CREATE TABLE name (\n  item,\n  …\n) suffix.
func createTable(name string, items []string, suffix string) string {
	return "CREATE TABLE " + name + " (\n  " + strings.Join(items, ",\n  ") + "\n)" + suffix
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ---- Oracle -----------------------------------------------------------------

// Definition implements DefinitionFetcher.
func (db *OracleDBRepository) Definition(ctx context.Context, schema, name string, opt DefinitionOptions) (*Definition, error) {
	kinds := map[string]bool{}
	rows, err := db.Conn.QueryContext(ctx, `
SELECT OBJECT_TYPE FROM ALL_OBJECTS
 WHERE OWNER = :1 AND OBJECT_NAME = :2 AND OBJECT_TYPE IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW')`, schema, name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		kinds[k] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d := &Definition{Schema: schema, Name: name}
	switch {
	case kinds["MATERIALIZED VIEW"], kinds["VIEW"]:
		mat := kinds["MATERIALIZED VIEW"]
		q := `SELECT TEXT FROM ALL_VIEWS WHERE OWNER = :1 AND VIEW_NAME = :2`
		d.Kind, d.Source = RelView, "ALL_VIEWS"
		if mat {
			q = `SELECT QUERY FROM ALL_MVIEWS WHERE OWNER = :1 AND MVIEW_NAME = :2`
			d.Kind, d.Source = RelMatView, "ALL_MVIEWS"
		}
		var text sql.NullString
		if err := db.Conn.QueryRowContext(ctx, q, schema, name).Scan(&text); err != nil {
			return nil, err
		}
		v := &ViewDef{Schema: schema, Name: name, Materialized: mat, Query: text.String}
		if err := db.viewWrites(ctx, schema, map[string]*ViewDef{name: v}); err != nil {
			v.Writes = nil
		}
		d.View, d.DDL = v, v.CreateStatement()
		return d, nil
	case kinds["TABLE"]:
		d.Kind = RelTable
	default:
		return nil, ErrNoObject
	}
	if !opt.SkipMetadata {
		// the first call on an instance can take 15 s (then ~0.4 s): don't
		// wait for it, the dictionary is quicker
		mctx, cancel := context.WithTimeout(ctx, oracleMetadataTimeout)
		ddl, err := db.metadataDDL(mctx, schema, name)
		slow := errors.Is(mctx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			d.DDL, d.Source = ddl, "DBMS_METADATA"
			return d, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// ORA-31603 without SELECT_CATALOG_ROLE: generate it, like DBeaver
		d.MetadataFailed = !slow
	}
	ddl, note, err := db.generateTableDDL(ctx, schema, name)
	if err != nil {
		return nil, err
	}
	d.DDL, d.Note, d.Source = ddl, note, "generated from the data dictionary (ALL_* views)"
	return d, nil
}

// metadataDDL: the table, its indexes and comments from DBMS_METADATA, on a
// session of its own (the transform parameters are per session; reset after).
func (db *OracleDBRepository) metadataDDL(ctx context.Context, schema, name string) (string, error) {
	conn, err := db.Conn.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `
BEGIN
  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'SQLTERMINATOR', TRUE);
  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'PRETTY', TRUE);
  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'SEGMENT_ATTRIBUTES', FALSE);
  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'STORAGE', FALSE);
END;`); err != nil {
		return "", err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(rctx,
			`BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'DEFAULT'); END;`)
	}()
	var w ddlWriter
	var main string
	if err := conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE', :1, :2) FROM DUAL`, name, schema).Scan(&main); err != nil {
		return "", err
	}
	w.b.WriteString(strings.TrimSpace(main) + "\n")
	for _, dep := range []string{"INDEX", "COMMENT"} {
		var s sql.NullString
		// ORA-31608 when there is none
		if err := conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DEPENDENT_DDL('`+dep+`', :1, :2) FROM DUAL`, name, schema).Scan(&s); err == nil && strings.TrimSpace(s.String) != "" {
			w.b.WriteString("\n" + strings.TrimSpace(s.String) + "\n")
		}
	}
	return w.String(), nil
}

var oraNotNullRe = regexp.MustCompile(`(?i)^\s*"?([^"\s]+)"?\s+IS\s+NOT\s+NULL\s*$`)

// generateTableDDL builds the CREATE statements from the dictionary views.
func (db *OracleDBRepository) generateTableDDL(ctx context.Context, schema, name string) (string, string, error) {
	q := func(s string) string { return QuoteIdent(s, dialect.DatabaseDriverOracle, false) }
	full := q(schema) + "." + q(name)

	// columns
	rows, err := db.Conn.QueryContext(ctx, `
SELECT COLUMN_NAME, DATA_TYPE, DATA_LENGTH, CHAR_LENGTH, CHAR_USED, DATA_PRECISION, DATA_SCALE,
       NULLABLE, DATA_DEFAULT
  FROM ALL_TAB_COLUMNS
 WHERE OWNER = :1 AND TABLE_NAME = :2
 ORDER BY COLUMN_ID`, schema, name)
	if err != nil {
		return "", "", err
	}
	var items []string
	for rows.Next() {
		var col, typ, charUsed, nullable string
		var length, charLength sql.NullInt64
		var prec, scale sql.NullInt64
		var def sql.NullString
		var cu sql.NullString
		if err := rows.Scan(&col, &typ, &length, &charLength, &cu, &prec, &scale, &nullable, &def); err != nil {
			rows.Close()
			return "", "", err
		}
		charUsed = cu.String
		item := q(col) + " " + oracleType(typ, length.Int64, charLength.Int64, charUsed, prec, scale)
		if d := strings.TrimSpace(def.String); d != "" {
			item += " DEFAULT " + d
		}
		if nullable == "N" {
			item += " NOT NULL"
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if len(items) == 0 {
		return "", "", ErrNoObject
	}

	// constraint columns, by constraint
	consCols := map[string][]string{}
	rows, err = db.Conn.QueryContext(ctx, `
SELECT CONSTRAINT_NAME, COLUMN_NAME FROM ALL_CONS_COLUMNS
 WHERE OWNER = :1 AND TABLE_NAME = :2 ORDER BY CONSTRAINT_NAME, POSITION`, schema, name)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var c, col string
		if err := rows.Scan(&c, &col); err != nil {
			rows.Close()
			return "", "", err
		}
		consCols[c] = append(consCols[c], q(col))
	}
	rows.Close()

	type cons struct {
		name, typ, cond, rOwner, rName, deleteRule, generated, status, index string
	}
	var list []cons
	rows, err = db.Conn.QueryContext(ctx, `
SELECT CONSTRAINT_NAME, CONSTRAINT_TYPE, SEARCH_CONDITION, R_OWNER, R_CONSTRAINT_NAME, DELETE_RULE,
       GENERATED, STATUS, INDEX_NAME
  FROM ALL_CONSTRAINTS
 WHERE OWNER = :1 AND TABLE_NAME = :2 AND CONSTRAINT_TYPE IN ('P', 'U', 'R', 'C')`, schema, name)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var c cons
		var cond, rOwner, rName, del, gen, status, index sql.NullString
		if err := rows.Scan(&c.name, &c.typ, &cond, &rOwner, &rName, &del, &gen, &status, &index); err != nil {
			rows.Close()
			return "", "", err
		}
		c.cond, c.rOwner, c.rName, c.deleteRule = cond.String, rOwner.String, rName.String, del.String
		c.generated, c.status, c.index = gen.String, status.String, index.String
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	order := map[string]int{"P": 0, "U": 1, "R": 2, "C": 3}
	sort.SliceStable(list, func(i, j int) bool {
		if order[list[i].typ] != order[list[j].typ] {
			return order[list[i].typ] < order[list[j].typ]
		}
		return list[i].name < list[j].name
	})
	constraintIndexes := map[string]bool{}
	for _, c := range list {
		named := "CONSTRAINT " + q(c.name) + " "
		if c.generated == "GENERATED NAME" {
			named = ""
		}
		var item string
		cols := strings.Join(consCols[c.name], ", ")
		switch c.typ {
		case "P":
			item = named + "PRIMARY KEY (" + cols + ")"
			constraintIndexes[c.index] = true
		case "U":
			item = named + "UNIQUE (" + cols + ")"
			constraintIndexes[c.index] = true
		case "R":
			var rTable string
			var rCols []string
			r, err := db.Conn.QueryContext(ctx, `
SELECT TABLE_NAME, COLUMN_NAME FROM ALL_CONS_COLUMNS
 WHERE OWNER = :1 AND CONSTRAINT_NAME = :2 ORDER BY POSITION`, c.rOwner, c.rName)
			if err != nil {
				return "", "", err
			}
			for r.Next() {
				var t, col string
				if err := r.Scan(&t, &col); err != nil {
					r.Close()
					return "", "", err
				}
				rTable = t
				rCols = append(rCols, q(col))
			}
			r.Close()
			if rTable == "" {
				continue // referenced key not visible to this login
			}
			ref := q(rTable)
			if c.rOwner != schema {
				ref = q(c.rOwner) + "." + ref
			}
			item = named + "FOREIGN KEY (" + cols + ") REFERENCES " + ref + " (" + strings.Join(rCols, ", ") + ")"
			if c.deleteRule != "" && c.deleteRule != "NO ACTION" {
				item += " ON DELETE " + c.deleteRule
			}
		case "C":
			if c.generated == "GENERATED NAME" && oraNotNullRe.MatchString(c.cond) {
				continue // shown as NOT NULL on the column
			}
			item = named + "CHECK (" + strings.TrimSpace(c.cond) + ")"
		}
		if c.status == "DISABLED" {
			item += " DISABLE"
		}
		items = append(items, item)
	}

	var w ddlWriter
	w.stmt(createTable(full, items, ""))

	// indexes not backing a constraint
	type index struct {
		name, uniqueness, typ string
		cols                  []string
	}
	var idx []*index
	byName := map[string]*index{}
	rows, err = db.Conn.QueryContext(ctx, `
SELECT INDEX_NAME, UNIQUENESS, INDEX_TYPE FROM ALL_INDEXES
 WHERE TABLE_OWNER = :1 AND TABLE_NAME = :2 AND INDEX_TYPE NOT IN ('LOB', 'IOT - TOP')
 ORDER BY INDEX_NAME`, schema, name)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		x := &index{}
		if err := rows.Scan(&x.name, &x.uniqueness, &x.typ); err != nil {
			rows.Close()
			return "", "", err
		}
		if !constraintIndexes[x.name] {
			idx = append(idx, x)
			byName[x.name] = x
		}
	}
	rows.Close()
	if len(idx) > 0 {
		exprs := map[string]string{} // index \x00 position → expression
		rows, err = db.Conn.QueryContext(ctx, `
SELECT INDEX_NAME, COLUMN_POSITION, COLUMN_EXPRESSION FROM ALL_IND_EXPRESSIONS
 WHERE TABLE_OWNER = :1 AND TABLE_NAME = :2`, schema, name)
		if err == nil {
			for rows.Next() {
				var n string
				var pos int
				var e sql.NullString
				if rows.Scan(&n, &pos, &e) == nil {
					exprs[n+"\x00"+strconv.Itoa(pos)] = e.String
				}
			}
			rows.Close()
		}
		rows, err = db.Conn.QueryContext(ctx, `
SELECT INDEX_NAME, COLUMN_NAME, COLUMN_POSITION, DESCEND FROM ALL_IND_COLUMNS
 WHERE TABLE_OWNER = :1 AND TABLE_NAME = :2 ORDER BY INDEX_NAME, COLUMN_POSITION`, schema, name)
		if err != nil {
			return "", "", err
		}
		for rows.Next() {
			var n, col, desc string
			var pos int
			if err := rows.Scan(&n, &col, &pos, &desc); err != nil {
				rows.Close()
				return "", "", err
			}
			x := byName[n]
			if x == nil {
				continue
			}
			c := q(col)
			if e, ok := exprs[n+"\x00"+strconv.Itoa(pos)]; ok {
				c = strings.TrimSpace(e)
			}
			if desc == "DESC" && !strings.HasSuffix(strings.ToUpper(c), " DESC") {
				c += " DESC"
			}
			x.cols = append(x.cols, c)
		}
		rows.Close()
		for _, x := range idx {
			kind := "INDEX"
			switch {
			case x.uniqueness == "UNIQUE":
				kind = "UNIQUE INDEX"
			case strings.HasPrefix(x.typ, "BITMAP"):
				kind = "BITMAP INDEX"
			}
			w.stmt("CREATE " + kind + " " + q(schema) + "." + q(x.name) + " ON " + full + " (" + strings.Join(x.cols, ", ") + ")")
		}
	}

	// comments
	var tc sql.NullString
	if err := db.Conn.QueryRowContext(ctx, `SELECT COMMENTS FROM ALL_TAB_COMMENTS WHERE OWNER = :1 AND TABLE_NAME = :2`, schema, name).Scan(&tc); err == nil && tc.String != "" {
		w.stmt("COMMENT ON TABLE " + full + " IS " + sqlString(tc.String))
	}
	rows, err = db.Conn.QueryContext(ctx, `
SELECT c.COLUMN_NAME, c.COMMENTS FROM ALL_COL_COMMENTS c
  JOIN ALL_TAB_COLUMNS t ON t.OWNER = c.OWNER AND t.TABLE_NAME = c.TABLE_NAME AND t.COLUMN_NAME = c.COLUMN_NAME
 WHERE c.OWNER = :1 AND c.TABLE_NAME = :2 AND c.COMMENTS IS NOT NULL
 ORDER BY t.COLUMN_ID`, schema, name)
	if err == nil {
		for rows.Next() {
			var col, cm string
			if rows.Scan(&col, &cm) == nil {
				w.stmt("COMMENT ON COLUMN " + full + "." + q(col) + " IS " + sqlString(cm))
			}
		}
		rows.Close()
	}

	var note string
	var partitioned string
	if err := db.Conn.QueryRowContext(ctx, `SELECT PARTITIONED FROM ALL_TABLES WHERE OWNER = :1 AND TABLE_NAME = :2`, schema, name).Scan(&partitioned); err == nil && partitioned == "YES" {
		note = "partitioning not shown"
	}
	return w.String(), note, nil
}

// oracleType renders a column type as declared.
func oracleType(typ string, length, charLength int64, charUsed string, prec, scale sql.NullInt64) string {
	switch typ {
	case "VARCHAR2", "CHAR":
		if charUsed == "C" {
			return fmt.Sprintf("%s(%d CHAR)", typ, charLength)
		}
		return fmt.Sprintf("%s(%d)", typ, length)
	case "NVARCHAR2", "NCHAR":
		return fmt.Sprintf("%s(%d)", typ, charLength)
	case "RAW":
		return fmt.Sprintf("RAW(%d)", length)
	case "NUMBER":
		switch {
		case !prec.Valid && !scale.Valid:
			return "NUMBER"
		case !prec.Valid:
			return fmt.Sprintf("NUMBER(*,%d)", scale.Int64)
		case !scale.Valid || scale.Int64 == 0:
			return fmt.Sprintf("NUMBER(%d)", prec.Int64)
		}
		return fmt.Sprintf("NUMBER(%d,%d)", prec.Int64, scale.Int64)
	case "FLOAT":
		if prec.Valid {
			return fmt.Sprintf("FLOAT(%d)", prec.Int64)
		}
	}
	return typ
}

// ---- PostgreSQL -------------------------------------------------------------

// Definition implements DefinitionFetcher.
func (db *PostgreSQLDBRepository) Definition(ctx context.Context, schema, name string, _ DefinitionOptions) (*Definition, error) {
	var oid int64
	var kind string
	err := db.Conn.QueryRowContext(ctx, `
SELECT c.oid, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`, schema, name).Scan(&oid, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoObject
	}
	if err != nil {
		return nil, err
	}
	d := &Definition{Schema: schema, Name: name, Source: "pg_catalog", Kind: RelTable}
	q := func(s string) string { return QuoteIdent(s, dialect.DatabaseDriverPostgreSQL, false) }
	full := q(schema) + "." + q(name)
	var w ddlWriter

	if kind == "v" || kind == "m" {
		v := &ViewDef{Schema: schema, Name: name, Materialized: kind == "m"}
		var events int
		if err := db.Conn.QueryRowContext(ctx, `SELECT COALESCE(pg_get_viewdef($1::oid), ''), pg_relation_is_updatable($1::oid::regclass, true)`, oid).Scan(&v.Query, &events); err != nil {
			return nil, err
		}
		v.Writes = &ViewWrites{Insert: events&8 != 0, Update: events&4 != 0, Delete: events&16 != 0}
		d.Kind, d.View = RelView, v
		if v.Materialized {
			d.Kind = RelMatView
		}
		w.b.WriteString(v.CreateStatement())
		if err := db.pgIndexesAndComments(ctx, oid, full, &w, false); err != nil {
			return nil, err
		}
		d.DDL = w.String()
		return d, nil
	}

	rows, err := db.Conn.QueryContext(ctx, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       COALESCE(pg_get_expr(ad.adbin, ad.adrelid), ''), a.attidentity::text, a.attgenerated::text,
       COALESCE(CASE WHEN a.attcollation <> t.typcollation THEN co.collname END, '')
  FROM pg_attribute a
  JOIN pg_type t ON t.oid = a.atttypid
  LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
  LEFT JOIN pg_collation co ON co.oid = a.attcollation
 WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
 ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, err
	}
	var items []string
	for rows.Next() {
		var col, typ, def, identity, generated, coll string
		var notNull bool
		if err := rows.Scan(&col, &typ, &notNull, &def, &identity, &generated, &coll); err != nil {
			rows.Close()
			return nil, err
		}
		item := q(col) + " " + typ
		if coll != "" {
			item += " COLLATE " + q(coll)
		}
		switch {
		case generated == "s":
			item += " GENERATED ALWAYS AS (" + def + ") STORED"
		case identity == "a":
			item += " GENERATED ALWAYS AS IDENTITY"
		case identity == "d":
			item += " GENERATED BY DEFAULT AS IDENTITY"
		case def != "":
			item += " DEFAULT " + def
		}
		if notNull && identity == "" {
			item += " NOT NULL"
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Conn.QueryContext(ctx, `
SELECT conname, pg_get_constraintdef(oid, true)
  FROM pg_constraint
 WHERE conrelid = $1 AND contype IN ('p', 'u', 'f', 'c', 'x')
 ORDER BY CASE contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 WHEN 'f' THEN 2 WHEN 'c' THEN 3 ELSE 4 END, conname`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cn, def string
		if err := rows.Scan(&cn, &def); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, "CONSTRAINT "+q(cn)+" "+def)
	}
	rows.Close()

	suffix := ""
	if kind == "p" {
		var key string
		if db.Conn.QueryRowContext(ctx, `SELECT pg_get_partkeydef($1::oid)`, oid).Scan(&key) == nil && key != "" {
			suffix = " PARTITION BY " + key
		}
	}
	if kind == "f" {
		d.Note = "a foreign table: server and options not shown"
	}
	w.stmt(createTable(full, items, suffix))
	if err := db.pgIndexesAndComments(ctx, oid, full, &w, true); err != nil {
		return nil, err
	}
	d.DDL = w.String()
	return d, nil
}

// pgIndexesAndComments adds the indexes not backing a constraint and the
// comments of a relation (columns with withColumns).
func (db *PostgreSQLDBRepository) pgIndexesAndComments(ctx context.Context, oid int64, full string, w *ddlWriter, withColumns bool) error {
	rows, err := db.Conn.QueryContext(ctx, `
SELECT pg_get_indexdef(i.indexrelid)
  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
 WHERE i.indrelid = $1
   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid AND k.conrelid = i.indrelid)
 ORDER BY c.relname`, oid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			rows.Close()
			return err
		}
		w.stmt(def)
	}
	rows.Close()
	var tc sql.NullString
	if db.Conn.QueryRowContext(ctx, `SELECT obj_description($1::oid, 'pg_class')`, oid).Scan(&tc) == nil && tc.String != "" {
		w.stmt("COMMENT ON TABLE " + full + " IS " + sqlString(tc.String))
	}
	if !withColumns {
		return nil
	}
	rows, err = db.Conn.QueryContext(ctx, `
SELECT a.attname, col_description(a.attrelid, a.attnum)
  FROM pg_attribute a
 WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped AND col_description(a.attrelid, a.attnum) IS NOT NULL
 ORDER BY a.attnum`, oid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var col, cm string
		if err := rows.Scan(&col, &cm); err != nil {
			return err
		}
		w.stmt("COMMENT ON COLUMN " + full + "." + QuoteIdent(col, dialect.DatabaseDriverPostgreSQL, false) + " IS " + sqlString(cm))
	}
	return rows.Err()
}

// ---- SQLite -----------------------------------------------------------------

// Definition implements DefinitionFetcher.
func (db *SQLite3DBRepository) Definition(ctx context.Context, schema, name string, _ DefinitionOptions) (*Definition, error) {
	var typ, realName string
	var ddl sql.NullString
	err := db.Conn.QueryRowContext(ctx, `SELECT type, name, sql FROM sqlite_master WHERE name = ? COLLATE NOCASE AND type IN ('table', 'view')`, name).Scan(&typ, &realName, &ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoObject
	}
	if err != nil {
		return nil, err
	}
	d := &Definition{Schema: schema, Name: realName, Kind: RelTable, Source: "sqlite_master"}
	var w ddlWriter
	w.stmt(ddl.String)
	if typ == "view" {
		d.Kind = RelView
		d.View = &ViewDef{Schema: schema, Name: realName, DDL: ddl.String}
	}
	rows, err := db.Conn.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE type IN ('index', 'trigger') AND tbl_name = ? AND sql IS NOT NULL ORDER BY type, name`, realName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "CREATE TRIGGER") && typ != "view" {
			continue // triggers of tables left out; a view's INSTEAD OF triggers explain its writes
		}
		w.stmt(s)
	}
	d.DDL = w.String()
	return d, rows.Err()
}

// ---- from the schema cache --------------------------------------------------

// GenerateDefinition makes an approximate CREATE TABLE from the schema cache:
// columns (types, defaults, NOT NULL), the primary key, foreign keys of the
// current schema and the indexes the background load found.
func GenerateDefinition(dc *DBCache, driver dialect.DatabaseDriver, schema, name string) (*Definition, bool) {
	if dc == nil {
		return nil, false
	}
	if schema == "" {
		schema = dc.DefaultSchema()
	}
	cols, ok := dc.ColumnDatabase(schema, name)
	if !ok || len(cols) == 0 {
		return nil, false
	}
	q := func(s string) string { return QuoteIdent(s, driver, false) }
	full := q(name)
	if schema != "" && driver != dialect.DatabaseDriverSQLite3 {
		full = q(schema) + "." + q(name)
	}
	d := &Definition{Schema: schema, Name: cols[0].Table, Kind: RelTable,
		Source: "generated from the schema cache", Note: "approximate: no CHECK constraints, index expressions or comments"}
	if v := dc.Relations.View(name); v != nil && strings.EqualFold(schema, dc.DefaultSchema()) {
		d.Kind, d.View, d.DDL, d.Source, d.Note = dc.Relations.Kind(name), v, v.CreateStatement(), "the schema cache", ""
		return d, true
	}
	var items, pk []string
	for _, c := range cols {
		item := q(c.Name) + " " + c.Type
		if c.Default.Valid && strings.TrimSpace(c.Default.String) != "" {
			item += " DEFAULT " + strings.TrimSpace(c.Default.String)
		}
		if n := strings.ToUpper(c.Null); n == "NO" || n == "N" {
			item += " NOT NULL"
		}
		items = append(items, item)
		if IsPrimaryKey(c.Key) {
			pk = append(pk, q(c.Name))
		}
	}
	if len(pk) > 0 {
		items = append(items, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}
	if strings.EqualFold(schema, dc.DefaultSchema()) {
		for _, fk := range dc.foreignKeyList {
			if fk == nil || len(*fk) == 0 || !strings.EqualFold((*fk)[0][0].Table, name) {
				continue
			}
			var from, to []string
			for _, p := range *fk {
				from = append(from, q(p[0].Name))
				to = append(to, q(p[1].Name))
			}
			items = append(items, "FOREIGN KEY ("+strings.Join(from, ", ")+") REFERENCES "+q((*fk)[0][1].Table)+" ("+strings.Join(to, ", ")+")")
		}
	}
	var w ddlWriter
	w.stmt(createTable(full, items, ""))
	if dc.Objects != nil {
		for _, x := range dc.Objects.OfTable(ObjectIndex, schema, name) {
			if x.Detail == "primary key" || len(x.Columns) == 0 {
				continue
			}
			kind := "INDEX"
			if x.Detail == "unique" {
				kind = "UNIQUE INDEX"
			}
			qc := make([]string, len(x.Columns))
			for i, c := range x.Columns {
				qc[i] = q(c)
			}
			w.stmt("CREATE " + kind + " " + q(x.Name) + " ON " + full + " (" + strings.Join(qc, ", ") + ")")
		}
	}
	d.DDL = w.String()
	return d, true
}

// SynonymResolver resolves an unqualified name through a synonym, as the
// database would: the current schema's, then PUBLIC.
type SynonymResolver interface {
	ResolveSynonym(ctx context.Context, name string) (owner, table string, err error)
}

// ResolveSynonym implements SynonymResolver; "" when there is none.
func (db *OracleDBRepository) ResolveSynonym(ctx context.Context, name string) (string, string, error) {
	rows, err := db.Conn.QueryContext(ctx, `
SELECT TABLE_OWNER, TABLE_NAME FROM ALL_SYNONYMS
 WHERE SYNONYM_NAME = :1 AND OWNER IN (SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'), 'PUBLIC')
   AND DB_LINK IS NULL
 ORDER BY CASE OWNER WHEN 'PUBLIC' THEN 1 ELSE 0 END`, strings.ToUpper(name))
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	if rows.Next() {
		var owner, table string
		err := rows.Scan(&owner, &table)
		return owner, table, err
	}
	return "", "", rows.Err()
}

// SplitQualified splits name or schema.name as typed: quoted parts as
// written, unquoted ones as the database folds them (Oracle upper case,
// PostgreSQL lower case; others as typed).
func SplitQualified(s string, driver dialect.DatabaseDriver) (schema, name string) {
	var parts []string
	var b strings.Builder
	quoted := false
	fold := func(p string, wasQuoted bool) string {
		if wasQuoted {
			return p
		}
		switch driver {
		case dialect.DatabaseDriverOracle:
			return strings.ToUpper(p)
		case dialect.DatabaseDriverPostgreSQL:
			return strings.ToLower(p)
		}
		return p
	}
	partQuoted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && quoted && i+1 < len(s) && s[i+1] == '"':
			b.WriteByte('"')
			i++
		case c == '"':
			quoted = !quoted
			partQuoted = true
		case c == '.' && !quoted:
			parts = append(parts, fold(b.String(), partQuoted))
			b.Reset()
			partQuoted = false
		default:
			b.WriteByte(c)
		}
	}
	parts = append(parts, fold(strings.TrimSpace(b.String()), partQuoted))
	if len(parts) == 1 {
		return "", parts[0]
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}
