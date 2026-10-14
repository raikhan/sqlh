package exec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v4/stdlib"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// PostgreSQL previews (Options.Limit > 0) run through a server-side cursor.
//
// Elsewhere a capped query is sent as written and abandoned after Limit
// rows, which is cheap when the driver fetches in batches (go-ora, SQLite).
// PostgreSQL instead streams the whole result, and pgx reads it to the end
// when the rows are closed, so a preview of a large query took as long as an
// export. With a cursor only Limit+1 rows are sent, and the planner favours
// plans that return the first rows quickly (cursor_tuple_fraction). The
// statement itself is not rewritten.
//
// A cursor needs a transaction:
//   - outside one, the preview gets its own BEGIN … COMMIT: the same as the
//     statement on its own in autocommit, nothing is left open;
//   - inside the user's transaction the cursor is declared there (it sees
//     uncommitted changes), with a savepoint around DECLARE so a statement
//     that can't be a cursor leaves the transaction intact;
//   - in a failed transaction, or when DECLARE is refused (data-modifying
//     WITH, SELECT … INTO, a syntax error), the statement runs as written.
//     Errors are then reported as before.

// previewSavepoint guards DECLARE inside the user's transaction.
const previewSavepoint = "sqlh_preview"

// cursorSeq numbers the preview cursors. Each preview needs its own name:
// pgx caches prepared statements by their text, result columns included, so
// one "FETCH … FROM sqlh_preview" served every preview with the first one's
// columns — PostgreSQL refused the next query with a different column count
// (08P01 "bind message has 5 result formats but query has 9 columns") until
// the session ended, and with a 1-column first query the results were
// decoded as that column's type.
var cursorSeq atomic.Uint64

func newCursorName() string { return fmt.Sprintf("sqlh_preview_%d", cursorSeq.Add(1)) }

// cursorVerbs: statements a cursor can be declared for.
var cursorVerbs = map[string]bool{"SELECT": true, "WITH": true, "VALUES": true, "TABLE": true}

// cleanupTimeout bounds COMMIT/ROLLBACK/CLOSE after a run.
const cleanupTimeout = 10 * time.Second

// pgPreview runs a capped PostgreSQL query through a cursor. ok is false when
// it did not run the statement (not applicable, or DECLARE was refused): the
// caller runs it as written.
func pgPreview(ctx context.Context, q Querier, st sqlsplit.Statement, newSink func() (Sink, error), opts Options, out *Outcome) (ok bool, err error) {
	if opts.Limit <= 0 || !cursorVerbs[strings.ToUpper(firstWordRe.FindString(StripLeadingComments(st.SQL)))] {
		return false, nil
	}
	c, isConn := q.(*sql.Conn) // a pinned session; a pool could split the statements
	if !isConn {
		return false, nil
	}
	var own bool // the preview's own transaction, vs the user's
	switch pgTxStatus(c) {
	case 'I':
		own = true
	case 'T':
	default: // not PostgreSQL, or a failed transaction
		return false, nil
	}
	cursor := newCursorName()
	if !declare(ctx, c, cursor, st.SQL, own) {
		return false, nil
	}
	err = fetch(ctx, c, cursor, newSink, opts, out)
	return true, finish(ctx, c, cursor, own, err)
}

// pgTxStatus returns the session's transaction status ('I' idle, 'T' in a
// transaction, 'E' in a failed one), or 0 for other drivers.
func pgTxStatus(c *sql.Conn) byte {
	var st byte
	_ = c.Raw(func(dc any) error {
		if pc, ok := dc.(*stdlib.Conn); ok {
			st = pc.Conn().PgConn().TxStatus()
		}
		return nil
	})
	return st
}

// declare opens the cursor; false (with everything undone) if PostgreSQL
// refuses it.
func declare(ctx context.Context, c *sql.Conn, cursor, query string, own bool) bool {
	begin, undo := "SAVEPOINT "+previewSavepoint, []string{"ROLLBACK TO SAVEPOINT " + previewSavepoint, "RELEASE SAVEPOINT " + previewSavepoint}
	if own {
		begin, undo = "BEGIN", []string{"ROLLBACK"}
	}
	if _, err := c.ExecContext(ctx, begin); err != nil {
		return false
	}
	// a newline after FOR: the statement may start with a "--" comment
	if _, err := c.ExecContext(ctx, "DECLARE "+cursor+" NO SCROLL CURSOR FOR\n"+query); err != nil {
		cleanup(ctx, c, undo...)
		return false
	}
	if !own {
		if _, err := c.ExecContext(ctx, "RELEASE SAVEPOINT "+previewSavepoint); err != nil {
			cleanup(ctx, c, "CLOSE "+cursor)
			return false
		}
	}
	return true
}

// fetch reads Limit+1 rows: the extra one tells readRows the result was
// truncated.
func fetch(ctx context.Context, c *sql.Conn, cursor string, newSink func() (Sink, error), opts Options, out *Outcome) error {
	rows, err := c.QueryContext(ctx, fmt.Sprintf("FETCH FORWARD %d FROM %s", opts.Limit+1, cursor))
	if err != nil {
		return err
	}
	err = readRows(ctx, rows, newSink, opts, out)
	// closed here, not deferred: CLOSE/COMMIT need the connection next
	if cerr := rows.Close(); err == nil { //nolint:sqlclosecheck

		err = cerr
	}
	return err
}

// finish closes the cursor and ends the preview's own transaction. After a
// failure the own transaction is rolled back; in the user's transaction a
// database error has aborted it, exactly as the statement run as written
// would have.
func finish(ctx context.Context, c *sql.Conn, cursor string, own bool, err error) error {
	if err != nil {
		if own {
			cleanup(ctx, c, "ROLLBACK")
		} else {
			cleanup(ctx, c, "CLOSE "+cursor) // fails harmlessly if aborted
		}
		return err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if _, err := c.ExecContext(cctx, "CLOSE "+cursor); err != nil {
		if own {
			cleanup(ctx, c, "ROLLBACK")
		}
		return err
	}
	if own {
		if _, err := c.ExecContext(cctx, "COMMIT"); err != nil {
			return err
		}
	}
	return nil
}

// cleanup runs statements best-effort, also after ctx was cancelled.
func cleanup(ctx context.Context, c *sql.Conn, stmts ...string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	for _, s := range stmts {
		_, _ = c.ExecContext(cctx, s)
	}
}
