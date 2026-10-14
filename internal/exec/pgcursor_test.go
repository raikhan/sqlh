package exec

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// pgSession opens a pinned session on $SQLH_TEST_PG (a DSN), e.g.
//
//	SQLH_TEST_PG='host=localhost user=postgres password=… dbname=postgres sslmode=disable'
func pgSession(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	dsn := os.Getenv("SQLH_TEST_PG")
	if dsn == "" {
		t.Skip("SQLH_TEST_PG not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return db, c
}

func pgStmt(q string) sqlsplit.Statement { return sqlsplit.Split(q, sqlsplit.PostgreSQL)[0] }

func pgRun(t *testing.T, q Querier, sql string, limit int) (Outcome, *Table, error) {
	t.Helper()
	tb := &Table{}
	out, err := Run(context.Background(), q, pgStmt(sql), func() (Sink, error) { return tb, nil }, Options{Limit: limit})
	return out, tb, err
}

func mustExec(t *testing.T, c *sql.Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := c.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// openCursors: the session's named cursors (none may be left behind; the
// extended protocol's unnamed portal, this query's own, is listed too).
func openCursors(t *testing.T, c *sql.Conn) int {
	t.Helper()
	var n int
	if err := c.QueryRowContext(context.Background(), "select count(*) from pg_cursors where name <> ''").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPGPreviewDoesNotReadEverything(t *testing.T) {
	db, c := pgSession(t)
	// a set-returning function in the select list streams (in FROM it would
	// be materialised before the first row)
	const q = "select generate_series(1, 3000000) as g, md5(g::text)"

	started := time.Now()
	out, tb, err := pgRun(t, c, strings.Replace(q, "md5(g::text)", "md5(random()::text)", 1), 100)
	cursor := time.Since(started)
	if err != nil || out.Rows != 100 || !out.Truncated || len(tb.Rows) != 100 || tb.Rows[99][0] != "100" {
		t.Fatalf("cursor preview: %+v %v", out, err)
	}

	// through the pool (no pinned session) the statement runs as written
	started = time.Now()
	out, _, err = pgRun(t, db, strings.Replace(q, "md5(g::text)", "md5(random()::text)", 1), 100)
	plain := time.Since(started)
	if err != nil || out.Rows != 100 || !out.Truncated {
		t.Fatalf("plain preview: %+v %v", out, err)
	}
	t.Logf("preview of 3M rows: cursor %s, as written %s", cursor, plain)
	if cursor > plain/5 {
		t.Errorf("cursor preview not faster: %s vs %s", cursor, plain)
	}
	if st := pgTxStatus(c); st != 'I' {
		t.Errorf("transaction left open: status %q", st)
	}
	if n := openCursors(t, c); n != 0 {
		t.Errorf("%d cursors left open", n)
	}
}

func TestPGPreviewComplete(t *testing.T) {
	_, c := pgSession(t)
	for _, q := range []string{
		"select g from generate_series(1, 5) g order by g",
		"-- leading comment\nwith x as (select g from generate_series(1, 5) g) select * from x",
		"values (1), (2), (3), (4), (5)",
	} {
		out, tb, err := pgRun(t, c, q, 5) // exactly the limit: complete
		if err != nil || out.Rows != 5 || out.Truncated || out.Kind != KindResultSet || len(tb.Columns) != 1 {
			t.Errorf("%q: %+v %v", q, out, err)
		}
	}
	out, _, err := pgRun(t, c, "select g from generate_series(1, 6) g", 5)
	if err != nil || out.Rows != 5 || !out.Truncated {
		t.Errorf("one over the limit: %+v %v", out, err)
	}
	if st := pgTxStatus(c); st != 'I' {
		t.Errorf("status %q", st)
	}
}

func TestPGPreviewInUserTransaction(t *testing.T) {
	_, c := pgSession(t)
	mustExec(t, c, "begin", "create temp table sqlh_t (id int)", "insert into sqlh_t select generate_series(1, 10)")
	defer mustExec(t, c, "rollback")

	// sees the transaction's uncommitted rows, leaves it open
	out, _, err := pgRun(t, c, "select * from sqlh_t order by id", 3)
	if err != nil || out.Rows != 3 || !out.Truncated {
		t.Fatalf("%+v %v", out, err)
	}
	if st := pgTxStatus(c); st != 'T' {
		t.Fatalf("status %q, want the user's transaction still open", st)
	}
	if n := openCursors(t, c); n != 0 {
		t.Errorf("%d cursors left open", n)
	}

	// DECLARE is refused (data-modifying WITH): runs as written, the
	// transaction survives the refusal
	out, _, err = pgRun(t, c, "with d as (delete from sqlh_t where id > 8 returning id) select * from d order by id", 5)
	if err != nil || out.Rows != 2 || out.Truncated {
		t.Fatalf("data-modifying WITH: %+v %v", out, err)
	}
	if st := pgTxStatus(c); st != 'T' {
		t.Fatalf("status %q after a refused DECLARE", st)
	}
	var n int
	if err := c.QueryRowContext(context.Background(), "select count(*) from sqlh_t").Scan(&n); err != nil || n != 8 {
		t.Errorf("rows after delete: %d %v", n, err)
	}

	// a runtime error aborts the transaction, as the statement would
	if _, _, err := pgRun(t, c, "select 1 / (id - 3) from sqlh_t order by id", 5); err == nil {
		t.Fatal("want division by zero")
	}
	if st := pgTxStatus(c); st != 'E' {
		t.Errorf("status %q, want the aborted transaction", st)
	}
	// in a failed transaction: runs as written (and fails like it)
	if _, _, err := pgRun(t, c, "select 1", 5); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("in failed transaction: %v", err)
	}
}

func TestPGPreviewRefusedOrFailingOutsideTransaction(t *testing.T) {
	_, c := pgSession(t)
	mustExec(t, c, "create temp table sqlh_u (id int)", "insert into sqlh_u select generate_series(1, 4)")

	// data-modifying WITH: runs as written, autocommitted
	out, _, err := pgRun(t, c, "with d as (delete from sqlh_u where id > 2 returning id) select * from d", 10)
	if err != nil || out.Rows != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	// SELECT … INTO creates a table: refused as a cursor, runs as written
	if _, _, err := pgRun(t, c, "select * into temp sqlh_v from sqlh_u", 10); err != nil {
		t.Fatalf("select into: %v", err)
	}
	var n int
	if err := c.QueryRowContext(context.Background(), "select count(*) from sqlh_v").Scan(&n); err != nil || n != 2 {
		t.Errorf("sqlh_v: %d %v", n, err)
	}

	// syntax error: reported like before
	if _, _, err := pgRun(t, c, "select from from sqlh_u", 10); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("syntax error: %v", err)
	}
	// runtime error during FETCH: the preview's transaction is rolled back
	if _, _, err := pgRun(t, c, "select 1 / (g - 3) from generate_series(1, 5) g", 10); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("division by zero: %v", err)
	}
	if st := pgTxStatus(c); st != 'I' {
		t.Errorf("status %q after errors", st)
	}
	if n := openCursors(t, c); n != 0 {
		t.Errorf("%d cursors left open", n)
	}
}

func TestPGPreviewCancel(t *testing.T) {
	_, c := pgSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err := Run(ctx, c, pgStmt("select pg_sleep(5)"), func() (Sink, error) { return &Table{}, nil }, Options{Limit: 10})
	if err == nil {
		t.Fatal("want an error after cancel")
	}
}

func TestPGExportUnchanged(t *testing.T) {
	_, c := pgSession(t)
	// Limit 0 (export): all rows, as written
	out, _, err := pgRun(t, c, "select g from generate_series(1, 1000) g", 0)
	if err != nil || out.Rows != 1000 || out.Truncated {
		t.Fatalf("%+v %v", out, err)
	}
}
