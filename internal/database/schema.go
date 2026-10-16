package database

// The current schema of a connection (DBConfig.CurrentSchema) is set on every
// session the pool opens, so the editor's queries, the pinned session and
// the metadata queries all resolve unqualified names in it.
//
//   - Oracle: ALTER SESSION SET CURRENT_SCHEMA (needs no privilege; names
//     still resolve only to objects the login may see).
//   - PostgreSQL: SET search_path, the schema first and the connection's
//     original path after it (extensions in public keep working). Set after
//     connecting rather than as a startup parameter, which PgBouncer refuses
//     ("unsupported startup parameter: search_path" before PostgreSQL 18).
//     Safe with session pooling: the server connection stays ours, and
//     PgBouncer's server_reset_query (DISCARD ALL) clears it on release.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v4"
)

// initConnector runs init on every new connection of a pool.
type initConnector struct {
	driver.Connector
	init func(ctx context.Context, c driver.Conn) error
}

func (c *initConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.init(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// execDriver runs a statement without arguments on a driver connection.
func execDriver(ctx context.Context, c driver.Conn, query string) error {
	if ex, ok := c.(driver.ExecerContext); ok {
		_, err := ex.ExecContext(ctx, query, nil)
		return err
	}
	st, err := c.Prepare(query)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = st.Exec(nil) //nolint:staticcheck // drivers without ExecerContext
	return err
}

// oracleSetSchema is the statement making schema current.
func oracleSetSchema(schema string) string {
	return "ALTER SESSION SET CURRENT_SCHEMA = " + QuoteIdentAlways(schema)
}

// oracleSchemaInit sets the current schema; an unknown schema fails the
// connection (ORA-01435).
func oracleSchemaInit(schema string) func(ctx context.Context, c driver.Conn) error {
	stmt := oracleSetSchema(schema)
	return func(ctx context.Context, c driver.Conn) error {
		if err := execDriver(ctx, c, stmt); err != nil {
			return fmt.Errorf("schema %s: %w", schema, err)
		}
		return nil
	}
}

// pgSchemaAfterConnect puts schema first in the session's search_path; a
// schema that doesn't exist (or isn't usable) fails the connection.
func pgSchemaAfterConnect(schema string) func(ctx context.Context, c *pgx.Conn) error {
	return func(ctx context.Context, c *pgx.Conn) error {
		var path string
		if err := c.QueryRow(ctx, "SELECT current_setting('search_path')").Scan(&path); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, "SET search_path TO "+pgSearchPath(schema, path)); err != nil {
			return fmt.Errorf("schema %s: %w", schema, err)
		}
		var cur *string
		if err := c.QueryRow(ctx, "SELECT current_schema()").Scan(&cur); err != nil {
			return err
		}
		if cur == nil || *cur != schema {
			return fmt.Errorf("schema %q does not exist in this database, or the login has no USAGE on it", schema)
		}
		return nil
	}
}

// pgSearchPath is schema followed by the elements of path other than it.
func pgSearchPath(schema, path string) string {
	parts := []string{QuoteIdentAlways(schema)}
	for _, p := range splitSearchPath(path) {
		if pgIdentName(p) != schema {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

// splitSearchPath splits a search_path setting at commas outside quotes.
func splitSearchPath(path string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	flush := func() {
		if p := strings.TrimSpace(b.String()); p != "" {
			out = append(out, p)
		}
		b.Reset()
	}
	for _, r := range path {
		switch {
		case r == '"':
			quoted = !quoted // "" inside quotes toggles twice
		case r == ',' && !quoted:
			flush()
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return out
}

// pgIdentName is the name a search_path element stands for: quoted as
// written, unquoted folded to lower case.
func pgIdentName(p string) string {
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		return strings.ReplaceAll(p[1:len(p)-1], `""`, `"`)
	}
	return strings.ToLower(p)
}

// QuoteIdentAlways double-quotes an identifier (both dialects' quoting).
func QuoteIdentAlways(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
