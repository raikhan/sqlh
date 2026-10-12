package exec

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultResultsDir is used when no results directory is configured.
const DefaultResultsDir = "{workspace}/.sqlh/results"

// ExpandDir resolves a results directory template. Supported: {workspace},
// {home}, {conn} and a leading "~". A relative result is taken relative to
// the workspace.
func ExpandDir(tmpl, workspace, conn string) string {
	if tmpl == "" {
		tmpl = DefaultResultsDir
	}
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(tmpl, "~/") || tmpl == "~" {
		tmpl = home + tmpl[1:]
	}
	r := strings.NewReplacer("{workspace}", workspace, "{home}", home, "{conn}", safeName(conn))
	dir := filepath.Clean(r.Replace(tmpl))
	if !filepath.IsAbs(dir) && workspace != "" {
		dir = filepath.Join(workspace, dir)
	}
	return dir
}

var unsafeNameRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func safeName(s string) string {
	s = strings.Trim(unsafeNameRe.ReplaceAllString(s, "_"), "_.")
	if s == "" {
		return "db"
	}
	return s
}

// PrepareDir creates the results directory. If it lies inside a ".sqlh"
// directory, that directory gets a ".gitignore" containing "*" so results are
// never committed (and the project's own .gitignore is left alone).
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if filepath.Base(d) == ".sqlh" {
			gi := filepath.Join(d, ".gitignore")
			if _, err := os.Stat(gi); os.IsNotExist(err) {
				return os.WriteFile(gi, []byte("# created by sqlh: query results are not versioned\n*\n"), 0o644)
			}
			return nil
		}
		if parent := filepath.Dir(d); parent == d {
			return nil
		}
	}
}

// ResultPath returns a new, unused file path: {conn}_{yyyymmdd-HHMMSS}.csv,
// with "_{index}" for the index-th result set of a multi-statement run.
func ResultPath(dir, conn string, at time.Time, index int) string {
	base := safeName(conn) + "_" + at.Format("20060102-150405")
	if index > 0 {
		base += fmt.Sprintf("_%d", index)
	}
	p := filepath.Join(dir, base+".csv")
	for n := 2; fileExists(p) || fileExists(p+".part"); n++ {
		p = filepath.Join(dir, fmt.Sprintf("%s-%d.csv", base, n))
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// LatestName is the symlink in the results directory pointing at the most
// recently completed result file.
const LatestName = "latest.csv"

// UpdateLatest points dir/latest.csv at file (a relative symlink).
func UpdateLatest(dir, file string) error {
	link := filepath.Join(dir, LatestName)
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Base(file), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link) // atomic replace
}

// CSVFile is a Sink writing one result set to a CSV file. Rows go to
// "<path>.part", which is renamed on End and removed on Abort, so an
// interrupted export never looks complete.
type CSVFile struct {
	Path string
	f    *os.File
	w    *csv.Writer
}

func NewCSVFile(path string) *CSVFile { return &CSVFile{Path: path} }

func (c *CSVFile) part() string { return c.Path + ".part" }

func (c *CSVFile) Begin(columns []string) error {
	f, err := os.OpenFile(c.part(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	c.f, c.w = f, csv.NewWriter(f)
	return c.w.Write(columns)
}

func (c *CSVFile) Row(values []string) error { return c.w.Write(values) }

func (c *CSVFile) End(bool) error {
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		c.Abort()
		return err
	}
	if err := c.f.Close(); err != nil {
		_ = os.Remove(c.part())
		return err
	}
	return os.Rename(c.part(), c.Path)
}

func (c *CSVFile) Abort() {
	if c.f != nil {
		_ = c.f.Close()
	}
	_ = os.Remove(c.part())
}

// Table is a Sink collecting rows in memory (for small, capped result sets).
type Table struct {
	Columns   []string
	Rows      [][]string
	Truncated bool
}

func (t *Table) Begin(columns []string) error {
	t.Columns = append([]string(nil), columns...)
	return nil
}

func (t *Table) Row(values []string) error {
	t.Rows = append(t.Rows, append([]string(nil), values...))
	return nil
}

func (t *Table) End(truncated bool) error { t.Truncated = truncated; return nil }
func (t *Table) Abort()                   {}
