package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Object kinds of SchemaObjects.
const (
	ObjectIndex      = "index"
	ObjectConstraint = "constraint"
	ObjectSequence   = "sequence"
	ObjectTrigger    = "trigger"
	ObjectType       = "type"
)

// SchemaObject is an index, constraint, sequence, trigger or user-defined
// type.
type SchemaObject struct {
	Kind   string // Object* constant
	Schema string
	Name   string
	// Table: the table of an index, constraint or trigger.
	Table string
	// Detail: the index's or constraint's kind ("unique", "primary key",
	// "foreign key", …), a type's kind ("enum", "domain", …).
	Detail string
	// Columns of an index or constraint.
	Columns []string
	// RefTable: the table a foreign key references.
	RefTable string
	// Generated: the database named it (Oracle SYS_C…, PostgreSQL defaults
	// are not distinguished).
	Generated bool
}

// SchemaObjects are the objects a cache knows besides tables, views and
// functions. Names are matched case-insensitively.
type SchemaObjects struct {
	list []*SchemaObject
	// partial: kind \t SCHEMA whose list may miss objects the user can't see
	// (Oracle ALL_TRIGGERS/ALL_SEQUENCES of another owner)
	partial map[string]bool
	// byKey: kind \t SCHEMA \t NAME → objects (a constraint name may repeat
	// across tables in PostgreSQL)
	byKey map[string][]*SchemaObject
}

// ObjectLister is implemented by repositories that can list SchemaObjects.
type ObjectLister interface {
	SchemaObjects(ctx context.Context) (*SchemaObjects, error)
}

// NewSchemaObjects indexes a list.
func NewSchemaObjects(list []*SchemaObject) *SchemaObjects {
	o := &SchemaObjects{list: list, byKey: map[string][]*SchemaObject{}, partial: map[string]bool{}}
	for _, x := range list {
		k := objectKey(x.Kind, x.Schema, x.Name)
		o.byKey[k] = append(o.byKey[k], x)
	}
	sort.SliceStable(o.list, func(i, j int) bool { return o.list[i].Name < o.list[j].Name })
	return o
}

func objectKey(kind, schema, name string) string {
	return kind + "\t" + strings.ToUpper(schema) + "\t" + strings.ToUpper(name)
}

// MarkPartial notes that the list of a kind in a schema may be incomplete.
func (o *SchemaObjects) MarkPartial(kind, schema string) *SchemaObjects {
	o.partial[kind+"\t"+strings.ToUpper(schema)] = true
	return o
}

// Partial reports whether the list of a kind in a schema may be incomplete.
func (o *SchemaObjects) Partial(kind, schema string) bool {
	return o != nil && o.partial[kind+"\t"+strings.ToUpper(schema)]
}

// Partials lists the incomplete kinds as "kind\tSCHEMA".
func (o *SchemaObjects) Partials() []string {
	if o == nil {
		return nil
	}
	var out []string
	for k := range o.partial {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// List returns every object.
func (o *SchemaObjects) List() []*SchemaObject {
	if o == nil {
		return nil
	}
	return o.list
}

// All lists the objects of a kind in a schema.
func (o *SchemaObjects) All(kind, schema string) []*SchemaObject {
	if o == nil {
		return nil
	}
	var out []*SchemaObject
	for _, x := range o.list {
		if x.Kind == kind && strings.EqualFold(x.Schema, schema) {
			out = append(out, x)
		}
	}
	return out
}

// Find returns the objects of a kind with that name in a schema.
func (o *SchemaObjects) Find(kind, schema, name string) []*SchemaObject {
	if o == nil {
		return nil
	}
	return o.byKey[objectKey(kind, schema, name)]
}

// FindAnySchema returns the objects of a kind with that name in any schema.
func (o *SchemaObjects) FindAnySchema(kind, name string) []*SchemaObject {
	if o == nil {
		return nil
	}
	var out []*SchemaObject
	for _, x := range o.list {
		if x.Kind == kind && strings.EqualFold(x.Name, name) {
			out = append(out, x)
		}
	}
	return out
}

// OfTable lists the objects of a kind belonging to a table.
func (o *SchemaObjects) OfTable(kind, schema, table string) []*SchemaObject {
	if o == nil {
		return nil
	}
	var out []*SchemaObject
	for _, x := range o.list {
		if x.Kind == kind && strings.EqualFold(x.Schema, schema) && strings.EqualFold(x.Table, table) {
			out = append(out, x)
		}
	}
	return out
}

// Doc is a short markdown description of an object.
func (x *SchemaObject) Doc() string {
	var b strings.Builder
	what := x.Kind
	if x.Detail != "" {
		what = x.Detail + " " + x.Kind
		if x.Kind == ObjectConstraint || x.Kind == ObjectType {
			what = x.Detail
			if x.Kind == ObjectType {
				what += " type"
			} else {
				what += " constraint"
			}
		}
	}
	fmt.Fprintf(&b, "`%s` %s", x.Name, what)
	if x.Table != "" {
		fmt.Fprintf(&b, " on `%s`", x.Table)
	}
	if len(x.Columns) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(x.Columns, ", "))
	}
	if x.RefTable != "" {
		fmt.Fprintf(&b, " → `%s`", x.RefTable)
	}
	return b.String()
}

func splitList(s string) []string {
	if s == "" || s == "-" {
		return nil
	}
	return strings.Split(s, ",")
}

func scanObjects(rows *sql.Rows, err error, scan func(*sql.Rows) (*SchemaObject, error)) ([]*SchemaObject, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SchemaObject
	for rows.Next() {
		x, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if x != nil {
			out = append(out, x)
		}
	}
	return out, rows.Err()
}

// ---- PostgreSQL ---------------------------------------------------------------

const pgSchemaFilter = `n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'`

var pgObjectQueries = []struct {
	kind  string
	query string
}{
	{ObjectIndex, `
SELECT n.nspname, ci.relname, ct.relname,
       CASE WHEN ix.indisprimary THEN 'primary key' WHEN ix.indisunique THEN 'unique' ELSE '' END,
       coalesce(array_to_string(ARRAY(
         SELECT a.attname FROM unnest(ix.indkey) WITH ORDINALITY k(attnum, ord)
           JOIN pg_attribute a ON a.attrelid = ix.indrelid AND a.attnum = k.attnum
          ORDER BY k.ord), ','), ''),
       ''
  FROM pg_index ix
  JOIN pg_class ci ON ci.oid = ix.indexrelid
  JOIN pg_class ct ON ct.oid = ix.indrelid
  JOIN pg_namespace n ON n.oid = ci.relnamespace
 WHERE ` + pgSchemaFilter},
	{ObjectConstraint, `
SELECT n.nspname, c.conname, t.relname,
       CASE c.contype WHEN 'p' THEN 'primary key' WHEN 'f' THEN 'foreign key' WHEN 'u' THEN 'unique'
                      WHEN 'c' THEN 'check' WHEN 'x' THEN 'exclusion' WHEN 'n' THEN 'not null'
                      ELSE 'trigger' END,
       coalesce(array_to_string(ARRAY(
         SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(attnum, ord)
           JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
          ORDER BY k.ord), ','), ''),
       coalesce(rt.relname, '')
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
  LEFT JOIN pg_class rt ON rt.oid = c.confrelid
 WHERE c.conrelid <> 0 AND ` + pgSchemaFilter},
	{ObjectSequence, `
SELECT n.nspname, c.relname, '', '', '', ''
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind = 'S' AND ` + pgSchemaFilter},
	{ObjectTrigger, `
SELECT n.nspname, tg.tgname, t.relname, '', '', ''
  FROM pg_trigger tg
  JOIN pg_class t ON t.oid = tg.tgrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
 WHERE NOT tg.tgisinternal AND ` + pgSchemaFilter},
	{ObjectType, `
SELECT n.nspname, t.typname, '',
       CASE t.typtype WHEN 'e' THEN 'enum' WHEN 'd' THEN 'domain' WHEN 'c' THEN 'composite'
                      ELSE 'range' END,
       '', ''
  FROM pg_type t
  JOIN pg_namespace n ON n.oid = t.typnamespace
 WHERE t.typtype IN ('e', 'd', 'c', 'r')
   AND (t.typtype <> 'c' OR EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = t.typrelid AND c.relkind = 'c'))
   AND ` + pgSchemaFilter},
}

// SchemaObjects implements ObjectLister.
func (db *PostgreSQLDBRepository) SchemaObjects(ctx context.Context) (*SchemaObjects, error) {
	var out []*SchemaObject
	for _, q := range pgObjectQueries {
		kind := q.kind
		rows, err := db.Conn.QueryContext(ctx, q.query)
		list, err := scanObjects(rows, err, func(r *sql.Rows) (*SchemaObject, error) {
			x := &SchemaObject{Kind: kind}
			var cols string
			if err := r.Scan(&x.Schema, &x.Name, &x.Table, &x.Detail, &cols, &x.RefTable); err != nil {
				return nil, err
			}
			x.Columns = splitList(cols)
			// PostgreSQL 18 names NOT NULL constraints (t_col_not_null)
			x.Generated = x.Detail == "not null"
			return x, nil
		})
		if err != nil {
			return nil, fmt.Errorf("%ss: %w", kind, err)
		}
		out = append(out, list...)
	}
	return NewSchemaObjects(out), nil
}

// ---- Oracle -------------------------------------------------------------------

// SchemaObjects implements ObjectLister (allowed schemas only). Flat
// queries joined here: correlated subqueries on the dictionary views took a
// minute on CADE. System-named CHECK constraints (mostly NOT NULL, ~11k on
// CADE) are left out: telling NOT NULL apart needs SEARCH_CONDITION_VC,
// which took 4.5 s.
func (db *OracleDBRepository) SchemaObjects(ctx context.Context) (*SchemaObjects, error) {
	query := func(what, ownerCol, q string, scan func(r *sql.Rows) error) error {
		pred, args := db.ownerFilter(ownerCol)
		rows, err := db.Conn.QueryContext(ctx, fmt.Sprintf(q, pred), args...)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
		}
		return rows.Err()
	}
	key := func(owner, name string) string { return owner + "\t" + name }
	var out []*SchemaObject
	byKey := map[string]*SchemaObject{}
	add := func(x *SchemaObject) {
		out = append(out, x)
		if x.Kind == ObjectIndex || x.Kind == ObjectConstraint {
			byKey[x.Kind+"\t"+key(x.Schema, x.Name)] = x
		}
	}

	err := query("indexes", "OWNER", `
SELECT OWNER, INDEX_NAME, TABLE_NAME, UNIQUENESS FROM ALL_INDEXES
 WHERE %s AND INDEX_TYPE <> 'LOB' AND TABLE_NAME NOT LIKE 'BIN$%%'`, func(r *sql.Rows) error {
		x := &SchemaObject{Kind: ObjectIndex}
		var uniq string
		if err := r.Scan(&x.Schema, &x.Name, &x.Table, &uniq); err != nil {
			return err
		}
		if uniq == "UNIQUE" {
			x.Detail = "unique"
		}
		add(x)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = query("index columns", "INDEX_OWNER", `
SELECT INDEX_OWNER, INDEX_NAME, COLUMN_NAME FROM ALL_IND_COLUMNS
 WHERE %s ORDER BY INDEX_OWNER, INDEX_NAME, COLUMN_POSITION`, func(r *sql.Rows) error {
		var owner, name, col string
		if err := r.Scan(&owner, &name, &col); err != nil {
			return err
		}
		if x := byKey[ObjectIndex+"\t"+key(owner, name)]; x != nil {
			x.Columns = append(x.Columns, col)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	refs := map[*SchemaObject]string{} // foreign key → referenced constraint
	err = query("constraints", "OWNER", `
SELECT OWNER, CONSTRAINT_NAME, TABLE_NAME, CONSTRAINT_TYPE, NVL(R_OWNER, '-'),
       NVL(R_CONSTRAINT_NAME, '-'), GENERATED
  FROM ALL_CONSTRAINTS
 WHERE %s AND CONSTRAINT_TYPE IN ('P', 'U', 'R', 'C') AND TABLE_NAME NOT LIKE 'BIN$%%'
   AND NOT (CONSTRAINT_TYPE = 'C' AND GENERATED = 'GENERATED NAME')`, func(r *sql.Rows) error {
		x := &SchemaObject{Kind: ObjectConstraint}
		var typ, rOwner, rName, gen string
		if err := r.Scan(&x.Schema, &x.Name, &x.Table, &typ, &rOwner, &rName, &gen); err != nil {
			return err
		}
		x.Detail = map[string]string{"P": "primary key", "U": "unique", "R": "foreign key", "C": "check"}[typ]
		x.Generated = gen == "GENERATED NAME"
		if typ == "R" {
			refs[x] = key(rOwner, rName)
		}
		add(x)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for x, k := range refs {
		if target := byKey[ObjectConstraint+"\t"+k]; target != nil {
			x.RefTable = target.Table
		}
	}
	err = query("constraint columns", "OWNER", `
SELECT OWNER, CONSTRAINT_NAME, COLUMN_NAME FROM ALL_CONS_COLUMNS
 WHERE %s AND COLUMN_NAME IS NOT NULL ORDER BY OWNER, CONSTRAINT_NAME, POSITION`, func(r *sql.Rows) error {
		var owner, name, col string
		if err := r.Scan(&owner, &name, &col); err != nil {
			return err
		}
		if x := byKey[ObjectConstraint+"\t"+key(owner, name)]; x != nil {
			x.Columns = append(x.Columns, col)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, q := range []struct{ what, kind, ownerCol, sql string }{
		{"sequences", ObjectSequence, "SEQUENCE_OWNER", `SELECT SEQUENCE_OWNER, SEQUENCE_NAME, '-', '-' FROM ALL_SEQUENCES WHERE %s`},
		{"triggers", ObjectTrigger, "OWNER", `SELECT OWNER, TRIGGER_NAME, NVL(TABLE_NAME, '-'), '-' FROM ALL_TRIGGERS WHERE %s AND TRIGGER_NAME NOT LIKE 'BIN$%%'`},
		{"types", ObjectType, "OWNER", `SELECT OWNER, TYPE_NAME, '-', LOWER(NVL(TYPECODE, '-')) FROM ALL_TYPES WHERE %s`},
	} {
		kind := q.kind
		err := query(q.what, q.ownerCol, q.sql, func(r *sql.Rows) error {
			x := &SchemaObject{Kind: kind}
			if err := r.Scan(&x.Schema, &x.Name, &x.Table, &x.Detail); err != nil {
				return err
			}
			if x.Table == "-" {
				x.Table = ""
			}
			if x.Detail == "-" {
				x.Detail = ""
			}
			add(x)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// ALL_TRIGGERS shows another owner's triggers only with CREATE ANY
	// TRIGGER, ALL_SEQUENCES only the sequences granted: for owners other
	// than the session user those lists may miss objects.
	objs := NewSchemaObjects(out)
	var user string
	if err := db.Conn.QueryRowContext(ctx, "SELECT SYS_CONTEXT('USERENV', 'SESSION_USER') FROM DUAL").Scan(&user); err != nil {
		return nil, err
	}
	owners := db.schemas
	if len(owners) == 0 {
		cur, err := db.CurrentSchema(ctx)
		if err != nil {
			return nil, err
		}
		owners = []string{cur}
	}
	for _, o := range owners {
		if !strings.EqualFold(o, user) {
			objs.MarkPartial(ObjectTrigger, o).MarkPartial(ObjectSequence, o)
		}
	}
	return objs, nil
}

// ---- SQLite -------------------------------------------------------------------

// SchemaObjects implements ObjectLister: indexes and triggers (SQLite has
// no sequences or user types, and constraints can't be altered by name).
func (db *SQLite3DBRepository) SchemaObjects(ctx context.Context) (*SchemaObjects, error) {
	rows, err := db.Conn.QueryContext(ctx, `
SELECT m.type, m.name, m.tbl_name,
       CASE WHEN m.type = 'index' THEN
         coalesce((SELECT group_concat(ii.name, ',') FROM pragma_index_info(m.name) ii), '')
       ELSE '' END
  FROM sqlite_master m
 WHERE m.type IN ('index', 'trigger') AND m.name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	list, err := scanObjects(rows, err, func(r *sql.Rows) (*SchemaObject, error) {
		var typ, name, table, cols string
		if err := r.Scan(&typ, &name, &table, &cols); err != nil {
			return nil, err
		}
		x := &SchemaObject{Kind: ObjectIndex, Name: name, Table: table, Columns: splitList(cols)}
		if typ == "trigger" {
			x.Kind = ObjectTrigger
		}
		return x, nil
	})
	if err != nil {
		return nil, err
	}
	return NewSchemaObjects(list), nil
}
