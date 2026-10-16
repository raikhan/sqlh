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
	diags    map[string][]lsp.Diagnostic
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
		for uri, edits := range p.Edit.Changes { // unversioned (ASCII test texts)
			for _, te := range edits {
				text := e.text[uri]
				off := func(pos lsp.Position) int {
					o := 0
					for l := 0; l < pos.Line; l++ {
						o += strings.IndexByte(text[o:], '\n') + 1
					}
					return o + pos.Character
				}
				e.text[uri] = text[:off(te.Range.Start)] + te.NewText + text[off(te.Range.End):]
			}
			e.version[uri]++
			e.edits++
			text, version := e.text[uri], e.version[uri]
			go func() {
				_ = e.conn.Notify(context.Background(), "textDocument/didChange", lsp.DidChangeTextDocumentParams{
					TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: uri, Version: version},
					ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: text}},
				})
			}()
		}
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
	case "textDocument/publishDiagnostics":
		var p lsp.PublishDiagnosticsParams
		_ = json.Unmarshal(*req.Params, &p)
		e.mu.Lock()
		e.diags[string(p.URI)] = p.Diagnostics
		e.mu.Unlock()
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
	return startTestServer(t, cfg, ws, text)
}

// startTestServer starts a server on cfg with a fake editor that has
// ws/q.sql open with text.
func startTestServer(t *testing.T, cfg *config.Config, ws, text string) *dumpServer {
	t.Helper()
	s := NewServer()
	s.SpecificFileCfg = cfg
	ed := &fakeEditor{t: t, text: map[string]string{}, version: map[string]int{}, diags: map[string][]lsp.Diagnostic{}}
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

func TestHeaderDiagnostics(t *testing.T) {
	st := headerState{editor: "pg", aliases: []string{"pg", "ora"}}
	cases := []struct {
		name, text string
		st         headerState
		severity   int
		contains   string
	}{
		{"no header", "select 1", st, 0, ""},
		{"same", "-- sqlh: conn=pg\nselect 1", st, 0, ""},
		{"other connection", "-- sqlh: conn=ora\nselect 1", st, lsp.SeverityInformation, "targets ora, the editor is on pg"},
		{"db= is an error", "-- sqlh: conn=pg db=rep\nselect 1", st, lsp.SeverityError, "db=/schema= are no longer supported"},
		{"schema= is an error", "-- sqlh: conn=pg schema=x\nselect 1", st, lsp.SeverityError, "no longer supported"},
		{"unknown", "-- sqlh: conn=nope\nselect 1", st, lsp.SeverityWarning, `unknown connection "nope"; configured: pg, ora`},
		{"not connected: no comparison", "-- sqlh: conn=ora\nselect 1", headerState{aliases: st.aliases}, 0, ""},
		{"not connected: unknown still", "-- sqlh: conn=nope", headerState{aliases: st.aliases}, lsp.SeverityWarning, "unknown"},
		{"no conn key", "-- sqlh: note=x\nselect 1", st, 0, ""},
		{"db= without conn", "-- sqlh: db=x\nselect 1", st, lsp.SeverityError, "no longer supported"},
	}
	for _, c := range cases {
		ds := headerDiagnostics(c.text, c.st)
		if c.severity == 0 {
			if len(ds) != 0 {
				t.Errorf("%s: %+v", c.name, ds)
			}
			continue
		}
		if len(ds) != 1 || ds[0].Severity != c.severity || !strings.Contains(ds[0].Message, c.contains) {
			t.Errorf("%s: %+v", c.name, ds)
			continue
		}
		if r := ds[0].Range; r.Start.Line != 0 || r.Start.Character != 0 || r.End.Character != len(strings.SplitN(c.text, "\n", 2)[0]) {
			t.Errorf("%s: range %+v", c.name, r)
		}
	}
	// indented header: the range starts at the comment
	if ds := headerDiagnostics("\n  -- sqlh: conn=ora", st); len(ds) != 1 || ds[0].Range.Start != (lsp.Position{Line: 1, Character: 2}) {
		t.Errorf("indented: %+v", ds)
	}
}

// waitDiags waits until the document's published diagnostics satisfy ok.
func (d *dumpServer) waitDiags(what string, ok func([]lsp.Diagnostic) bool) []lsp.Diagnostic {
	d.t.Helper()
	var last []lsp.Diagnostic
	for i := 0; i < 300; i++ {
		d.editor.mu.Lock()
		last = d.editor.diags[d.uri]
		d.editor.mu.Unlock()
		if ok(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.t.Fatalf("%s: diagnostics %+v", what, last)
	return nil
}

func headerHint(ds []lsp.Diagnostic) *lsp.Diagnostic {
	for i := range ds {
		if ds[i].Range.Start.Line == 0 && ds[i].Severity == lsp.SeverityInformation {
			return &ds[i]
		}
	}
	return nil
}

func (d *dumpServer) codeActions(line int) []lsp.Command {
	d.t.Helper()
	var cmds []lsp.Command
	if err := d.editor.conn.Call(context.Background(), "textDocument/codeAction", lsp.CodeActionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: d.uri},
		Range:        lsp.Range{Start: lsp.Position{Line: line}, End: lsp.Position{Line: line, Character: 1}},
	}, &cmds); err != nil {
		d.t.Fatal(err)
	}
	return cmds
}

func (d *dumpServer) execute(cmd lsp.Command) {
	d.t.Helper()
	if err := d.editor.conn.Call(context.Background(), "workspace/executeCommand",
		lsp.ExecuteCommandParams{Command: cmd.Command, Arguments: cmd.Arguments}, nil); err != nil {
		d.t.Fatalf("%s: %v", cmd.Command, err)
	}
}

func TestHeaderHintAndActions(t *testing.T) {
	d := newDumpServer(t, "-- sqlh: conn=lite2\nselect name from t;\n")

	// the editor is on lite (the first connection): hint on the header
	ds := d.waitDiags("mismatch", func(ds []lsp.Diagnostic) bool { return headerHint(ds) != nil })
	if h := headerHint(ds); !strings.Contains(h.Message, "targets lite2, the editor is on lite") {
		t.Errorf("hint %q", h.Message)
	}

	// on the header line the header actions come first; elsewhere after the run actions
	cmds := d.codeActions(0)
	if len(cmds) < 2 || cmds[0].Command != CommandUseHeader || cmds[1].Command != CommandStampHeader {
		t.Fatalf("actions on the header: %+v", cmds[:2])
	}
	if !strings.Contains(cmds[0].Title, "lite2") || !strings.Contains(cmds[1].Title, "lite") {
		t.Errorf("titles %q / %q", cmds[0].Title, cmds[1].Title)
	}
	if cmds := d.codeActions(1); cmds[0].Command != CommandRun || cmds[3].Command != CommandUseHeader {
		t.Errorf("actions elsewhere: %v %v", cmds[0].Command, cmds[3].Command)
	}

	// switch the editor to the header's connection: the hint goes, no actions
	d.execute(cmds[0])
	d.waitDiags("after useHeader", func(ds []lsp.Diagnostic) bool { return headerHint(ds) == nil })
	d.s.mu.Lock()
	alias := d.s.curDBCfg.Alias
	d.s.mu.Unlock()
	if alias != "lite2" {
		t.Fatalf("editor on %s", alias)
	}
	if cmds := d.codeActions(0); cmds[0].Command == CommandUseHeader {
		t.Error("header actions offered without a mismatch")
	}

	// switching elsewhere (not from this buffer) brings it back
	var st control.Status
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite"}, &st); err != nil {
		t.Fatal(err)
	}
	d.waitDiags("after switch", func(ds []lsp.Diagnostic) bool { return headerHint(ds) != nil })

	// point the header at the editor: header rewritten, hint gone
	edits := d.edits()
	d.execute(lsp.Command{Command: CommandStampHeader, Arguments: []interface{}{d.uri}})
	if got := d.waitEdit(edits); !strings.HasPrefix(got, "-- sqlh: conn=lite\n") {
		t.Fatalf("after stamp: %q", got)
	}
	d.waitDiags("after stamp", func(ds []lsp.Diagnostic) bool { return headerHint(ds) == nil })

	// an unknown connection: warning, and only the "point at" action
	d.editor.mu.Lock()
	d.editor.text[d.uri] = "-- sqlh: conn=nope\nselect 1;\n"
	d.editor.version[d.uri]++
	text, v := d.editor.text[d.uri], d.editor.version[d.uri]
	d.editor.mu.Unlock()
	_ = d.editor.conn.Notify(context.Background(), "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: d.uri, Version: v},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: text}},
	})
	d.waitDiags("unknown", func(ds []lsp.Diagnostic) bool {
		return len(ds) > 0 && ds[0].Severity == lsp.SeverityWarning && strings.Contains(ds[0].Message, `unknown connection "nope"`)
	})
	if cmds := d.codeActions(0); cmds[0].Command != CommandStampHeader {
		t.Errorf("unknown alias actions: %v", cmds[0].Command)
	}
}
