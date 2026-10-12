// Package report formats run results for people: one-line summaries and
// text tables. Shared by the LSP server and the CLI.
package report

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"

	"github.com/raikhan/sqlh/internal/exec"
)

// Summary is the one-line result of a run, e.g.
// "oracle_dev: 100 rows (more available) → .sqlh/results/x.csv · 129ms".
// Paths are shown relative to workspace when inside it.
func Summary(alias, workspace string, outcomes []exec.Outcome, files []string, elapsed time.Duration) string {
	rel := func(p string) string {
		if r, err := filepath.Rel(workspace, p); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return p
	}
	if len(outcomes) == 1 {
		o := outcomes[0]
		switch o.Kind {
		case exec.KindResultSet:
			more := ""
			if o.Truncated {
				more = " (more available)"
			}
			return fmt.Sprintf("%s: %s%s → %s · %s", alias, RowCount(o.Rows), more, rel(files[0]), Duration(elapsed))
		case exec.KindPLSQL:
			return fmt.Sprintf("%s: PL/SQL block completed · %s", alias, Duration(elapsed))
		default:
			return fmt.Sprintf("%s: %s · %s", alias, Affected(o.Affected), Duration(elapsed))
		}
	}
	var parts []string
	aff := int64(-1) // stays -1 when no DML ran
	execs := 0
	for _, o := range outcomes {
		if o.Kind != exec.KindResultSet {
			execs++
			if o.Affected >= 0 {
				aff = max(aff, 0) + o.Affected
			}
		}
	}
	parts = append(parts, fmt.Sprintf("%d statements", len(outcomes)))
	if len(files) > 0 {
		parts = append(parts, fmt.Sprintf("%d result files (latest → %s)", len(files), rel(filepath.Join(filepath.Dir(files[len(files)-1]), exec.LatestName))))
	}
	if execs > 0 {
		parts = append(parts, Affected(aff))
	}
	parts = append(parts, Duration(elapsed))
	return alias + ": " + strings.Join(parts, " · ")
}

// RowCount: "1 row", "1,234 rows".
func RowCount(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return Thousands(n) + " rows"
}

// Affected: "3 rows affected", or "statement executed" when unknown (-1).
func Affected(n int64) string {
	switch {
	case n < 0:
		return "statement executed"
	case n == 1:
		return "1 row affected"
	default:
		return RowCount(n) + " affected"
	}
}

// Duration rounds for display.
func Duration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// Thousands formats 1234567 as 1,234,567.
func Thousands(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Text renders result tables (and DML/PL/SQL outcomes) as text.
func Text(outcomes []exec.Outcome, tables []*exec.Table, vertical bool) string {
	buf := new(bytes.Buffer)
	for i, o := range outcomes {
		if i > 0 {
			fmt.Fprintln(buf)
		}
		switch o.Kind {
		case exec.KindPLSQL:
			fmt.Fprintln(buf, "PL/SQL block completed")
			continue
		case exec.KindExecuted:
			fmt.Fprintln(buf, "Query OK, "+Affected(o.Affected))
			continue
		}
		t := tables[i]
		if vertical {
			vw := newVerticalTableWriter(buf)
			vw.setHeaders(t.Columns)
			for _, r := range t.Rows {
				vw.appendRow(r)
			}
			vw.render()
		} else {
			tbl := tablewriter.NewTable(buf, tablewriter.WithHeaderConfig(tw.CellConfig{
				Formatting: tw.CellFormatting{AutoFormat: tw.Off},
			}))
			headers := make([]any, len(t.Columns))
			for k, v := range t.Columns {
				headers[k] = v
			}
			tbl.Header(headers...)
			for _, r := range t.Rows {
				row := make([]any, len(r))
				for k, v := range r {
					row[k] = v
				}
				_ = tbl.Append(row...)
			}
			_ = tbl.Render()
		}
		if t.Truncated {
			fmt.Fprintf(buf, "first %d rows shown, more available (Export → CSV for all rows)\n", len(t.Rows))
		} else {
			fmt.Fprintf(buf, "%s in set\n", RowCount(int64(len(t.Rows))))
		}
	}
	return buf.String()
}

type verticalTableWriter struct {
	writer       io.Writer
	headers      []string
	rows         [][]string
	headerMaxLen int
}

func newVerticalTableWriter(writer io.Writer) *verticalTableWriter {
	return &verticalTableWriter{
		writer: writer,
	}
}

func (vtw *verticalTableWriter) setHeaders(headers []string) {
	vtw.headers = headers
	for _, h := range headers {
		length := len(h)
		if vtw.headerMaxLen < length {
			vtw.headerMaxLen = length
		}
	}
}

func (vtw *verticalTableWriter) appendRow(row []string) {
	vtw.rows = append(vtw.rows, row)
}

func (vtw *verticalTableWriter) render() {
	for rowNum, row := range vtw.rows {
		fmt.Fprintf(vtw.writer, "***************************[ %d. row ]***************************", rowNum+1)
		fmt.Fprintln(vtw.writer, "")
		for colNum, col := range row {
			header := vtw.headers[colNum]

			padHeader := fmt.Sprintf("%"+strconv.Itoa(vtw.headerMaxLen)+"s", header)
			fmt.Fprintf(vtw.writer, "%s | %s", padHeader, col)
			fmt.Fprintln(vtw.writer, "")
		}
	}
}
