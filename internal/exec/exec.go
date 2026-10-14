// Package exec runs SQL statements and streams result sets to sinks (CSV
// files, text tables). It has no LSP dependency so the LSP server and the CLI
// can share it.
package exec

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Sink receives one result set. Exactly one of End or Abort is called after
// a successful Begin.
type Sink interface {
	Begin(columns []string) error
	Row(values []string) error
	End(truncated bool) error
	Abort()
}

// Options control a run.
type Options struct {
	// Limit caps the rows read per result set; 0 reads everything.
	Limit  int
	Values ValueFormatter
	// Progress, if set, is called about once a second with the rows read so far.
	Progress func(rows int64)
}

type Kind int

const (
	KindNone      Kind = iota // not run to completion (error before any result)
	KindResultSet             // rows were returned (and sent to a sink)
	KindExecuted              // DML/DDL; Affected may be set
	KindPLSQL                 // an Oracle PL/SQL unit
)

// Outcome describes one executed statement.
type Outcome struct {
	Line      int // 0-based start line of the statement
	Kind      Kind
	Columns   int
	Rows      int64
	Truncated bool  // Limit was reached and more rows were available
	Affected  int64 // rows affected for DML (KindExecuted); -1 otherwise
	Elapsed   time.Duration
}

var (
	rowVerbs = map[string]bool{
		"SELECT": true, "WITH": true, "VALUES": true, "TABLE": true, "SHOW": true,
		"EXPLAIN": true, "DESCRIBE": true, "DESC": true, "FETCH": true, "PRAGMA": true,
	}
	dmlVerbs    = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true}
	returningRe = regexp.MustCompile(`(?i)\breturning\b`)
	firstWordRe = regexp.MustCompile(`^[A-Za-z]+`)
)

// ReturnsRows reports whether a statement should be run as a query.
func ReturnsRows(st sqlsplit.Statement) bool {
	if st.PLSQL {
		return false
	}
	body := StripLeadingComments(st.SQL)
	verb := strings.ToUpper(firstWordRe.FindString(body))
	switch {
	case verb == "PRAGMA":
		return !strings.Contains(body, "=") // PRAGMA x = y sets, PRAGMA x reads
	case rowVerbs[verb]:
		return true
	case dmlVerbs[verb] && returningRe.MatchString(body):
		return true // PostgreSQL/SQLite ... RETURNING
	}
	return false
}

// writeWordRe: words that make a query more than a plain read (FOR UPDATE
// included: it locks). False positives only cost a reuse.
var writeWordRe = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|upsert|into|truncate|nextval|setval|lock|call|exec|execute|perform)\b`)

// ReadOnly reports whether a statement is a plain read: SELECT, WITH, VALUES
// or TABLE without data-modifying parts. It can't see a function that writes
// (select my_proc()). Anything else may have changed what queries return.
func ReadOnly(st sqlsplit.Statement) bool {
	if st.PLSQL {
		return false
	}
	body := StripLeadingComments(st.SQL)
	return cursorVerbs[strings.ToUpper(firstWordRe.FindString(body))] && !writeWordRe.MatchString(body)
}

func isDML(sql string) bool {
	return dmlVerbs[strings.ToUpper(firstWordRe.FindString(StripLeadingComments(sql)))]
}

// StripLeadingComments removes leading "--" and "/* */" comments.
func StripLeadingComments(q string) string {
	for {
		q = strings.TrimSpace(q)
		switch {
		case strings.HasPrefix(q, "--"):
			if i := strings.IndexByte(q, '\n'); i >= 0 {
				q = q[i+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(q, "/*"):
			if i := strings.Index(q, "*/"); i >= 0 {
				q = q[i+2:]
			} else {
				return ""
			}
		default:
			return q
		}
	}
}

// Run executes one statement. newSink is called only when the statement
// returns a result set, so DML never creates output files.
func Run(ctx context.Context, q Querier, st sqlsplit.Statement, newSink func() (Sink, error), opts Options) (out Outcome, err error) {
	start := time.Now()
	out = Outcome{Line: st.Start.Line, Affected: -1}
	defer func() { out.Elapsed = time.Since(start) }()

	if !ReturnsRows(st) {
		res, err := q.ExecContext(ctx, st.SQL)
		if err != nil {
			return out, err
		}
		out.Kind = KindExecuted
		if st.PLSQL {
			out.Kind = KindPLSQL
		} else if isDML(st.SQL) {
			// only meaningful for DML; BEGIN/COMMIT/DDL report 0 or odd counts
			if n, err := res.RowsAffected(); err == nil {
				out.Affected = n
			}
		}
		return out, nil
	}

	if ok, err := pgPreview(ctx, q, st, newSink, opts, &out); ok {
		return out, err
	}

	rows, err := q.QueryContext(ctx, st.SQL)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	return out, readRows(ctx, rows, newSink, opts, &out)
}

// readRows sends a result set to a new sink (DML without a result set,
// e.g. PostgreSQL "WITH … UPDATE" without RETURNING, gets none). The caller
// closes rows.
func readRows(ctx context.Context, rows *sql.Rows, newSink func() (Sink, error), opts Options, out *Outcome) error {
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		for rows.Next() {
		}
		out.Kind = KindExecuted
		return rows.Err()
	}
	types := make([]string, len(cols))
	if cts, err := rows.ColumnTypes(); err == nil {
		for i, ct := range cts {
			types[i] = ct.DatabaseTypeName()
		}
	}
	for i, c := range cols {
		if strings.TrimSpace(c) == "" {
			cols[i] = "col" + strconv.Itoa(i+1)
		}
	}

	sink, err := newSink()
	if err != nil {
		return err
	}
	if err := sink.Begin(cols); err != nil {
		sink.Abort()
		return err
	}
	out.Kind = KindResultSet
	out.Columns = len(cols)

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	strs := make([]string, len(cols))
	lastReport := time.Now()
	for rows.Next() {
		if opts.Limit > 0 && out.Rows == int64(opts.Limit) {
			out.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			sink.Abort()
			return err
		}
		for i, v := range vals {
			strs[i] = opts.Values.Format(v, types[i])
		}
		if err := sink.Row(strs); err != nil {
			sink.Abort()
			return err
		}
		out.Rows++
		if opts.Progress != nil && out.Rows%1000 == 0 && time.Since(lastReport) >= time.Second {
			opts.Progress(out.Rows)
			lastReport = time.Now()
		}
	}
	if err := rows.Err(); err != nil {
		sink.Abort()
		return err
	}
	if err := ctx.Err(); err != nil { // cancelled between rows
		sink.Abort()
		return err
	}
	return sink.End(out.Truncated)
}
