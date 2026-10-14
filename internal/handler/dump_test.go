package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/header"
	"github.com/raikhan/sqlh/internal/lsp"
)

// fakeEditor is an LSP client that applies workspace/applyEdit to its copy
// of the document and reports the change back with didChange, like Helix.
type fakeEditor struct {
	t    *testing.T
	conn *jsonrpc2.Conn

	mu       sync.Mutex
	text     map[string]string
	version  map[string]int
	edits    int
	messages []string
}

func (e *fakeEditor) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	switch req.Method {
	case "workspace/applyEdit":
		var p lsp.ApplyWorkspaceEditParams
		if err := json.Unmarshal(*req.Params, &p); err != nil {
			return nil, err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, dc := range p.Edit.DocumentChanges {
			uri := dc.TextDocument.URI
			if int(dc.TextDocument.Version) != e.version[uri] {
				return lsp.ApplyWorkspaceEditResult{FailureReason: "version mismatch"}, nil
			}
			for _, te := range dc.Edits {
				if te.Range.Start.Line != te.Range.End.Line || te.Range.Start.Character != 0 {
					e.t.Errorf("unexpected edit range %+v", te.Range)
				}
				e.text[uri] = header.Apply(e.text[uri], header.Edit{Line: te.Range.Start.Line, OldLen: te.Range.End.Character, Text: te.NewText})
			}
			e.version[uri]++
			e.edits++
			text, version := e.text[uri], e.version[uri]
			go func() { // like the editor: the change comes back as didChange
				_ = e.conn.Notify(context.Background(), "textDocument/didChange", lsp.DidChangeTextDocumentParams{
					TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: uri, Version: version},
					ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: text}},
				})
			}()
		}
		return lsp.ApplyWorkspaceEditResult{Applied: true}, nil
	case "window/showMessage":
		var p lsp.ShowMessageParams
		_ = json.Unmarshal(*req.Params, &p)
		e.mu.Lock()
		e.messages = append(e.messages, p.Message)
		e.mu.Unlock()
	}
	return nil, nil
}

func (e *fakeEditor) doc(uri string) (string, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.text[uri], e.edits
}

type dumpServer struct {
	t      *testing.T
	s      *Server
	editor *fakeEditor
	ws     string
	file   string
	uri    string
	cfg    *config.Config
}

func newDumpServer(t *testing.T, text string) *dumpServer {
	t.Helper()
	// Unix socket paths are short on macOS: not under t.TempDir()
	rt, err := os.MkdirTemp("/tmp", "sqlh-rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(rt) })
	t.Setenv("SQLH_RUNTIME_DIR", rt)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, db := range []string{"a.db", "b.db"} {
		c, err := sql.Open("sqlite3", "file:"+filepath.Join(base, db))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec("create table t (id integer, name text); insert into t values (1, '" + db + "'), (2, 'x')"); err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	cfg := &config.Config{Connections: []*database.DBConfig{
		{Alias: "lite", Driver: "sqlite3", DataSourceName: "file:" + filepath.Join(base, "a.db")},
		{Alias: "lite2", Driver: "sqlite3", DataSourceName: "file:" + filepath.Join(base, "b.db")},
	}}

	s := NewServer()
	s.SpecificFileCfg = cfg
	ed := &fakeEditor{t: t, text: map[string]string{}, version: map[string]int{}}
	a, b := net.Pipe()
	srvConn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(a, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(s.Handle))
	ed.conn = jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(b, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.AsyncHandler(jsonrpc2.HandlerWithError(ed.handle)))
	t.Cleanup(func() {
		ed.conn.Close()
		srvConn.Close()
		_ = s.Stop()
	})
	ctx := context.Background()
	if err := ed.conn.Call(ctx, "initialize", lsp.InitializeParams{RootURI: "file://" + ws}, nil); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(ws, "q.sql")
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := "file://" + file
	ed.text[uri], ed.version[uri] = text, 1
	if err := ed.conn.Call(ctx, "textDocument/didOpen", lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{URI: uri, LanguageID: "sql", Version: 1, Text: text},
	}, nil); err != nil {
		t.Fatal(err)
	}
	return &dumpServer{t: t, s: s, editor: ed, ws: ws, file: file, uri: uri, cfg: cfg}
}

// call is a CLI request over the control socket.
func (d *dumpServer) call(method string, params, result interface{}) error {
	d.t.Helper()
	sock, _, err := control.Find(d.ws)
	if err != nil {
		d.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, err := control.Dial(ctx, sock)
	if err != nil {
		d.t.Fatal(err)
	}
	defer cl.Close()
	return cl.Call(ctx, method, params, result)
}

func (d *dumpServer) run(p control.RunParams) *control.RunResult {
	d.t.Helper()
	var res control.RunResult
	if err := d.call(control.MethodRun, p, &res); err != nil {
		d.t.Fatalf("run %+v: %v", p, err)
	}
	return &res
}

// waitEdit waits for the editor to have applied more than before edits,
// then for the server to have the result.
func (d *dumpServer) waitEdit(before int) string {
	d.t.Helper()
	for i := 0; i < 200; i++ {
		if _, n := d.editor.doc(d.uri); n > before {
			return d.waitSynced()
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.t.Fatal("no edit arrived")
	return ""
}

func (d *dumpServer) edits() int {
	_, n := d.editor.doc(d.uri)
	return n
}

// waitSynced waits until the server has the editor's text (didChange is async).
func (d *dumpServer) waitSynced() string {
	d.t.Helper()
	for i := 0; i < 200; i++ {
		text, _ := d.editor.doc(d.uri)
		d.s.mu.Lock()
		f := d.s.files[d.uri]
		synced := f != nil && f.Text == text
		d.s.mu.Unlock()
		if synced {
			return text
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.t.Fatal("server never got the edited document")
	return ""
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHeaderStampedOnRunAndSwitch(t *testing.T) {
	d := newDumpServer(t, "select name from t order by id;\n")

	d.run(control.RunParams{File: d.file, Line: 1})
	if got := d.waitEdit(0); got != "-- sqlh: conn=lite\n\nselect name from t order by id;\n" {
		t.Fatalf("after run: %q", got)
	}
	// the file on disk is never touched by the server
	if got := readFile(t, d.file); strings.Contains(got, "sqlh:") {
		t.Errorf("file on disk edited: %q", got)
	}
	// running again: no edit
	_, edits := d.editor.doc(d.uri)
	d.run(control.RunParams{File: d.file, Line: 3})
	time.Sleep(50 * time.Millisecond)
	if _, again := d.editor.doc(d.uri); again != edits {
		t.Error("an up-to-date header was edited")
	}

	// switching from the buffer rewrites the header
	var st control.Status
	edits = d.edits()
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite2", File: d.file}, &st); err != nil {
		t.Fatal(err)
	}
	if got := d.waitEdit(edits); !strings.HasPrefix(got, "-- sqlh: conn=lite2\n") {
		t.Fatalf("after connect: %q", got)
	}
	// switching without --file leaves it; the next run updates it
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite"}, &st); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got, _ := d.editor.doc(d.uri); !strings.HasPrefix(got, "-- sqlh: conn=lite2\n") {
		t.Fatalf("connect without --file edited: %q", got)
	}
	edits = d.edits()
	d.run(control.RunParams{File: d.file, Line: 3})
	if got := d.waitEdit(edits); !strings.HasPrefix(got, "-- sqlh: conn=lite\n") {
		t.Fatalf("after run on lite: %q", got)
	}

	// SQL from stdin/--sql: no document, no edit
	_, edits = d.editor.doc(d.uri)
	d.run(control.RunParams{SQL: "select 1"})
	time.Sleep(50 * time.Millisecond)
	if _, again := d.editor.doc(d.uri); again != edits {
		t.Error("--sql run edited the document")
	}
}

func TestDumpReuse(t *testing.T) {
	d := newDumpServer(t, "-- sqlh: conn=lite\nselect name from t order by id;\n")
	out := filepath.Join(d.ws, "out", "q.csv")
	dump := func(fresh bool) *control.RunResult {
		return d.run(control.RunParams{File: d.file, Line: 2, Out: out, Dump: true, All: true, Fresh: fresh})
	}

	// nothing to reuse yet: runs
	if res := dump(false); res.ReusedFrom != "" || readFile(t, out) != "name\na.db\nx\n" {
		t.Fatalf("first dump: %+v", res)
	}

	// an export (e) is reused by the next dump
	exp := d.run(control.RunParams{File: d.file, Line: 2, All: true})
	if len(exp.Files) != 1 {
		t.Fatalf("export: %+v", exp)
	}
	os.Remove(out)
	res := dump(false)
	if res.ReusedFrom != exp.Files[0] || readFile(t, out) != "name\na.db\nx\n" || !strings.Contains(res.Summary, "reused the result from") {
		t.Fatalf("dump after export: %+v", res)
	}
	if res.Statements[0].Rows != 2 {
		t.Errorf("reused rows %+v", res.Statements)
	}
	// a dump's own output is not remembered: the export still is
	if res := dump(false); res.ReusedFrom != exp.Files[0] {
		t.Errorf("second dump: %+v", res)
	}
	// --fresh runs
	if res := dump(true); res.ReusedFrom != "" {
		t.Errorf("fresh dump reused: %+v", res)
	}

	// a statement that may write forgets everything
	d.run(control.RunParams{SQL: "update t set name = 'y' where id = 2"})
	if res := dump(false); res.ReusedFrom != "" || readFile(t, out) != "name\na.db\ny\n" {
		t.Fatalf("dump after update: %+v %q", res, readFile(t, out))
	}

	// a truncated preview (r) is not complete: not reused; a complete one is
	d.run(control.RunParams{File: d.file, Line: 2, Limit: 1})
	if res := dump(false); res.ReusedFrom != "" {
		t.Errorf("reused a truncated preview: %+v", res)
	}
	prev := d.run(control.RunParams{File: d.file, Line: 2, Limit: 100})
	if res := dump(false); res.ReusedFrom != prev.Files[0] {
		t.Errorf("complete preview not reused: %+v", res)
	}

	// another statement text (a changed query) runs
	d.editor.mu.Lock()
	d.editor.text[d.uri] = "-- sqlh: conn=lite\nselect name from t order by id desc;\n"
	d.editor.version[d.uri]++
	text, v := d.editor.text[d.uri], d.editor.version[d.uri]
	d.editor.mu.Unlock()
	_ = d.editor.conn.Notify(context.Background(), "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: d.uri, Version: v},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: text}},
	})
	d.waitSynced()
	if res := dump(false); res.ReusedFrom != "" || readFile(t, out) != "name\ny\na.db\n" {
		t.Errorf("changed query reused: %+v", res)
	}

	// results.reuseFor: 0 disables
	d.run(control.RunParams{File: d.file, Line: 2, All: true})
	d.s.mu.Lock()
	d.cfg.Results.ReuseFor = "0"
	d.s.mu.Unlock()
	if res := dump(false); res.ReusedFrom != "" {
		t.Errorf("reused with reuseFor 0: %+v", res)
	}
	d.s.mu.Lock()
	d.cfg.Results.ReuseFor = ""
	d.s.mu.Unlock()

	// a connection switch forgets (new session)
	d.run(control.RunParams{File: d.file, Line: 2, All: true})
	var st control.Status
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite2"}, &st); err != nil {
		t.Fatal(err)
	}
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite"}, &st); err != nil {
		t.Fatal(err)
	}
	if res := dump(false); res.ReusedFrom != "" {
		t.Errorf("reused across a reconnect: %+v", res)
	}
}

func TestDumpViaServerNumbersAndKeepsLatest(t *testing.T) {
	d := newDumpServer(t, "-- sqlh: conn=lite\nselect 1 as a;\nselect 2 as b;\n")
	out := filepath.Join(d.ws, "q.csv")
	res := d.run(control.RunParams{File: d.file, Out: out, Dump: true, All: true})
	if len(res.Files) != 2 || res.Files[1] != filepath.Join(d.ws, "q_2.csv") {
		t.Fatalf("files %v", res.Files)
	}
	if readFile(t, res.Files[1]) != "b\n2\n" || strings.Contains(res.Summary, "latest") {
		t.Errorf("summary %q", res.Summary)
	}
	if _, err := os.Lstat(filepath.Join(d.ws, ".sqlh", "results", "latest.csv")); !os.IsNotExist(err) {
		t.Error("dump updated latest.csv")
	}
}
