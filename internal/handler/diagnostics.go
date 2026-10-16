package handler

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lint"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// lintDelay debounces checks while typing.
const lintDelay = 300 * time.Millisecond

var diagnosticSource = "sqlh"

// lintState schedules document checks. It has its own lock: checks run on
// timers, outside request handling (s.mu).
type lintState struct {
	mu      sync.Mutex
	timers  map[string]*time.Timer
	gen     map[string]int // per document: newest scheduled check
	cat     *lint.Catalog
	catFor  *database.DBCache
	catDial sqlsplit.Dialect
}

// scheduleLint checks a document after a short delay. s.mu must be held.
func (s *Server) scheduleLint(uri string) {
	f, ok := s.files[uri]
	if !ok || s.editor == nil {
		return
	}
	text, conn := f.Text, s.editor
	hs := s.headerState()
	enabled := s.dbConn != nil && s.curDBCfg != nil && s.getConfig().DiagnosticsEnabled()
	var d sqlsplit.Dialect
	executedKeyPrefix := ""
	if enabled {
		d = sqlsplit.DialectOf(string(s.curDBCfg.Driver))
		executedKeyPrefix = connAlias(s.curDBCfg) + "\t\t"
	}

	l := &s.lint
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timers == nil {
		l.timers, l.gen = map[string]*time.Timer{}, map[string]int{}
	}
	l.gen[uri]++
	gen := l.gen[uri]
	if t := l.timers[uri]; t != nil {
		t.Stop()
	}
	l.timers[uri] = time.AfterFunc(lintDelay, func() {
		var cat *lint.Catalog
		if enabled {
			cat = s.lintCatalog(d)
		}
		// the connection header is checked even with diagnostics: false
		opts := lint.Options{SkipDDL: s.jobs.executedDDLFilter(executedKeyPrefix)}
		diags := append(headerDiagnostics(text, hs), toLSPDiagnostics(lint.CheckWith(text, cat, opts))...)
		l.mu.Lock()
		stale := l.gen[uri] != gen
		l.mu.Unlock()
		if !stale {
			publishDiagnostics(conn, uri, diags)
		}
	})
}

// relintAll re-checks every open document (schema cache or connection changed).
func (s *Server) relintAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uri := range s.files {
		s.scheduleLint(uri)
	}
}

// clearLint drops a closed document's diagnostics. s.mu must be held.
func (s *Server) clearLint(uri string) {
	l := &s.lint
	l.mu.Lock()
	if t := l.timers[uri]; t != nil {
		t.Stop()
		delete(l.timers, uri)
	}
	delete(l.gen, uri)
	l.mu.Unlock()
	if s.editor != nil {
		publishDiagnostics(s.editor, uri, nil)
	}
}

func publishDiagnostics(conn *jsonrpc2.Conn, uri string, diags []lsp.Diagnostic) {
	if diags == nil {
		diags = []lsp.Diagnostic{} // an empty list clears them
	}
	err := conn.Notify(context.Background(), "textDocument/publishDiagnostics",
		lsp.PublishDiagnosticsParams{URI: lsp.DocumentURI(uri), Diagnostics: diags})
	if err != nil {
		log.Println("publish diagnostics:", err)
	}
}

func toLSPDiagnostics(ds []lint.Diagnostic) []lsp.Diagnostic {
	out := make([]lsp.Diagnostic, 0, len(ds))
	for _, d := range ds {
		out = append(out, lsp.Diagnostic{
			Range: lsp.Range{
				Start: lsp.Position{Line: d.Start.Line, Character: d.Start.Char},
				End:   lsp.Position{Line: d.End.Line, Character: d.End.Char},
			},
			Severity: lsp.SeverityError,
			Source:   &diagnosticSource,
			Message:  d.Message,
		})
	}
	return out
}

// lintCatalog returns the checks' view of the schema cache, rebuilt only when
// the cache changes.
func (s *Server) lintCatalog(d sqlsplit.Dialect) *lint.Catalog {
	cache := s.worker.Cache()
	if cache == nil {
		return nil
	}
	l := &s.lint
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cat != nil && l.catFor == cache && l.catDial == d {
		return l.cat
	}
	l.cat, l.catFor, l.catDial = buildCatalog(cache, d), cache, d
	return l.cat
}

func buildCatalog(cache *database.DBCache, d sqlsplit.Dialect) *lint.Catalog {
	def := cache.DefaultSchema()
	b := lint.NewCatalog(d, def)
	for _, schema := range cache.Schemas {
		b.Schema(schema)
	}
	for schema, tables := range cache.SchemaTables {
		for _, t := range tables {
			var cols []string
			if descs, ok := cache.ColumnDatabase(schema, t); ok {
				cols = make([]string, 0, len(descs))
				for _, c := range descs {
					cols = append(cols, c.Name)
				}
			}
			b.Table(schema, t, cols)
		}
	}
	for _, v := range cache.Relations.Views() {
		if w := v.Writes; w != nil {
			b.Writes(v.Schema, v.Name, v.Materialized, w.Insert, w.Update, w.Delete)
		}
	}
	if rel := cache.Relations; rel != nil && rel.ViewsLoaded {
		for _, t := range cache.SchemaTables[strings.ToUpper(def)] {
			b.Kind(def, t, string(rel.Kind(t)))
		}
		b.KindsKnown(def)
	}
	if cache.Objects != nil {
		for _, x := range cache.Objects.List() {
			if x.Kind != database.ObjectType {
				b.Object(x.Kind, x.Schema, x.Name, x.Table)
			}
		}
		for _, p := range cache.Objects.Partials() {
			kind, schema, _ := strings.Cut(p, "\t")
			b.ObjectsPartial(kind, schema)
		}
		b.ObjectsKnown(true)
	}
	switch d {
	case sqlsplit.PostgreSQL, sqlsplit.SQLite:
		b.Complete(true)
	case sqlsplit.Oracle:
		// Unqualified names may be synonyms: only sure once they're loaded,
		// and when the current schema's own tables are cached (allow-list).
		_, ownCached := cache.SchemaTables[strings.ToUpper(def)]
		for owner, names := range cache.Synonyms {
			for _, n := range names {
				switch {
				case owner == "PUBLIC" || strings.EqualFold(owner, def):
					b.Unqualified(n)
				default:
					b.Table(owner, n, nil)
				}
			}
		}
		b.Complete(cache.Synonyms != nil && ownCached)
	}
	return b.Build()
}
