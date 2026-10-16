package handler

// Sample rows for the panel: a few rows of a table, or the most common values
// of a column, on a pooled session of their own (never the editor's pinned
// session: a sample neither waits for nor blocks a running query). The SQL is
// built from names found in the schema cache, never from the request's text.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/exec"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

const (
	sampleTimeout = 10 * time.Second
	sampleRows    = 50
	sampleValues  = 10
	sampleMaxRows = 500
	sampleCell    = 200 // characters kept of a cell
)

func (s *Server) controlSample(ctx context.Context, p control.SampleParams) (*control.SampleResult, error) {
	s.mu.Lock()
	cache := s.worker.Cache()
	if s.dbConn == nil || s.curDBCfg == nil || cache == nil {
		s.mu.Unlock()
		return nil, ErrNoConnection
	}
	cfg := s.curDBCfg
	if cfg.SamplePolicy() == database.SampleOff {
		s.mu.Unlock()
		return nil, fmt.Errorf("sample rows are off for %s (sample: off)", connAlias(cfg))
	}
	db, driver := s.dbConn.Conn, cfg.Driver
	schema, name, cols, isDefault, err := catalog.Resolve(catalog.CacheSource{C: cache}, p.Table)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	q := func(id string) string { return database.QuoteIdent(id, driver, false) }
	from := q(name)
	if !isDefault && schema != "" {
		from = q(schema) + "." + from
	}
	var sql string
	limit := p.Limit
	if p.Column != "" {
		col := ""
		for _, c := range cols {
			if strings.EqualFold(c.Name, p.Column) || strings.EqualFold(q(c.Name), p.Column) {
				col = q(c.Name)
			}
		}
		if col == "" {
			return nil, fmt.Errorf("%s has no column %q", p.Table, p.Column)
		}
		if limit <= 0 {
			limit = sampleValues
		}
		sql = limitSQL(driver, col+", COUNT(*) AS n", from, " GROUP BY "+col+" ORDER BY 2 DESC", min(limit, sampleMaxRows)+1)
	} else {
		if limit <= 0 {
			limit = sampleRows
		}
		sql = limitSQL(driver, "*", from, "", min(limit, sampleMaxRows)+1)
	}
	limit = min(limit, sampleMaxRows)

	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	start := time.Now()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, sql)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("sample of %s: no answer within %s", p.Table, sampleTimeout)
		}
		return nil, err
	}
	defer rows.Close()
	res := &control.SampleResult{Table: p.Table, SQL: sql, Rows: [][]*string{}}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	for _, t := range types {
		res.Columns = append(res.Columns, t.Name())
		res.Types = append(res.Types, t.DatabaseTypeName())
	}
	f := exec.ValueFormatter{Dialect: sqlsplit.DialectOf(string(driver))}
	vals := make([]any, len(types))
	ptrs := make([]any, len(types))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if len(res.Rows) == limit {
			res.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v == nil {
				continue
			}
			text := f.Format(v, res.Types[i])
			if utf8.RuneCountInString(text) > sampleCell {
				text = string([]rune(text)[:sampleCell]) + "… (" + strconv.Itoa(len(text)) + " bytes)"
			}
			row[i] = &text
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil && !res.Truncated {
		return nil, err
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

// limitSQL: SELECT cols FROM from tail, at most n rows, in the driver's syntax.
func limitSQL(driver dialect.DatabaseDriver, cols, from, tail string, n int) string {
	switch driver {
	case dialect.DatabaseDriverOracle:
		return fmt.Sprintf("SELECT %s FROM %s%s FETCH FIRST %d ROWS ONLY", cols, from, tail, n)
	case dialect.DatabaseDriverMssql:
		return fmt.Sprintf("SELECT TOP %d %s FROM %s%s", n, cols, from, tail)
	}
	return fmt.Sprintf("SELECT %s FROM %s%s LIMIT %d", cols, from, tail, n)
}
