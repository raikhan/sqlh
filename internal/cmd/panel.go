package cmd

// The side panel (`sqlh panel`): the launcher Helix runs (open or focus the
// panel of its pane), `panel take` (the hidden Helix binding inserting what
// the panel chose) and `panel run` (the TUI itself, in the panel's pane).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/connedit"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/herdr"
	"github.com/raikhan/sqlh/internal/history"
	"github.com/raikhan/sqlh/internal/tui"
)

func panelCommand() *cli.Command {
	dirFlag := &cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "find the workspace from `DIR`"}
	cursorFlags := []cli.Flag{
		&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "the editor's buffer (Helix %{buffer_name})"},
		&cli.IntFlag{Name: "line", Aliases: []string{"l"}, Usage: "1-based cursor line (Helix %{cursor_line})"},
		&cli.IntFlag{Name: "col", Usage: "1-based cursor column (Helix %{cursor_column})"},
	}
	return &cli.Command{
		Name:  "panel",
		Usage: "open (or focus) the side panel of this Helix pane: connections, objects, the statement, queries, history; Ctrl-Y inserts at the cursor",
		Flags: append([]cli.Flag{dirFlag, editorFlag("")}, cursorFlags...),
		Action: editorAction(func(c *cli.Context) error {
			return panelOpenCmd(c)
		}),
		Subcommands: []*cli.Command{
			{
				Name:   "take",
				Usage:  "print what the panel chose, for the editor's cursor (the Helix binding the panel triggers)",
				Flags:  append([]cli.Flag{dirFlag, editorFlag("")}, cursorFlags...),
				Action: editorAction(panelTakeCmd),
			},
			{
				Name:   "run",
				Hidden: true,
				Usage:  "run the panel in this terminal",
				Flags: []cli.Flag{dirFlag,
					&cli.StringFlag{Name: "editor-pane", Usage: "the Herdr pane of the editor"},
					&cli.IntFlag{Name: "editor-pid", Usage: "the editor's process"},
				},
				Action: panelRunCmd,
			},
		},
	}
}

// cursorOf reads --file/--line/--col.
func cursorOf(c *cli.Context) (*control.Cursor, error) {
	if c.String("file") == "" || c.Int("line") <= 0 {
		return nil, nil
	}
	abs, err := absPath(c.String("file"))
	if err != nil {
		return nil, err
	}
	return &control.Cursor{File: abs, Line: c.Int("line"), Col: max(c.Int("col"), 1)}, nil
}

func serverStatus(cl *control.Client) (*control.Status, error) {
	var st control.Status
	if err := call(cl, control.MethodStatus, nil, &st); err != nil {
		return nil, err
	}
	if st.Panel == nil { // an older server
		st.Panel = &control.PanelSettings{Placement: "right", InsertKeys: []string{"esc", "space", "i", "P"}}
	}
	return &st, nil
}

// ---- sqlh panel -------------------------------------------------------------

func panelOpenCmd(c *cli.Context) error {
	cl, ws, err := server(c)
	if err != nil {
		if errors.Is(err, control.ErrNoServer) {
			return errors.New("no running sqlh server for this workspace (open a .sql file in the editor first)")
		}
		return err
	}
	defer cl.Close()
	hd, err := herdr.FromEnv()
	if err != nil {
		return errors.New("the panel runs next to the editor in Herdr (outside Herdr: sqlh tui)")
	}
	editor := herdr.CurrentPane()
	cur, err := cursorOf(c)
	if err != nil {
		return err
	}
	var res control.PanelFocusResult
	if err := call(cl, control.MethodPanelFocus, control.PanelFocusParams{EditorPane: editor, Cursor: cur}, &res); err != nil {
		return err
	}
	if res.Found {
		if res.Pane == "" {
			return nil
		}
		return hd.Focus(res.Pane)
	}
	st, err := serverStatus(cl)
	if err != nil {
		return err
	}
	if ws == "" {
		ws = st.Workspace
	}
	argv, err := panelArgv(c, ws, editor, helixAncestor())
	if err != nil {
		return err
	}
	pane, err := splitFor(hd, editor, st.Panel.Placement, ws, argv, editor)
	if err != nil {
		return err
	}
	// The startup hook of ~/.config/helix runs HELIX_HELPER_ARGV; without it
	// the pane is a plain shell: type the command.
	if !waitPanel(cl, editor, 3*time.Second) {
		if err := hd.SendText(pane, "exec "+shellLine(argv)); err != nil {
			return err
		}
		return hd.SendKeys(pane, "enter")
	}
	return nil
}

// panelArgv is the command line of `sqlh panel run` for a new pane.
func panelArgv(c *cli.Context, ws, editor string, editorPID int) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv := []string{exe}
	if cfg := c.String("config"); cfg != "" {
		abs, err := absPath(cfg)
		if err != nil {
			return nil, err
		}
		argv = append(argv, "--config", abs)
	}
	argv = append(argv, "panel", "run", "-C", ws, "--editor-pane", editor)
	if editorPID > 0 {
		argv = append(argv, "--editor-pid", strconv.Itoa(editorPID))
	}
	return argv, nil
}

// splitFor creates a pane next to target per placement (half of it), started
// through the shell's HELIX_HELPER_ARGV hook, and focuses it.
func splitFor(hd *herdr.Client, target, placement, cwd string, argv []string, origin string) (string, error) {
	dir, swap := "right", false
	switch placement {
	case "left":
		swap = true
	case "down":
		dir = "down"
	case "up":
		dir, swap = "down", true
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return "", err
	}
	pane, err := hd.Split(herdr.SplitOptions{
		Target: target, Direction: dir, Ratio: 0.5, Cwd: cwd, Focus: true,
		Env: map[string]string{"HELIX_HELPER_ARGV": string(argvJSON), "HELIX_ORIGIN_PANE_ID": origin},
	})
	if err != nil {
		return "", err
	}
	if swap {
		if err := hd.Swap(pane, target); err != nil {
			return pane, err
		}
	}
	return pane, nil
}

// waitPanel waits for the panel of an editor pane to register.
func waitPanel(cl *control.Client, editor string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var panels []control.PanelInfo
		if call(cl, control.MethodPanels, nil, &panels) == nil {
			for _, p := range panels {
				if p.EditorPane == editor {
					return true
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// shellLine quotes argv for sh and Nushell alike (single quotes).
func shellLine(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(q, " ")
}

// helixAncestor is the PID of the nearest hx process among this process's
// ancestors (Helix runs `sqlh panel` through its shell), 0 if none.
func helixAncestor() int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return 0
	}
	return findAncestor(string(out), os.Getppid(), func(comm string) bool {
		return comm == "hx" || strings.HasSuffix(comm, "/hx")
	})
}

func findAncestor(ps string, pid int, match func(string) bool) int {
	type proc struct {
		ppid int
		comm string
	}
	table := map[int]proc{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		p, err1 := strconv.Atoi(f[0])
		pp, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		table[p] = proc{pp, strings.Join(f[2:], " ")}
	}
	for seen := 0; pid > 1 && seen < 64; seen++ {
		p, ok := table[pid]
		if !ok {
			return 0
		}
		if match(p.comm) {
			return pid
		}
		pid = p.ppid
	}
	return 0
}

// ---- sqlh panel take --------------------------------------------------------

func panelTakeCmd(c *cli.Context) error {
	cl, _, err := server(c)
	if err != nil {
		return err
	}
	api := serverCatalog{cl}
	defer api.close()
	var res control.PanelTakeResult
	if err := call(cl, control.MethodPanelTake, control.PanelTakeParams{EditorPane: herdr.CurrentPane()}, &res); err != nil {
		return err
	}
	if !res.Found {
		api.notify("nothing to insert: choose an object in the panel")
		return nil
	}
	if res.Action.Kind == control.ActionReplace {
		cur, err := cursorOf(c)
		if err != nil || cur == nil {
			return errors.New("replace: no cursor (--file, --line)")
		}
		var msg string
		if err := call(cl, control.MethodReplaceStatement, control.ReplaceParams{File: cur.File, Line: cur.Line, Col: cur.Col, Text: res.Action.Text}, &msg); err != nil {
			return err
		}
		api.notify(msg)
		return nil
	}
	ctx := &control.ContextResult{}
	if f := c.String("file"); f != "" && c.Int("line") > 0 {
		if ctx, err = api.context(f, c.Int("line"), max(c.Int("col"), 1)); err != nil {
			return err
		}
	}
	text, copyIt, err := panelText(api, res.Action, ctx)
	if err != nil {
		return err
	}
	if copyIt || res.Action.Copy {
		if err := copyToClipboard(strings.TrimSpace(text)); err != nil {
			return err
		}
		api.notify("copied " + strings.TrimSpace(text) + " to the clipboard (the cursor is on a table name)")
		return nil
	}
	emit([]string{text}, "")
	return nil
}

// sameTable compares table names as typed (quotes and case ignored for
// unquoted parts; a schema on one side only is ignored).
func sameTable(a, b string) bool {
	norm := func(s string) (string, string) {
		schema, name := "", s
		if i := strings.LastIndexByte(s, '.'); i >= 0 {
			schema, name = s[:i], s[i+1:]
		}
		return strings.ToUpper(strings.Trim(schema, `"`)), strings.ToUpper(strings.Trim(name, `"`))
	}
	as, an := norm(a)
	bs, bn := norm(b)
	return an == bn && (as == "" || bs == "" || as == bs)
}

// stmtTable finds a table in the statement at the cursor.
func stmtTable(ctx *control.ContextResult, table string) (control.ScopeTable, bool) {
	for _, t := range ctx.Tables {
		name := t.Name
		if t.Schema != "" {
			name = t.Schema + "." + t.Name
		}
		if sameTable(name, table) {
			return t, true
		}
	}
	return control.ScopeTable{}, false
}

// panelText turns a panel action into the text to insert at the cursor
// described by ctx; copy: put it on the clipboard instead (the cursor is on
// a table name, inserting would split it). A table's Mode: "" — the join
// clause after JOIN, else the name; name; join — the join clause, with the
// JOIN keyword when the cursor isn't after one (an error when the table
// relates to none of the statement's).
func panelText(api catalogAPI, a control.PanelAction, ctx *control.ContextResult) (string, bool, error) {
	space := ""
	if ctx.NeedSpace {
		space = " "
	}
	switch a.Kind {
	case control.ActionText:
		return space + a.Text, false, nil
	case control.ActionColumns:
		prefix := ""
		if t, ok := stmtTable(ctx, a.Table); ok && (t.Alias != "" || len(ctx.Tables) > 1) {
			prefix = t.Ref + "."
		}
		cols := make([]string, len(a.Columns))
		for i, col := range a.Columns {
			cols[i] = prefix + col
		}
		return space + strings.Join(cols, ", "), false, nil
	}
	// a table
	if a.Mode == "join" {
		clause, ok := joinClause(api, a, ctx)
		switch {
		case len(ctx.Tables) == 0:
			return "", false, fmt.Errorf("%s: no tables in the statement to join to", a.Table)
		case !ok:
			return "", false, fmt.Errorf("%s relates to none of the statement's tables", a.Table)
		case ctx.InJoin:
			return space + clause, false, nil
		}
		kw := "JOIN"
		if ctx.LowercaseKeywords {
			kw = "join"
		}
		lead := ""
		if ctx.NeedSpace {
			lead = "\n" + ctx.Indent
		}
		return lead + kw + " " + clause, false, nil
	}
	if ctx.Focus != "" && !ctx.InJoin {
		return a.Table, true, nil
	}
	mode := a.Mode
	if mode == "" {
		mode = ctx.Insert
	}
	if !ctx.InJoin || len(ctx.Tables) == 0 || mode == "name" {
		return space + a.Table, false, nil
	}
	if clause, ok := joinClause(api, a, ctx); ok {
		return space + clause, false, nil
	}
	return space + a.Table, false, nil
}

// joinClause: `customer c ON c.id = o.customer_id` (a chain from a.Anchor
// when it is in the statement, else a join to a related table of the
// statement); ok false when nothing relates.
func joinClause(api catalogAPI, a control.PanelAction, ctx *control.ContextResult) (string, bool) {
	if len(ctx.Tables) == 0 {
		return "", false
	}
	taken := map[string]bool{}
	for _, t := range ctx.Tables {
		taken[strings.ToLower(t.Ref)] = true
		taken[strings.ToLower(t.Name)] = true
	}
	// the chain from a table of the statement
	if a.Anchor != "" && len(a.Path) > 0 {
		if t, ok := stmtTable(ctx, a.Anchor); ok && len(a.Path[0].Cols) > 0 {
			steps := make([]catalog.JoinStep, len(a.Path))
			for i, h := range a.Path {
				steps[i] = catalog.JoinStep{Insert: h.Table, Cols: h.Cols}
			}
			steps[0].AnchorRef = t.Ref
			return catalog.ComposeJoin(steps, taken, ctx.LowercaseKeywords, ctx.Indent), true
		}
	}
	// a table related to one of the statement's (the focused one first)
	tables := append([]control.ScopeTable(nil), ctx.Tables...)
	for i, t := range tables {
		if ctx.Focus != "" && strings.EqualFold(t.Name, ctx.Focus) && i > 0 {
			tables = append([]control.ScopeTable{t}, append(tables[:i:i], tables[i+1:]...)...)
			break
		}
	}
	for _, t := range tables {
		name := t.Name
		if t.Schema != "" {
			name = t.Schema + "." + t.Name
		}
		rel, err := api.relations(name)
		if err != nil {
			continue
		}
		for _, r := range rel.Relations {
			if len(r.Cols) > 0 && sameTable(r.Insert, a.Table) {
				step := catalog.JoinStep{Insert: a.Table, Cols: r.Cols, AnchorRef: t.Ref}
				return catalog.ComposeJoin([]catalog.JoinStep{step}, taken, ctx.LowercaseKeywords, ctx.Indent), true
			}
		}
	}
	return "", false
}

// ---- sqlh panel run ---------------------------------------------------------

func panelRunCmd(c *cli.Context) error {
	ws := c.String("dir")
	if ws == "" {
		ws, _ = os.Getwd()
	}
	be := &panelBackend{ws: ws, events: make(chan control.Event, 64)}
	be.setEditor(c.String("editor-pane"), c.Int("editor-pid"))
	if hd, err := herdr.FromEnv(); err == nil {
		be.hd = hd
	}
	res, err := be.connect(false)
	if err != nil {
		return err
	}
	st, err := serverStatus(be.client())
	if err != nil {
		return err
	}
	be.insertKeys = st.Panel.InsertKeys
	be.settings = *st.Panel
	go be.watch()
	p := tui.NewPanel(be, be.events, tui.Options{
		Icons: st.Panel.Icons, Keys: st.Panel.Keys, Syntax: st.Panel.Syntax, StatementCollapsed: st.Panel.StatementCollapsed, Cursor: res.Cursor, Workspace: ws,
	})
	defer logAway(c)()
	prog := tea.NewProgram(p, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = prog.Run()
	return err
}

// panelBackend implements tui.Backend over the control socket and Herdr.
type panelBackend struct {
	ws         string
	editorPane string
	editorPID  int
	hd         *herdr.Client
	insertKeys []string
	events     chan control.Event
	settings   control.PanelSettings

	mu   sync.Mutex
	cl   *control.Client
	file string // the file last opened in the editor (under mu)
	// startServer starts a server when there is none (sqlh tui: headless)
	startServer func() error
	// ownServer: startServer's server is the one serving (sqlh tui)
	ownServer func() bool
}

// editor returns the editor's pane and process (they change in sqlh tui).
func (b *panelBackend) editor() (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.editorPane, b.editorPID
}

func (b *panelBackend) setEditor(pane string, pid int) {
	b.mu.Lock()
	b.editorPane, b.editorPID = pane, pid
	b.mu.Unlock()
}

func (b *panelBackend) client() *control.Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cl
}

// connect dials the workspace's server and subscribes. Without one it
// starts its own (startServer), unless waitEditor: an editor's server is
// starting (it took the socket from ours).
func (b *panelBackend) connect(waitEditor bool) (*control.SubscribeResult, error) {
	sock, _, err := control.Find(b.ws)
	if errors.Is(err, control.ErrNoServer) && b.startServer != nil && !waitEditor {
		if serr := b.startServer(); serr != nil {
			return nil, serr
		}
		sock, _, err = control.Find(b.ws)
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cl, err := control.DialEvents(ctx, sock, func(ev control.Event) {
		select {
		case b.events <- ev:
		default: // the UI is behind: drop (the next event refreshes anyway)
		}
	})
	if err != nil {
		return nil, err
	}
	var res control.SubscribeResult
	pane, _ := b.editor()
	if err := call(cl, control.MethodSubscribe, control.SubscribeParams{Pane: herdr.CurrentPane(), EditorPane: pane}, &res); err != nil {
		cl.Close()
		return nil, err
	}
	b.mu.Lock()
	b.cl = cl
	b.mu.Unlock()
	return &res, nil
}

// resubscribe tells the server which editor pane the panel now belongs to.
func (b *panelBackend) resubscribe() {
	var res control.SubscribeResult
	pane, _ := b.editor()
	_ = call(b.client(), control.MethodSubscribe, control.SubscribeParams{Pane: herdr.CurrentPane(), EditorPane: pane}, &res)
}

// watch reconnects when the server goes away (e.g. :lsp-restart, the
// editor exiting). When ours gave the socket to an editor's server, that one
// is given editorGrace to appear; when an editor's server went, ours takes
// over at once (sqlh tui).
func (b *panelBackend) watch() {
	for {
		ours := b.ownServer != nil && b.ownServer()
		<-b.client().Done()
		// released since: an editor's server is taking over
		editorStarting := ours && !b.ownServer()
		b.events <- control.Event{Kind: tui.EventDisconnected}
		start, delay := time.Now(), 20*time.Millisecond
		for {
			wait := editorStarting && time.Since(start) < editorGrace
			if _, err := b.connect(wait); err == nil {
				b.events <- control.Event{Kind: tui.EventReconnected}
				break
			}
			time.Sleep(delay)
			if !wait {
				delay = min(delay*2, time.Second)
			} else {
				delay = 100 * time.Millisecond
			}
		}
	}
}

// editorGrace: how long an editor's server that took the socket may take
// to serve it.
const editorGrace = 3 * time.Second

func (b *panelBackend) Chains(from []string, target string) (out *control.ChainsResult, err error) {
	err = call(b.client(), control.MethodChains, control.ChainsParams{From: from, Target: target}, &out)
	return
}

func (b *panelBackend) Catalog() (out *control.CatalogResult, err error) {
	err = call(b.client(), control.MethodCatalog, nil, &out)
	return
}

func (b *panelBackend) Connections() (out []control.Connection, err error) {
	err = call(b.client(), control.MethodConnections, nil, &out)
	return
}

func (b *panelBackend) Columns(table string) (out []control.ColumnItem, err error) {
	return serverCatalog{b.client()}.columns(table)
}

func (b *panelBackend) Relations(table string) (*control.RelationsResult, error) {
	return serverCatalog{b.client()}.relations(table)
}

func (b *panelBackend) Context(cur control.Cursor) (*control.ContextResult, error) {
	return serverCatalog{b.client()}.context(cur.File, cur.Line, cur.Col)
}

func (b *panelBackend) DDL(table string) (*control.DDLResult, error) {
	var res control.DDLResult
	if err := call(b.client(), control.MethodDDL, control.DDLParams{Table: table}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (b *panelBackend) Sample(table, column string) (*control.SampleResult, error) {
	var res control.SampleResult
	if err := call(b.client(), control.MethodSample, control.SampleParams{Table: table, Column: column}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (b *panelBackend) History(cur *control.Cursor) (*control.HistoryResult, error) {
	p := control.HistoryParams{}
	if cur != nil {
		p.File, p.Line, p.Col = cur.File, cur.Line, cur.Col
	}
	var res control.HistoryResult
	if err := call(b.client(), control.MethodHistory, p, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Open opens a result in results.viewer, without the panel's terminal (the
// viewer must bring its own window or pane, as the Herdr pane script does).
func (b *panelBackend) Open(file string) error {
	st, err := serverStatus(b.client())
	if err != nil {
		return err
	}
	viewer := st.Viewer
	if len(viewer) == 0 {
		viewer = []string{"open"}
		if runtime.GOOS != "darwin" {
			viewer = []string{"xdg-open"}
		}
	}
	argv := viewerArgv(viewer, []string{file})
	cmd := exec.Command(argv[0], argv[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.Join(strings.Fields(string(out)), " ")
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("viewer %s: %s", argv[0], msg)
	}
	return nil
}

func (b *panelBackend) Connect(alias string) error {
	var st control.Status
	return call(b.client(), control.MethodConnect, control.ConnectParams{Alias: alias}, &st)
}

func (b *panelBackend) Refresh() error {
	var msg string
	return call(b.client(), control.MethodRefresh, nil, &msg)
}

func (b *panelBackend) Act(a control.PanelAction) error {
	if b.hd == nil {
		return errors.New("no editor to insert into (not in Herdr)")
	}
	pane, _ := b.editor()
	if pane == "" {
		return errors.New("no editor: open a query file (n: new query)")
	}
	if !b.editorRuns() {
		return errors.New("the editor's pane is gone")
	}
	var ok string
	if err := call(b.client(), control.MethodPanelAction, control.PanelActionParams{EditorPane: pane, Action: a}, &ok); err != nil {
		return err
	}
	if err := b.hd.SendKeys(pane, b.panelSettings().InsertKeys...); err != nil {
		return err
	}
	return b.hd.Focus(pane)
}

func (b *panelBackend) Copy(text string) error { return copyToClipboard(text) }

func (b *panelBackend) BackToEditor() error {
	pane, _ := b.editor()
	if b.hd == nil || pane == "" {
		return errors.New("no editor pane")
	}
	return b.hd.Focus(pane)
}

// EditorAlive: the panel ends with its editor.
func (b *panelBackend) EditorAlive() bool { return b.editorRuns() }

// editorRuns: the editor's pane exists and runs the editor (true without
// one). Herdr hiccups count as running; only a pane or process known to be
// gone doesn't.
func (b *panelBackend) editorRuns() bool {
	pane, pid := b.editor()
	if b.hd == nil || pane == "" {
		return true
	}
	if _, err := b.hd.Pane(pane); err != nil {
		return !herdr.NotFound(err)
	}
	if pid > 0 {
		return processAlive(pid)
	}
	info, err := b.hd.ProcessInfo(pane)
	if err != nil {
		return !herdr.NotFound(err)
	}
	return info.HasProcess("hx", 0)
}

func (b *panelBackend) AllColumns() ([]control.ColumnRef, error) {
	var res []control.ColumnRef
	if err := call(b.client(), control.MethodAllColumns, nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

func (b *panelBackend) Versions(lineage string) ([]history.Entry, error) {
	var res control.HistoryResult
	if err := call(b.client(), control.MethodHistory, control.HistoryParams{Lineage: lineage}, &res); err != nil {
		return nil, err
	}
	return res.Versions, nil
}

// Editor: the panel's editor (the file last opened in it, else its pane).
func (b *panelBackend) Editor() string {
	pane, _ := b.editor()
	if pane == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != "" {
		return filepath.Base(b.file)
	}
	return "helix"
}

func (b *panelBackend) setFile(f string) {
	b.mu.Lock()
	b.file = f
	b.mu.Unlock()
}

func (b *panelBackend) queriesDir() string {
	if d := b.panelSettings().QueriesDir; d != "" {
		return d
	}
	return filepath.Join(b.ws, "queries")
}

// QueryFiles lists the query folder's .sql files, newest first.
func (b *panelBackend) QueryFiles() ([]string, error) { return queryFiles(b.queriesDir()) }

// openInEditor: the attached editor has the file open (its server says).
func (b *panelBackend) openInEditor(path string) bool {
	pane, _ := b.editor()
	if b.hd == nil || pane == "" || !b.editorRuns() {
		return false
	}
	var open bool
	if err := call(b.client(), control.MethodFileOpen, control.FileParams{File: path}, &open); err != nil {
		return false
	}
	return open
}

// helixCommand types a command into the editor (`:open 'x'`).
func (b *panelBackend) helixCommand(cmd string) error {
	pane, _ := b.editor()
	if err := b.hd.SendKeys(pane, "esc", ":"); err != nil {
		return err
	}
	if err := b.hd.SendText(pane, cmd); err != nil {
		return err
	}
	return b.hd.SendKeys(pane, "enter")
}

// queryTarget: where a query file named name goes.
func (b *panelBackend) queryTarget(name string) string {
	dst := name
	if strings.HasPrefix(dst, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dst = filepath.Join(home, dst[2:])
		}
	}
	if !filepath.IsAbs(dst) {
		dst = filepath.Join(b.queriesDir(), dst)
	}
	if filepath.Ext(dst) == "" {
		dst += ".sql"
	}
	return filepath.Clean(dst)
}

func (b *panelBackend) RenameQuery(path, name string) (string, error) {
	dst := b.queryTarget(name)
	if dst == filepath.Clean(path) {
		return "", errors.New("that is its name")
	}
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("%s exists", filepath.Base(dst))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if b.openInEditor(path) {
		// Helix renames its buffer and the file (:move acts on the current
		// buffer: show this one first)
		if err := b.helixCommand("open " + helixQuote(path)); err != nil {
			return "", err
		}
		if err := b.helixCommand("move " + helixQuote(dst)); err != nil {
			return "", err
		}
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(dst); err == nil {
				b.setFile(dst)
				return dst, nil
			}
		}
		return "", errors.New("the editor didn't move it (unsaved changes?)")
	}
	return dst, os.Rename(path, dst)
}

func (b *panelBackend) EnsureFile(path, text string) (bool, error) {
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

func (b *panelBackend) DeleteQuery(path string) error {
	if b.openInEditor(path) {
		return fmt.Errorf("%s is open in the editor: close it there first (:buffer-close)", filepath.Base(path))
	}
	return os.Remove(path)
}

// maxPreviewFile: a query file's preview reads this much.
const maxPreviewFile = 256 << 10

func (b *panelBackend) FileText(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, maxPreviewFile+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	text := string(buf[:min(n, maxPreviewFile)])
	if n > maxPreviewFile {
		text += "\n-- … (cut at 256 KB)"
	}
	return text, nil
}

func (b *panelBackend) CurrentVersion(file, sql string) (out *control.CurrentVersionResult, err error) {
	err = call(b.client(), control.MethodCurrentVersion, control.CurrentVersionParams{File: file, SQL: sql}, &out)
	return
}

// NewQuery creates the next query_<N>.sql: the connection header, then body.
func (b *panelBackend) NewQuery(body string) (string, error) {
	st, err := serverStatus(b.client())
	if err != nil {
		return "", err
	}
	alias := ""
	if st.Connection != nil {
		alias = st.Connection.Alias
	}
	return newQueryFile(b.queriesDir(), alias, body)
}

// OpenFile opens a file in the panel's editor.
func (b *panelBackend) OpenFile(path string) error {
	pane, _ := b.editor()
	if b.hd == nil || pane == "" || !b.editorRuns() {
		return errors.New("no editor to open " + filepath.Base(path) + " in")
	}
	if err := b.hd.SendKeys(pane, "esc", ":"); err != nil {
		return err
	}
	if err := b.hd.SendText(pane, "open "+helixQuote(path)); err != nil {
		return err
	}
	if err := b.hd.SendKeys(pane, "enter"); err != nil {
		return err
	}
	b.setFile(path)
	return b.hd.Focus(pane)
}

// ---- connections in the config file -------------------------------------------

func (b *panelBackend) configPath() (string, error) {
	st := b.panelSettings()
	if st.ConfigPath == "" {
		return "", errors.New("the server's config doesn't come from a file (the editor's initializationOptions): edit it there")
	}
	return st.ConfigPath, nil
}

// panelSettings: the settings (they change on a reload).
func (b *panelBackend) panelSettings() control.PanelSettings {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.settings
	if st.InsertKeys == nil {
		st.InsertKeys = b.insertKeys
	}
	return st
}

// ConfigPath: the config file the server reads.
func (b *panelBackend) ConfigPath() (string, error) { return b.configPath() }

// ReloadSettings: the server re-reads the config; the panel's settings
// from it.
func (b *panelBackend) ReloadSettings() (*control.PanelSettings, error) {
	var msg string
	if err := call(b.client(), control.MethodReloadConfig, nil, &msg); err != nil {
		return nil, err
	}
	st, err := serverStatus(b.client())
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.settings = *st.Panel
	b.insertKeys = st.Panel.InsertKeys
	b.mu.Unlock()
	return st.Panel, nil
}

// readConfigFile parses a config file for editing (missing: empty).
func readConfigFile(path string) (*connedit.File, error) {
	text, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := connedit.Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// editConfigFile edits a config file's connections: the result must be a
// valid config before it is written.
func editConfigFile(path string, edit func(*connedit.File) error) error {
	f, err := readConfigFile(path)
	if err != nil {
		return err
	}
	if err := edit(f); err != nil {
		return err
	}
	if _, err := config.Parse(f.Bytes()); err != nil {
		return err
	}
	return connedit.WriteFile(path, f.Bytes())
}

func (b *panelBackend) ConnectionFields(alias string) (connedit.Fields, error) {
	path, err := b.configPath()
	if err != nil {
		return nil, err
	}
	f, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	return f.Get(alias)
}

// editConfig edits the server's config file and has the server re-read it.
func (b *panelBackend) editConfig(edit func(*connedit.File) error) error {
	path, err := b.configPath()
	if err != nil {
		return err
	}
	if err := editConfigFile(path, edit); err != nil {
		return err
	}
	var msg string
	return call(b.client(), control.MethodReloadConfig, nil, &msg)
}

func (b *panelBackend) SaveConnection(old string, fields connedit.Fields, after string) error {
	return b.editConfig(func(f *connedit.File) error { return f.Set(old, fields, after) })
}

func (b *panelBackend) DeleteConnection(alias string) error {
	return b.editConfig(func(f *connedit.File) error { return f.Delete(alias) })
}

// TestConnection connects with the form's fields (and pings) on a
// connection of its own.
func (b *panelBackend) TestConnection(fields connedit.Fields) error { return testConnection(fields) }

func testConnection(fields connedit.Fields) error {
	f, _ := connedit.Parse(nil)
	if err := f.Set("", fields, ""); err != nil {
		return err
	}
	cfg, err := config.Parse(f.Bytes())
	if err != nil {
		return err
	}
	conn, err := database.Open(cfg.Connections[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return conn.Conn.PingContext(ctx)
}

var _ tui.Backend = (*panelBackend)(nil)
