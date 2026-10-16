package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/raikhan/sqlh/internal/control"
)

// preview tabs
const (
	tabDescribe = iota
	tabDDL
	tabSample
	numTabs
)

var tabNames = []string{"Describe", "DDL", "Sample"}

// Panel is the Bubble Tea model of the side panel.
type Panel struct {
	be     Backend
	opt    Options
	ic     icons
	events <-chan control.Event

	width, height int

	cat    *control.CatalogResult
	catErr error
	objs   map[string][]control.CatalogObject
	conns  []control.Connection

	cursor   *control.Cursor
	ctx      *control.ContextResult
	hist     *control.HistoryResult
	stmtRels map[string]*control.RelationsResult

	// caches by table (as typed), cleared when the catalog changes
	cols    map[string][]control.ColumnItem
	rels    map[string]*control.RelationsResult
	errs    map[string]error
	loading map[string]bool
	ddl     map[string]ddlEntry
	samples map[string]sampleEntry // table, or table \x00 column (most common values)

	record    bool // the Sample tab shows one row at a time
	recordRow int
	sampleTok int // debounces automatic samples

	roots    []*node
	expanded map[string]bool // ids of expanded nodes (kept across rebuilds)
	marked   map[string]bool // ids of marked columns
	sel      int
	top      int
	selID    string

	tab     int
	pscroll int

	search  bool
	query   string
	items   []searchItem
	results []match
	rsel    int
	rtop    int

	menu *menu
	help bool

	msg    string
	msgErr bool
	msgAt  time.Time

	disconnected bool
	files        []string // query files (sqlh tui)
	editor       string   // the attached editor (sqlh tui)
	reloadAt     time.Time
	noTick       bool // tests: no lifetime checks
}

type sampleEntry struct {
	res     *control.SampleResult
	err     error
	loading bool
}

type ddlEntry struct {
	res     *control.DDLResult
	err     error
	loading bool
}

// NewPanel makes the panel; events are the server's notifications (and the
// backend's synthetic ones).
func NewPanel(be Backend, events <-chan control.Event, opt Options) *Panel {
	if opt.BackToEditorKey == "" {
		opt.BackToEditorKey = "backspace"
	}
	p := &Panel{be: be, opt: opt, ic: iconSet(opt.Icons), events: events, cursor: opt.Cursor,
		expanded: map[string]bool{}, marked: map[string]bool{}} // no size until the terminal reports it
	p.resetCaches()
	return p
}

// workspace: the Workspace backend in `sqlh tui`, else nil.
func (p *Panel) workspace() Workspace {
	if !p.opt.Standalone {
		return nil
	}
	ws, _ := p.be.(Workspace)
	return ws
}

func (p *Panel) resetCaches() {
	p.cols = map[string][]control.ColumnItem{}
	p.rels = map[string]*control.RelationsResult{}
	p.errs = map[string]error{}
	p.loading = map[string]bool{}
	p.ddl = map[string]ddlEntry{}
	p.samples = map[string]sampleEntry{}
}

// ---- messages ---------------------------------------------------------------

type catalogMsg struct {
	cat *control.CatalogResult
	err error
}
type connsMsg struct {
	conns []control.Connection
	err   error
}
type tableMsg struct {
	table string
	cols  []control.ColumnItem
	rel   *control.RelationsResult
	err   error
}
type contextMsg struct {
	cursor control.Cursor
	ctx    *control.ContextResult
	rels   map[string]*control.RelationsResult
	err    error
}
type ddlMsg struct {
	table string
	res   *control.DDLResult
	err   error
}
type sampleMsg struct {
	key string
	res *control.SampleResult
	err error
}
type sampleDueMsg struct{ tok int }
type historyMsg struct {
	res *control.HistoryResult
	err error
}
type eventMsg struct{ ev control.Event }
type eventsClosedMsg struct{}
type doneMsg struct {
	text   string
	err    error
	reload bool
}
type tickMsg struct{}
type aliveMsg struct {
	alive  bool
	editor string
	files  []string
}
type reloadMsg struct{}

// ---- commands ---------------------------------------------------------------

func (p *Panel) loadCatalog() tea.Cmd {
	be := p.be
	return tea.Batch(
		func() tea.Msg { c, err := be.Catalog(); return catalogMsg{c, err} },
		func() tea.Msg { c, err := be.Connections(); return connsMsg{c, err} },
	)
}

func (p *Panel) loadTable(table string) tea.Cmd {
	if table == "" || p.loading[table] {
		return nil
	}
	if _, ok := p.cols[table]; ok {
		return nil
	}
	if _, ok := p.errs[table]; ok {
		return nil
	}
	p.loading[table] = true
	be := p.be
	return func() tea.Msg {
		cols, err := be.Columns(table)
		if err != nil {
			return tableMsg{table: table, err: err}
		}
		rel, err := be.Relations(table)
		return tableMsg{table: table, cols: cols, rel: rel, err: err}
	}
}

func (p *Panel) loadContext() tea.Cmd {
	if p.cursor == nil {
		return nil
	}
	cur := *p.cursor
	be := p.be
	return func() tea.Msg {
		ctx, err := be.Context(cur)
		if err != nil {
			return contextMsg{cursor: cur, err: err}
		}
		rels := map[string]*control.RelationsResult{}
		for _, t := range ctx.Tables {
			name := t.Name
			if t.Schema != "" {
				name = t.Schema + "." + t.Name
			}
			if r, err := be.Relations(name); err == nil {
				rels[strings.ToUpper(name)] = r
			}
		}
		return contextMsg{cursor: cur, ctx: ctx, rels: rels}
	}
}

func (p *Panel) loadHistory() tea.Cmd {
	var cur *control.Cursor
	if p.cursor != nil {
		c := *p.cursor
		cur = &c
	}
	be := p.be
	return func() tea.Msg { r, err := be.History(cur); return historyMsg{r, err} }
}

func (p *Panel) loadDDL(table string) tea.Cmd {
	if e, ok := p.ddl[table]; ok && (e.loading || e.res != nil || e.err != nil) {
		return nil
	}
	p.ddl[table] = ddlEntry{loading: true}
	be := p.be
	return func() tea.Msg { r, err := be.DDL(table); return ddlMsg{table, r, err} }
}

func sampleKey(table, column string) string {
	if column == "" {
		return table
	}
	return table + "\x00" + column
}

func (p *Panel) loadSample(table, column string) tea.Cmd {
	k := sampleKey(table, column)
	if e, ok := p.samples[k]; ok && (e.loading || e.res != nil) {
		return nil
	}
	p.samples[k] = sampleEntry{loading: true}
	be := p.be
	return func() tea.Msg { r, err := be.Sample(table, column); return sampleMsg{k, r, err} }
}

// samplePolicy: auto, manual or off.
func (p *Panel) samplePolicy() string {
	if p.cat == nil || p.cat.SamplePolicy == "" {
		return "manual"
	}
	return p.cat.SamplePolicy
}

func (p *Panel) waitEvent() tea.Cmd {
	if p.events == nil {
		return nil
	}
	ch := p.events
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg{ev}
	}
}

func (p *Panel) tick() tea.Cmd {
	if p.noTick {
		return nil
	}
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

// Init starts loading.
func (p *Panel) Init() tea.Cmd {
	cmds := []tea.Cmd{p.loadCatalog(), p.loadContext(), p.loadHistory(), p.waitEvent(), p.tick()}
	if ws := p.workspace(); ws != nil {
		cmds = append(cmds, func() tea.Msg {
			files, _ := ws.QueryFiles()
			return aliveMsg{alive: true, editor: ws.Editor(), files: files}
		})
	}
	return tea.Batch(cmds...)
}

// ---- tree maintenance -------------------------------------------------------

// rebuild makes the tree from the current data, keeping expansion and the
// selection.
func (p *Panel) rebuild() []tea.Cmd {
	if cur := p.selected(); cur != nil {
		p.selID = cur.id
	}
	var roots []*node
	if p.cursor != nil || p.ctx != nil {
		roots = append(roots, buildStatement(p.ctx, p.stmtRels, p.cursor, p.ic))
	}
	if p.workspace() != nil {
		roots = append(roots, buildQueryFiles(p.files, p.ic))
	}
	h := buildHistory(p.hist, p.ic)
	// open when the statement at the cursor has earlier versions
	h.expanded = p.hist != nil && len(p.hist.Versions) > 1
	roots = append(roots, h)
	switch {
	case p.cat != nil:
		roots = append(roots, buildCatalog(p.cat, p.ic))
	case p.catErr != nil:
		roots = append(roots, &node{kind: nInfo, label: "not connected: " + p.catErr.Error(), id: "err"})
	default:
		roots = append(roots, &node{kind: nInfo, label: "loading…", id: "loading"})
	}
	roots = append(roots, buildConnections(p.conns, p.ic))
	p.roots = roots
	var cmds []tea.Cmd
	for _, r := range p.roots {
		cmds = append(cmds, p.restore(r)...)
	}
	p.restoreSelection()
	return cmds
}

// restore applies the remembered expansion to n and its descendants (nodes
// built expanded by default stay so unless the user collapsed them), filling
// lazy nodes on the way.
func (p *Panel) restore(n *node) []tea.Cmd {
	var cmds []tea.Cmd
	if e, ok := p.expanded[n.id]; ok {
		n.expanded = e
	}
	if n.expanded && n.lazy && !n.loaded {
		if c := p.ensureChildren(n); c != nil {
			cmds = append(cmds, c)
		}
	}
	for _, c := range n.children {
		cmds = append(cmds, p.restore(c)...)
	}
	return cmds
}

func (p *Panel) restoreSelection() {
	rows := visible(p.roots)
	for i, n := range rows {
		if n.id == p.selID {
			p.sel = i
			return
		}
	}
	// the nearest ancestor still visible
	id := p.selID
	for id != "" {
		k := strings.LastIndexByte(id, '/')
		if k < 0 {
			break
		}
		id = id[:k]
		for i, n := range rows {
			if n.id == id {
				p.sel = i
				return
			}
		}
	}
	if p.sel >= len(rows) {
		p.sel = max(len(rows)-1, 0)
	}
}

// ensureChildren fills a lazy node from the caches, or starts loading.
func (p *Panel) ensureChildren(n *node) tea.Cmd {
	if !n.lazy || n.loaded {
		return nil
	}
	if cols, ok := p.cols[n.table]; ok {
		fillTable(n, cols, p.rels[n.table], p.objsFor(n), p.ic)
		return nil
	}
	if err, ok := p.errs[n.table]; ok {
		n.children = nil
		n.add(&node{kind: nInfo, label: err.Error()})
		n.loaded = true
		return nil
	}
	if len(n.children) == 0 {
		n.add(&node{kind: nInfo, label: "loading…"})
	}
	n.loading = true
	return p.loadTable(n.table)
}

// objsFor: the indexes, constraints, triggers of a table node.
func (p *Panel) objsFor(n *node) []control.CatalogObject {
	schema, name := n.schema, n.obj.Name
	if name == "" {
		name = unquote(lastPart(n.table))
		if i := strings.IndexByte(n.table, '.'); i >= 0 {
			schema = unquote(n.table[:i])
		}
	}
	if schema == "" && p.cat != nil {
		schema = p.cat.Schema
		for _, s := range p.cat.Schemas {
			if s.Default {
				schema = s.Name
			}
		}
	}
	return p.objs[strings.ToUpper(schema)+"\t"+strings.ToUpper(name)]
}

func lastPart(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func unquote(s string) string { return strings.ReplaceAll(strings.Trim(s, `"`), `""`, `"`) }

func (p *Panel) rows() []*node { return visible(p.roots) }

func (p *Panel) selected() *node {
	rows := p.rows()
	if p.sel < 0 || p.sel >= len(rows) {
		return nil
	}
	return rows[p.sel]
}

func (p *Panel) setExpanded(n *node, e bool) tea.Cmd {
	n.expanded = e
	p.expanded[n.id] = e
	if e {
		return p.ensureChildren(n)
	}
	return nil
}

// ---- Update -----------------------------------------------------------------

// Update handles messages.
func (p *Panel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
		return p, nil
	case catalogMsg:
		p.cat, p.catErr = msg.cat, msg.err
		if msg.err == nil {
			p.objs = tableObjects(msg.cat)
			p.items = searchItems(msg.cat)
			if p.search {
				p.runSearch()
			}
		}
		return p, tea.Batch(append(p.rebuild(), p.previewCmd())...)
	case connsMsg:
		if msg.err == nil {
			p.conns = msg.conns
		}
		return p, tea.Batch(p.rebuild()...)
	case contextMsg:
		if msg.err == nil {
			p.ctx, p.stmtRels = msg.ctx, msg.rels
		} else {
			p.ctx, p.stmtRels = nil, nil
		}
		return p, tea.Batch(p.rebuild()...)
	case tableMsg:
		delete(p.loading, msg.table)
		if msg.err != nil {
			p.errs[msg.table] = msg.err
		} else {
			p.cols[msg.table], p.rels[msg.table] = msg.cols, msg.rel
		}
		return p, tea.Batch(p.rebuild()...)
	case ddlMsg:
		p.ddl[msg.table] = ddlEntry{res: msg.res, err: msg.err}
		return p, nil
	case sampleMsg:
		p.samples[msg.key] = sampleEntry{res: msg.res, err: msg.err}
		return p, nil
	case historyMsg:
		if msg.err == nil {
			p.hist = msg.res
		}
		return p, tea.Batch(p.rebuild()...)
	case sampleDueMsg:
		if msg.tok != p.sampleTok || p.tab != tabSample {
			return p, nil
		}
		if n := p.previewNode(); n != nil && autoSample(n) {
			return p, p.loadSample(previewTable(n), "")
		}
		return p, nil
	case eventMsg:
		return p, tea.Batch(p.onEvent(msg.ev), p.waitEvent())
	case eventsClosedMsg:
		return p, tea.Quit
	case reloadMsg:
		if time.Now().Before(p.reloadAt) {
			return p, nil
		}
		return p, p.loadCatalog()
	case doneMsg:
		if msg.err != nil {
			p.flash(msg.err.Error(), true)
		} else if msg.text != "" {
			p.flash(msg.text, false)
		}
		if msg.reload {
			return p, p.loadCatalog()
		}
		return p, nil
	case tickMsg:
		be, ws := p.be, p.workspace()
		return p, tea.Batch(p.tick(), func() tea.Msg {
			m := aliveMsg{alive: be.EditorAlive()}
			if ws != nil {
				m.editor = ws.Editor()
				m.files, _ = ws.QueryFiles()
			}
			return m
		})
	case aliveMsg:
		if !msg.alive {
			return p, tea.Quit
		}
		if p.workspace() != nil && (msg.editor != p.editor || strings.Join(msg.files, "\n") != strings.Join(p.files, "\n")) {
			p.editor, p.files = msg.editor, msg.files
			return p, tea.Batch(p.rebuild()...)
		}
		return p, nil
	case tea.MouseMsg:
		return p, p.onMouse(msg)
	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste && !p.search {
			// keys arriving together come as one message ("jj"): one at a time
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				cmds = append(cmds, p.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}))
			}
			return p, tea.Batch(cmds...)
		}
		return p, p.onKey(msg)
	}
	return p, nil
}

func (p *Panel) flash(s string, isErr bool) {
	p.msg, p.msgErr, p.msgAt = s, isErr, time.Now()
}

func (p *Panel) onEvent(ev control.Event) tea.Cmd {
	switch ev.Kind {
	case control.EventCache, control.EventConnection:
		p.resetCaches()
		walkAll(p.roots, func(n *node) {
			if n.lazy {
				n.loaded, n.loading = false, false
			}
		})
		// the background load sends several events: reload once they settle
		p.reloadAt = time.Now().Add(250 * time.Millisecond)
		cmds := []tea.Cmd{func() tea.Msg { time.Sleep(300 * time.Millisecond); return reloadMsg{} }}
		if ev.Kind == control.EventConnection {
			cmds = append(cmds, p.loadContext())
		}
		return tea.Batch(cmds...)
	case control.EventRun: // a query may have changed the data
		p.samples = map[string]sampleEntry{}
		return nil
	case control.EventHistory:
		return p.loadHistory()
	case control.EventCursor, control.EventFocus:
		if ev.Cursor != nil {
			c := *ev.Cursor
			p.cursor = &c
			return tea.Batch(p.loadContext(), p.loadHistory())
		}
	case EventDisconnected:
		p.disconnected = true
		p.flash("lost the sqlh server; reconnecting…", true)
	case EventReconnected:
		p.disconnected = false
		p.flash("reconnected", false)
		p.resetCaches()
		return tea.Batch(p.loadCatalog(), p.loadContext(), p.loadHistory())
	}
	return nil
}

// previewCmd loads what the preview of the selected node needs.
func (p *Panel) previewCmd() tea.Cmd {
	n := p.previewNode()
	if n == nil {
		return nil
	}
	table := previewTable(n)
	if table == "" {
		return nil
	}
	switch p.tab {
	case tabDDL:
		return p.loadDDL(table)
	case tabSample:
		cmds := []tea.Cmd{p.loadTable(table)}
		if p.samplePolicy() == "auto" && autoSample(n) {
			// once the selection rests: moving through the tree must not
			// query every table on the way
			p.sampleTok++
			tok := p.sampleTok
			cmds = append(cmds, func() tea.Msg { time.Sleep(300 * time.Millisecond); return sampleDueMsg{tok} })
		}
		return tea.Batch(cmds...)
	}
	return p.loadTable(table)
}

// autoSample: tables are sampled automatically (auto policy), views only on
// request (a view may be an expensive query).
func autoSample(n *node) bool {
	k := n.tableKind
	if n.rel != nil && k == "" {
		k = n.rel.Kind
	}
	return k != "view" && k != "materialized view" && previewTable(n) != ""
}

// previewTable is the table a node's preview describes.
func previewTable(n *node) string {
	switch n.kind {
	case nTable, nRelation, nStmtTable, nColumn:
		return n.table
	case nGroup:
		if n.parent != nil && (n.parent.kind == nTable || n.parent.kind == nRelation || n.parent.kind == nStmtTable) {
			return n.parent.table
		}
	case nObject:
		if n.parent != nil && n.parent.parent != nil {
			if t := n.parent.parent; t.kind == nTable || t.kind == nRelation || t.kind == nStmtTable {
				return t.table
			}
		}
	}
	return ""
}

// ---- keys -------------------------------------------------------------------

func (p *Panel) onKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if p.help {
		p.help = false
		return nil
	}
	if p.menu != nil {
		return p.menuKey(key)
	}
	if p.search {
		return p.searchKey(k)
	}
	rows := p.rows()
	n := p.selected()
	move := func(d int) tea.Cmd {
		p.sel = clamp(p.sel+d, 0, len(rows)-1)
		p.pscroll = 0
		return p.previewCmd()
	}
	switch key {
	case p.opt.BackToEditorKey:
		be := p.be
		return func() tea.Msg { return doneMsg{err: be.BackToEditor()} }
	case "q":
		return tea.Quit
	case "j", "down", "ctrl+n":
		return move(1)
	case "k", "up", "ctrl+p":
		return move(-1)
	case "g", "home":
		return move(-len(rows))
	case "G", "end":
		return move(len(rows))
	case "ctrl+d", "pgdown":
		return move(max(p.treeHeight()/2, 1))
	case "ctrl+u", "pgup":
		return move(-max(p.treeHeight()/2, 1))
	case "l", "right":
		if n == nil {
			return nil
		}
		if n.expandable() && !n.expanded {
			return tea.Batch(p.setExpanded(n, true), p.previewCmd())
		}
		if n.expanded && len(n.children) > 0 {
			return move(1)
		}
	case "h", "left":
		if n == nil {
			return nil
		}
		if n.expanded {
			return p.setExpanded(n, false)
		}
		if n.parent != nil {
			for i, r := range rows {
				if r == n.parent {
					p.sel = i
					break
				}
			}
			return p.previewCmd()
		}
	case "enter":
		return p.activate(n)
	case " ":
		if n != nil && n.kind == nColumn {
			p.marked[n.id] = !p.marked[n.id]
			return move(1)
		}
		if n != nil && n.expandable() {
			return p.setExpanded(n, !n.expanded)
		}
	case "/":
		p.search, p.query, p.rsel, p.rtop = true, "", 0, 0
		p.runSearch()
	case ".", "shift+tab", "a":
		p.openMenu(n)
	case "tab":
		p.tab = (p.tab + 1) % numTabs
		p.pscroll = 0
		return p.previewCmd()
	case "s":
		if n := p.previewNode(); n != nil && previewTable(n) != "" {
			if p.samplePolicy() == "off" {
				p.flash("sample rows are off for this connection (sample: off)", true)
				return nil
			}
			p.tab = tabSample
			table, col := previewTable(n), ""
			if n.kind == nColumn {
				col = n.col.Insert
			}
			delete(p.samples, sampleKey(table, col)) // s again: fresh
			return tea.Batch(p.loadTable(table), p.loadSample(table, col))
		}
	case "v":
		if p.tab == tabSample {
			p.record = !p.record
			p.recordRow, p.pscroll = 0, 0
		}
	case "p":
		if p.tab == tabSample && p.record {
			p.recordRow = max(p.recordRow-1, 0)
			p.pscroll = 0
		}
	case "J":
		p.pscroll++
	case "K":
		p.pscroll = max(p.pscroll-1, 0)
	case "y":
		if n != nil {
			return p.copyCmd(insertText(n))
		}
	case "o":
		if n != nil && n.kind == nHistory {
			return p.openCmd(n.run.Result)
		}
	case "n":
		if p.tab == tabSample && p.record {
			p.recordRow++
			p.pscroll = 0
		} else if p.workspace() != nil {
			return p.newQueryCmd()
		}
	case "e":
		if n != nil && n.kind == nHistory && n.run.File != "" && p.workspace() != nil {
			return p.openFileCmd(n.run.File)
		}
	case "u":
		return p.useCmd(n)
	case "r":
		be := p.be
		p.flash("refreshing…", false)
		return func() tea.Msg {
			if err := be.Refresh(); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: "schema cache refreshed"}
		}
	case "?":
		p.help = true
	}
	return nil
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

// insertText is the plain name of a node (copy, objects).
func insertText(n *node) string {
	switch n.kind {
	case nTable, nRelation, nStmtTable:
		return n.table
	case nColumn:
		return n.col.Insert
	case nObject:
		return n.obj.Insert
	case nConnItem:
		return n.conn.Alias
	case nHistory:
		return n.run.SQL
	case nSchema:
		return n.schema
	}
	return n.label
}

// activate is Enter on a node.
func (p *Panel) activate(n *node) tea.Cmd {
	if n == nil {
		return nil
	}
	switch n.kind {
	case nTable:
		return p.act(control.PanelAction{Kind: control.ActionTable, Table: n.table}, "insert "+n.table)
	case nRelation:
		return p.act(control.PanelAction{Kind: control.ActionTable, Table: n.table, Anchor: n.anchor, Path: n.path}, "insert "+n.table)
	case nColumn:
		cols := p.markedColumns(n)
		return p.act(control.PanelAction{Kind: control.ActionColumns, Table: n.table, Columns: cols}, "insert "+strings.Join(cols, ", "))
	case nObject:
		return p.act(control.PanelAction{Kind: control.ActionText, Text: n.obj.Insert}, "insert "+n.obj.Insert)
	case nConnItem:
		return p.useCmd(n)
	case nQueryFile:
		return p.openFileCmd(n.file)
	case nHistory:
		if n.version {
			if p.hist != nil && n.run.SQL == p.hist.Statement {
				p.flash("that is the current text", false)
				return nil
			}
			return p.act(control.PanelAction{Kind: control.ActionReplace, Text: n.run.SQL}, "replaced the statement")
		}
		return p.act(control.PanelAction{Kind: control.ActionText, Text: n.run.SQL}, "inserted the query")
	}
	if n.expandable() {
		return p.setExpanded(n, !n.expanded)
	}
	return nil
}

// markedColumns: the marked columns of n's table (n's own when none is
// marked), in table order; clears the marks.
func (p *Panel) markedColumns(n *node) []string {
	var cols []string
	if n.parent != nil {
		for _, c := range n.parent.children {
			if c.kind == nColumn && p.marked[c.id] {
				cols = append(cols, c.col.Insert)
				delete(p.marked, c.id)
			}
		}
	}
	if len(cols) == 0 {
		cols = []string{n.col.Insert}
	}
	return cols
}

func (p *Panel) act(a control.PanelAction, what string) tea.Cmd {
	be := p.be
	return func() tea.Msg {
		if err := be.Act(a); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: what}
	}
}

func (p *Panel) openFileCmd(path string) tea.Cmd {
	ws := p.workspace()
	if ws == nil {
		return nil
	}
	return func() tea.Msg {
		if err := ws.OpenFile(path); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "opened " + shortPath(path)}
	}
}

// newQueryCmd creates the next query file (a SELECT of the highlighted
// table, if any) and opens it.
func (p *Panel) newQueryCmd() tea.Cmd {
	ws := p.workspace()
	if ws == nil {
		return nil
	}
	body := ""
	if n := p.selected(); n != nil {
		if t := previewTable(n); t != "" && n.kind != nColumn {
			kw := func(s string) string { return s }
			if p.ctx != nil && p.ctx.LowercaseKeywords {
				kw = strings.ToLower
			}
			body = kw("SELECT") + " *\n" + kw("FROM") + " " + t + "\n"
		}
	}
	return func() tea.Msg {
		path, err := ws.NewQuery(body)
		if err != nil {
			return doneMsg{err: err}
		}
		if err := ws.OpenFile(path); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "new query " + shortPath(path)}
	}
}

func (p *Panel) openCmd(file string) tea.Cmd {
	if file == "" {
		p.flash("this run wrote no result file", true)
		return nil
	}
	be := p.be
	return func() tea.Msg {
		if err := be.Open(file); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "opened " + shortPath(file)}
	}
}

func (p *Panel) copyCmd(text string) tea.Cmd {
	be := p.be
	return func() tea.Msg {
		if err := be.Copy(text); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "copied " + text}
	}
}

// useCmd switches to a connection or schema.
func (p *Panel) useCmd(n *node) tea.Cmd {
	if n == nil {
		return nil
	}
	be := p.be
	switch n.kind {
	case nConnItem:
		alias := n.conn.Alias
		p.flash("connecting to "+alias+"…", false)
		return func() tea.Msg {
			if err := be.Connect(alias); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: "connected to " + alias, reload: true}
		}
	case nSchema:
		name := n.schema
		p.flash("switching to "+name+"…", false)
		return func() tea.Msg {
			if err := be.UseSchema(name); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: "current schema " + name, reload: true}
		}
	}
	return nil
}

// ---- search -----------------------------------------------------------------

func (p *Panel) runSearch() {
	texts := make([]string, len(p.items))
	for i, it := range p.items {
		texts[i] = it.text
	}
	p.results = fuzzyRank(p.query, texts)
	p.rsel = clamp(p.rsel, 0, len(p.results)-1)
}

func (p *Panel) searchKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		p.search = false
		return p.previewCmd()
	case "enter":
		if it := p.searchSelected(); it != nil {
			p.search = false
			if isTableKind(it.obj.Kind) {
				return p.act(control.PanelAction{Kind: control.ActionTable, Table: it.obj.Insert}, "insert "+it.obj.Insert)
			}
			return p.act(control.PanelAction{Kind: control.ActionText, Text: it.obj.Insert}, "insert "+it.obj.Insert)
		}
	case "tab", "right", "ctrl+l":
		if it := p.searchSelected(); it != nil {
			p.search = false
			return p.reveal(it)
		}
	case "up", "ctrl+p", "ctrl+k":
		p.rsel = clamp(p.rsel-1, 0, len(p.results)-1)
		return p.previewCmd()
	case "down", "ctrl+n", "ctrl+j":
		p.rsel = clamp(p.rsel+1, 0, len(p.results)-1)
		return p.previewCmd()
	case "backspace":
		if r := []rune(p.query); len(r) > 0 {
			p.query = string(r[:len(r)-1])
			p.runSearch()
		}
	case "ctrl+u":
		p.query = ""
		p.runSearch()
	case "ctrl+w":
		q := strings.TrimRight(p.query, " ")
		if i := strings.LastIndexAny(q, " ._"); i >= 0 {
			p.query = q[:i]
		} else {
			p.query = ""
		}
		p.runSearch()
	default:
		if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
			p.query += string(k.Runes)
			p.rsel, p.rtop = 0, 0
			p.runSearch()
		}
	}
	return nil
}

func (p *Panel) searchSelected() *searchItem {
	if p.rsel < 0 || p.rsel >= len(p.results) {
		return nil
	}
	return &p.items[p.results[p.rsel].index]
}

// reveal expands the tree down to a searched object and selects it.
func (p *Panel) reveal(it *searchItem) tea.Cmd {
	var target *node
	walkAll(p.roots, func(n *node) {
		if target == nil && n.parent != nil && n.parent.kind == nGroup && n.schema == it.schema &&
			(n.kind == nTable || n.kind == nObject) && n.obj.Name == it.obj.Name && n.obj.Kind == it.obj.Kind {
			target = n
		}
	})
	if target == nil {
		return nil
	}
	for a := target.parent; a != nil; a = a.parent {
		a.expanded = true
		p.expanded[a.id] = true
	}
	for i, r := range p.rows() {
		if r == target {
			p.sel = i
		}
	}
	return p.previewCmd()
}

// ---- action menu ------------------------------------------------------------

type menuItem struct {
	key, label string
	run        func() tea.Cmd
}

type menu struct {
	title string
	items []menuItem
	sel   int
}

func (p *Panel) openMenu(n *node) {
	if n == nil {
		return
	}
	var items []menuItem
	add := func(key, label string, run func() tea.Cmd) {
		items = append(items, menuItem{key, label, run})
	}
	lower := p.ctx != nil && p.ctx.LowercaseKeywords
	kw := func(s string) string {
		if lower {
			return strings.ToLower(s)
		}
		return s
	}
	switch n.kind {
	case nTable, nRelation, nStmtTable:
		table := n.table
		base := control.PanelAction{Kind: control.ActionTable, Table: table}
		if n.kind == nRelation {
			base.Anchor, base.Path = n.anchor, n.path
		}
		add("i", "insert (join after JOIN)", func() tea.Cmd { return p.act(base, "insert "+table) })
		name := base
		name.Mode = "name"
		add("n", "insert the name", func() tea.Cmd { return p.act(name, "insert "+table) })
		join := base
		join.Mode = "join"
		add("j", "insert a join", func() tea.Cmd { return p.act(join, "insert join "+table) })
		add("s", "insert SELECT * FROM "+table, func() tea.Cmd {
			return p.act(control.PanelAction{Kind: control.ActionText, Text: kw("SELECT") + " *\n" + kw("FROM") + " " + table}, "insert select")
		})
		add("c", "insert all columns", func() tea.Cmd {
			cols := p.cols[table]
			if cols == nil {
				return func() tea.Msg { return doneMsg{err: fmt.Errorf("columns of %s not loaded yet", table)} }
			}
			var names []string
			for _, c := range cols {
				names = append(names, c.Insert)
			}
			return p.act(control.PanelAction{Kind: control.ActionColumns, Table: table, Columns: names}, "insert columns")
		})
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(table) })
		add("d", "show the DDL", func() tea.Cmd { p.tab = tabDDL; p.pscroll = 0; return p.previewCmd() })
		add("S", "show sample rows", func() tea.Cmd {
			return p.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
		})
	case nColumn:
		add("i", "insert", func() tea.Cmd { return p.activate(n) })
		add("s", "most common values", func() tea.Cmd {
			return p.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
		})
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(n.col.Insert) })
	case nObject:
		add("i", "insert the name", func() tea.Cmd { return p.activate(n) })
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(n.obj.Insert) })
	case nSchema:
		add("u", "use this schema", func() tea.Cmd { return p.useCmd(n) })
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(n.schema) })
	case nConnItem:
		add("u", "connect", func() tea.Cmd { return p.useCmd(n) })
	case nHistory:
		e := n.run
		if n.version {
			add("r", "replace the statement with this version", func() tea.Cmd { return p.activate(n) })
		}
		add("i", "insert this query at the cursor", func() tea.Cmd {
			return p.act(control.PanelAction{Kind: control.ActionText, Text: e.SQL}, "inserted the query")
		})
		add("y", "copy the SQL", func() tea.Cmd { return p.copyCmd(e.SQL) })
		if e.Result != "" {
			add("o", "open the result", func() tea.Cmd { return p.openCmd(e.Result) })
		}
		if e.File != "" && p.workspace() != nil {
			add("e", "open its file in the editor", func() tea.Cmd { return p.openFileCmd(e.File) })
		}
	}
	if n.kind != nHistory {
		add("r", "refresh the schema cache", func() tea.Cmd { return p.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}) })
	}
	p.menu = &menu{title: n.label, items: items}
}

func (p *Panel) menuKey(key string) tea.Cmd {
	m := p.menu
	switch key {
	case "esc", "q", ".":
		p.menu = nil
	case "j", "down", "ctrl+n":
		m.sel = clamp(m.sel+1, 0, len(m.items)-1)
	case "k", "up", "ctrl+p":
		m.sel = clamp(m.sel-1, 0, len(m.items)-1)
	case "enter":
		p.menu = nil
		return m.items[m.sel].run()
	default:
		for _, it := range m.items {
			if it.key == key {
				p.menu = nil
				return it.run()
			}
		}
	}
	return nil
}

// ---- mouse ------------------------------------------------------------------

func (p *Panel) onMouse(m tea.MouseMsg) tea.Cmd {
	if m.Action != tea.MouseActionPress {
		return nil
	}
	switch m.Button {
	case tea.MouseButtonWheelUp:
		p.sel = clamp(p.sel-3, 0, len(p.rows())-1)
		return p.previewCmd()
	case tea.MouseButtonWheelDown:
		p.sel = clamp(p.sel+3, 0, len(p.rows())-1)
		return p.previewCmd()
	}
	return nil
}
