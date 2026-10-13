package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/raikhan/sqlh/dialect"
)

// FunctionLister is implemented by repositories that can list the functions
// of the database (built-in and user-defined), loaded in the background.
type FunctionLister interface {
	Functions(ctx context.Context) ([]*dialect.Function, error)
}

// FunctionCatalog indexes the callable functions of a connection: the curated
// built-ins of its dialect merged with those listed by the database.
type FunctionCatalog struct {
	// unqualified: upper-case name → overloads
	unqualified map[string][]*dialect.Function
	// qualified: upper-case qualifier → upper-case name → overloads
	qualified map[string]map[string][]*dialect.Function
}

// NewFunctionCatalog merges the built-ins of driver with dbFunctions. The
// curated signature of a built-in comes first (a readable summary, and the
// only one for syntax forms such as CURRENT_DATE or TRIM(… FROM …)), followed
// by the database's exact overloads.
func NewFunctionCatalog(driver dialect.DatabaseDriver, dbFunctions []*dialect.Function) *FunctionCatalog {
	c := &FunctionCatalog{
		unqualified: map[string][]*dialect.Function{},
		qualified:   map[string]map[string][]*dialect.Function{},
	}
	for _, f := range dialect.BuiltinFunctions(driver) {
		key := strings.ToUpper(f.Name)
		c.unqualified[key] = append(c.unqualified[key], f)
	}
	for _, f := range dbFunctions {
		key := strings.ToUpper(f.Name)
		if f.Qualifier == "" {
			if dup(c.unqualified[key], f) {
				continue
			}
			c.unqualified[key] = append(c.unqualified[key], f)
			continue
		}
		q := strings.ToUpper(f.Qualifier)
		if c.qualified[q] == nil {
			c.qualified[q] = map[string][]*dialect.Function{}
		}
		c.qualified[q][key] = append(c.qualified[q][key], f)
	}
	return c
}

// dup: a curated signature already reads the same as f.
func dup(fs []*dialect.Function, f *dialect.Function) bool {
	for _, g := range fs {
		if g.Builtin && strings.EqualFold(g.Args, f.Args) {
			return true
		}
	}
	return false
}

// Lookup returns the overloads of a function, qualified ("" = unqualified).
func (c *FunctionCatalog) Lookup(qualifier, name string) []*dialect.Function {
	if c == nil {
		return nil
	}
	if qualifier == "" {
		return c.unqualified[strings.ToUpper(name)]
	}
	return c.qualified[strings.ToUpper(qualifier)][strings.ToUpper(name)]
}

// HasQualifier: functions are known under qualifier (a package or schema).
func (c *FunctionCatalog) HasQualifier(qualifier string) bool {
	if c == nil {
		return false
	}
	_, ok := c.qualified[strings.ToUpper(qualifier)]
	return ok
}

// Names returns the overload groups callable with qualifier ("" =
// unqualified), sorted by name.
func (c *FunctionCatalog) Names(qualifier string) [][]*dialect.Function {
	if c == nil {
		return nil
	}
	m := c.unqualified
	if qualifier != "" {
		m = c.qualified[strings.ToUpper(qualifier)]
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	groups := make([][]*dialect.Function, 0, len(keys))
	for _, k := range keys {
		groups = append(groups, m[k])
	}
	return groups
}

// FunctionDoc documents the overloads of a function (markdown).
func FunctionDoc(fns []*dialect.Function) string {
	if len(fns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("```sql\n")
	for _, f := range fns {
		if f.Qualifier != "" {
			b.WriteString(f.Qualifier + ".")
		}
		b.WriteString(f.Signature() + "\n")
	}
	b.WriteString("```\n")
	seen := map[string]bool{}
	for _, f := range fns {
		if f.Doc != "" && !seen[f.Doc] {
			seen[f.Doc] = true
			b.WriteString("\n" + f.Doc + "\n")
		}
	}
	if k := fns[0].Kind; k != dialect.FunctionScalar {
		b.WriteString("\n(" + k.String() + ")\n")
	}
	return b.String()
}

// ---- PostgreSQL ------------------------------------------------------------------

// pgFunctionsQuery lists functions callable by users. Functions of pg_catalog
// and of the search path are unqualified; others keep their schema.
// Excluded: type I/O, operator implementations, planner/index/aggregate
// support and statistics internals.
const pgFunctionsQuery = `
SELECT n.nspname,
       n.nspname = 'pg_catalog' OR n.nspname = ANY (current_schemas(true)),
       p.proname,
       pg_get_function_arguments(p.oid),
       coalesce(pg_get_function_result(p.oid), ''),
       p.prokind,
       coalesce(d.description, '')
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
  LEFT JOIN pg_description d
    ON d.objoid = p.oid AND d.classoid = 'pg_proc'::regclass AND d.objsubid = 0
 WHERE n.nspname <> 'information_schema'
   AND n.nspname NOT LIKE 'pg\_toast%'
   AND n.nspname NOT LIKE 'pg\_temp%'
   AND p.proname NOT LIKE '\_%'
   AND p.prorettype NOT IN ('internal'::regtype, 'cstring'::regtype, 'trigger'::regtype,
        'event_trigger'::regtype, 'language_handler'::regtype, 'fdw_handler'::regtype,
        'index_am_handler'::regtype, 'tsm_handler'::regtype, 'table_am_handler'::regtype)
   AND NOT ('internal'::regtype = ANY (p.proargtypes::oid[])
            OR 'cstring'::regtype = ANY (p.proargtypes::oid[]))
   AND NOT EXISTS (SELECT 1 FROM pg_operator o WHERE o.oprcode = p.oid)
   AND (n.nspname <> 'pg_catalog' OR (
            p.proname NOT LIKE 'pg\_stat\_get\_%'
        AND p.proname NOT LIKE 'binary\_upgrade\_%'
        AND p.proname NOT LIKE '%\_larger'
        AND p.proname NOT LIKE '%\_smaller'
        AND p.proname NOT LIKE 'hash%'
        AND coalesce(d.description, '') NOT LIKE 'I/O%'
        AND coalesce(d.description, '') NOT LIKE '%(internal)%'
        AND coalesce(d.description, '') NOT LIKE '%support%'
        AND coalesce(d.description, '') NOT LIKE 'convert % to %'
        AND coalesce(d.description, '') NOT LIKE 'implementation of %'
        AND coalesce(d.description, '') NOT LIKE '%selectivity%'
        AND coalesce(d.description, '') NOT LIKE 'less-equal-greater%'
        AND coalesce(d.description, '') NOT LIKE 'aggregate % function'
        AND coalesce(d.description, '') NOT IN ('hash', 'increment')))
 ORDER BY p.proname, p.oid`

// Functions implements FunctionLister.
func (db *PostgreSQLDBRepository) Functions(ctx context.Context) ([]*dialect.Function, error) {
	rows, err := db.Conn.QueryContext(ctx, pgFunctionsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fns []*dialect.Function
	for rows.Next() {
		var schema, name, args, result, kind, doc string
		var onPath bool
		if err := rows.Scan(&schema, &onPath, &name, &args, &result, &kind, &doc); err != nil {
			return nil, err
		}
		f := &dialect.Function{Name: name, Args: args, Result: result, Doc: doc}
		switch kind {
		case "a":
			f.Kind = dialect.FunctionAggregate
		case "w":
			f.Kind = dialect.FunctionWindow
		case "p":
			f.Kind = dialect.FunctionProcedure
		}
		if !onPath {
			f.Qualifier = schema
		}
		fns = append(fns, f)
		if onPath && schema != "pg_catalog" {
			// also reachable qualified: schema.function
			cp := *f
			cp.Qualifier = schema
			fns = append(fns, &cp)
		}
	}
	return fns, rows.Err()
}

// ---- Oracle -----------------------------------------------------------------------

// Functions implements FunctionLister: standalone functions/procedures and
// package members of the allowed schemas. Standalone units of the current
// schema are unqualified; others are qualified by owner, package members by
// package.
func (db *OracleDBRepository) Functions(ctx context.Context) ([]*dialect.Function, error) {
	current, err := db.CurrentSchema(ctx)
	if err != nil {
		return nil, err
	}
	pred, args := db.ownerFilter("a.OWNER")
	// One row per argument; POSITION 0 without a name is a function's result.
	query := `
SELECT a.OWNER, NVL(a.PACKAGE_NAME, '-'), a.OBJECT_NAME, a.SUBPROGRAM_ID, NVL(a.OVERLOAD, '-'),
       a.POSITION, NVL(a.ARGUMENT_NAME, '-'), NVL(a.DATA_TYPE, '-'), NVL(a.IN_OUT, 'IN'),
       NVL(a.DEFAULTED, 'N')
  FROM ALL_ARGUMENTS a
 WHERE a.DATA_LEVEL = 0
   AND ` + pred + `
   AND NOT EXISTS (SELECT 1 FROM ALL_TYPES t  -- object type methods
                    WHERE t.OWNER = a.OWNER AND t.TYPE_NAME = a.PACKAGE_NAME)
 ORDER BY a.OWNER, a.PACKAGE_NAME, a.OBJECT_NAME, a.SUBPROGRAM_ID, a.POSITION`
	rows, err := db.Conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type unit struct {
		owner, pkg, name string
		args             []string
		result           string
		hasResult        bool
	}
	var units []*unit
	var cur *unit
	var curKey string
	for rows.Next() {
		var owner, pkg, name, overload, argName, dataType, inOut, defaulted string
		var subprogram, position sql.NullInt64
		if err := rows.Scan(&owner, &pkg, &name, &subprogram, &overload, &position, &argName, &dataType, &inOut, &defaulted); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s\t%s\t%s\t%d\t%s", owner, pkg, name, subprogram.Int64, overload)
		if cur == nil || key != curKey {
			cur = &unit{owner: owner, name: name}
			if pkg != "-" {
				cur.pkg = pkg
			}
			units = append(units, cur)
			curKey = key
		}
		if position.Int64 == 0 && argName == "-" {
			cur.hasResult = true
			cur.result = dataType
			continue
		}
		if argName == "-" && dataType == "-" {
			continue // a parameterless unit has one empty argument row
		}
		a := strings.ToLower(argName) + " " + dataType
		if inOut != "IN" {
			a = strings.ToLower(argName) + " " + inOut + " " + dataType
		}
		if defaulted == "Y" {
			a = "[" + a + "]"
		}
		cur.args = append(cur.args, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fns := make([]*dialect.Function, 0, len(units))
	for _, u := range units {
		f := &dialect.Function{Name: u.name, Args: strings.Join(u.args, ", "), Result: u.result}
		if !u.hasResult {
			f.Kind = dialect.FunctionProcedure
		}
		switch {
		case u.pkg != "":
			f.Qualifier = u.pkg
			f.Doc = "package " + u.owner + "." + u.pkg
		case u.owner != current:
			f.Qualifier = u.owner
		}
		fns = append(fns, f)
	}
	return fns, nil
}
