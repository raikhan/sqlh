package handler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/completer"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/ddl"
	"github.com/raikhan/sqlh/internal/lint"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// TestLintProbe lints every .sql file under a directory against a live
// connection's full schema cache and prints the findings (SHOW=1: each one;
// PROBE_SCHEMA=S: pretend the login schema is S; SHOW_UNSURE=1).
//
//	SQLH_LINTPROBE=alias:dir go test ./internal/handler -run TestLintProbe -v
func TestLintProbe(t *testing.T) {
	spec := os.Getenv("SQLH_LINTPROBE")
	if spec == "" {
		t.Skip()
	}
	alias, dir, _ := strings.Cut(spec, ":")
	cfg, err := config.GetConfig(os.Getenv("HOME") + "/.config/sqlh/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	var conn *database.DBConfig
	for _, c := range cfg.Connections {
		if c.Alias == alias {
			conn = c
		}
	}
	dbc, err := database.Open(conn)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := database.CreateRepository(conn.Driver, dbc.Conn)
	if f, ok := repo.(database.SchemaFilterer); ok {
		f.SetSchemaFilter(conn.Schemas)
	}
	w := database.NewWorker()
	w.Start()
	if err := w.ReCache(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if c := w.Cache(); c.SecondaryLoaded && c.Relations != nil && c.Relations.ViewsLoaded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	d := sqlsplit.DialectOf(string(conn.Driver))
	cache := w.Cache()
	if sch := os.Getenv("PROBE_SCHEMA"); sch != "" {
		cp := *cache
		cp.SetDefaultSchema(sch)
		cache = &cp
		if cache.Objects != nil {
			for _, p := range cache.Objects.Partials() {
				fmt.Println("partial (ignored):", p)
			}
			cp.Objects = database.NewSchemaObjects(cache.Objects.List())
		}
	}
	cat := buildCatalog(cache, d)
	counts := map[string]int{}
	kinds := map[string]int{}
	unsure := map[string]int{}
	var files []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(strings.ToLower(p), ".sql") {
			files = append(files, p)
		}
		return nil
	})
	nStmt := 0
	var lintTime time.Duration
	var biggest string
	var biggestTime time.Duration
	for _, f := range files {
		b, _ := os.ReadFile(f)
		text := string(b)
		for _, st := range sqlsplit.Split(text, d) {
			nStmt++
			if a, _, ok := ddl.InStatement(st.SQL, d); ok && a.Kind != "" {
				kinds[a.Kind]++
				if a.Unsure {
					unsure[a.Kind]++
					if os.Getenv("SHOW_UNSURE") != "" && unsure[a.Kind] <= 3 {
						fmt.Printf("UNSURE %s: %.160s\n", a.Kind, strings.Join(strings.Fields(st.SQL), " "))
					}
				}
			}
		}
		t0 := time.Now()
		diags := lint.Check(text, cat)
		el := time.Since(t0)
		lintTime += el
		if el > biggestTime {
			biggestTime, biggest = el, fmt.Sprintf("%s (%d KB)", filepath.Base(f), len(text)/1024)
		}
		for _, dg := range diags {
			key := dg.Message
			if i := strings.IndexAny(key, " "); i > 0 {
				key = strings.Join(strings.Fields(dg.Message)[:2], " ")
			}
			counts[key]++
			if os.Getenv("SHOW") != "" {
				lines := strings.Split(text, "\n")
				fmt.Printf("%s:%d: %s   | %.120s\n", filepath.Base(f), dg.Start.Line+1, dg.Message, strings.TrimSpace(lines[dg.Start.Line]))
			}
		}
	}
	fmt.Println(len(files), "files", nStmt, "statements; lint", lintTime, "slowest", biggest, biggestTime)
	var ks []string
	for k, n := range kinds {
		ks = append(ks, fmt.Sprintf("%s=%d (unsure %d)", k, n, unsure[k]))
	}
	sort.Strings(ks)
	fmt.Println(strings.Join(ks, "\n"))
	fmt.Println(counts)
}

// TestComplProbe completes positions (CASES, ";;"-separated, | = cursor)
// against a live connection's full schema cache.
//
//	CASES='DROP INDEX |' SQLH_COMPLPROBE=alias go test ./internal/handler -run TestComplProbe -v
func TestComplProbe(t *testing.T) {
	alias := os.Getenv("SQLH_COMPLPROBE")
	if alias == "" {
		t.Skip()
	}
	cfg, _ := config.GetConfig(os.Getenv("HOME") + "/.config/sqlh/config.yml")
	var conn *database.DBConfig
	for _, c := range cfg.Connections {
		if c.Alias == alias {
			conn = c
		}
	}
	dbc, err := database.Open(conn)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := database.CreateRepository(conn.Driver, dbc.Conn)
	if f, ok := repo.(database.SchemaFilterer); ok {
		f.SetSchemaFilter(conn.Schemas)
	}
	w := database.NewWorker()
	w.Start()
	if err := w.ReCache(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if c := w.Cache(); c.SecondaryLoaded && c.Relations != nil && c.Relations.ViewsLoaded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	c := completer.NewCompleter(w.Cache())
	c.Driver = conn.Driver
	c.LowercaseIdentifiers = true
	for _, in := range strings.Split(os.Getenv("CASES"), ";;") {
		i := strings.Index(in, "|")
		text := in[:i] + in[i+1:]
		t0 := time.Now()
		items, err := c.Complete(text, lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{Position: lsp.Position{Line: 0, Character: i}}}, false)
		el := time.Since(t0)
		sort.Slice(items, func(a, b int) bool { return items[a].SortText < items[b].SortText })
		var labels []string
		for k, it := range items {
			if k < 8 {
				labels = append(labels, it.Label+"("+it.Detail+")")
			}
		}
		fmt.Printf("%-60s %3d items %v err=%v: %s\n", in, len(items), el.Round(time.Microsecond), err, strings.Join(labels, ", "))
	}
}
