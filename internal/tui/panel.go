package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

// main tabs
const (
	tabConns = iota
	tabObjects
	tabStmt
	tabQueries
	tabHistory
	numMainTabs
)

var mainTabNames = []string{"Connections", "Objects", "Statement", "Queries", "History"}
var mainTabShort = []string{"Conns", "Objects", "Stmt", "Queries", "History"}

// preview tabs
const (
	ptDescribe = iota
	ptDDL
	ptSample
	numPreviewTabs
)

var previewTabNames = []string{"Describe", "DDL", "Sample"}

// tabState: a main tab's tree and position.
type tabState struct {
	roots []*node
	sel   int
	top   int
	selID string
}

// Panel is the Bubble Tea model of the panel (and of sqlh tui).
type Panel struct {
	be     Backend
	opt    Options
	ic     icons
	km     keymap
	events <-chan control.Event

	width, height int

	cat     *control.CatalogResult
	catErr  error
	objs    map[string][]control.CatalogObject
	conns   []control.Connection
	allCols []control.ColumnRef
	// allColsLoading: the column search's list is on its way
	allColsLoading bool

	cursor   *control.Cursor
	ctx      *control.ContextResult
	hist     *control.HistoryResult
	stmtRels map[string]*control.RelationsResult
	files    []string
	editor   string

	// caches by table (as typed), cleared when the catalog changes
	cols     map[string][]control.ColumnItem
	rels     map[string]*control.RelationsResult
	errs     map[string]error
	loading  map[string]bool
	ddl      map[string]ddlEntry
	samples  map[string]sampleEntry // table, or table \x00 column (most common values)
	versions map[string][]history.Entry

	row       int // the Sample tab's row
	sampleTok int // debounces automatic samples

	tabs     [numMainTabs]tabState
	cur      int             // the main tab
	expanded map[string]bool // ids of expanded nodes (kept across rebuilds)
	marked   map[string]bool // ids of marked columns

	ptab    int // the preview tab
	pscroll int

	search  bool
	query   string
	skind   int
	items   []searchItem
	results []match
	rsel    int
	rtop    int
	// pendingReveal: a search result to select once its table has loaded
	pendingReveal func(*node) bool

	menu    *menu
	help    bool
	form    *connForm
	confirm *confirm

	msg    string
	msgErr bool
	msgAt  time.Time

	popup   string
	popupAt time.Time

	disconnected bool
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

// confirm asks yes/no before something destructive.
type confirm struct {
	question string
	yes      func() tea.Cmd
}

// NewPanel makes the panel; events are the server's notifications (and the
// backend's synthetic ones).
func NewPanel(be Backend, events <-chan control.Event, opt Options) *Panel {
	p := &Panel{be: be, opt: opt, ic: iconSet(opt.Icons), km: panelKeymap(opt.Keys), events: events, cursor: opt.Cursor,
		expanded: map[string]bool{}, marked: map[string]bool{}} // no size until the terminal reports it
	p.cur = tabStmt
	if opt.Standalone {
		p.cur = tabObjects
	}
	p.resetCaches()
	return p
}

func (p *Panel) resetCaches() {
	p.cols = map[string][]control.ColumnItem{}
	p.rels = map[string]*control.RelationsResult{}
	p.errs = map[string]error{}
	p.loading = map[string]bool{}
	p.ddl = map[string]ddlEntry{}
	p.samples = map[string]sampleEntry{}
	p.versions = map[string][]history.Entry{}
	p.allCols, p.allColsLoading = nil, false
}

func (p *Panel) tab() *tabState { return &p.tabs[p.cur] }

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
type versionsMsg struct {
	lineage string
	vs      []history.Entry
	err     error
}
type eventMsg struct{ ev control.Event }
type eventsClosedMsg struct{}
type doneMsg struct {
	text   string
	err    error
	reload bool
	popup  bool // say it in a popup
	tab    int  // switch to this tab + 1 (0: stay)
}
type tickMsg struct{}
type aliveMsg struct {
	alive  bool
	editor string
	files  []string
}
type reloadMsg struct{}
type popupDoneMsg struct{ at time.Time }

// ---- commands ---------------------------------------------------------------

func (p *Panel) loadCatalog() tea.Cmd {
	be := p.be
	return tea.Batch(
		func() tea.Msg { c, err := be.Catalog(); return catalogMsg{c, err} },
		p.loadConns(),
	)
}

func (p *Panel) loadConns() tea.Cmd {
	be := p.be
	return func() tea.Msg { c, err := be.Connections(); return connsMsg{c, err} }
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

func (p *Panel) poll() tea.Cmd {
	be := p.be
	return func() tea.Msg {
		files, _ := be.QueryFiles()
		return aliveMsg{alive: be.EditorAlive(), editor: be.Editor(), files: files}
	}
}

// Init starts loading.
func (p *Panel) Init() tea.Cmd {
	return tea.Batch(p.loadCatalog(), p.loadContext(), p.loadHistory(), p.waitEvent(), p.tick(), p.poll())
}

// ---- tree maintenance -------------------------------------------------------

// rebuild makes every tab's tree from the current data, keeping expansion
// and the selections.
func (p *Panel) rebuild() []tea.Cmd {
	for i := range p.tabs {
		t := &p.tabs[i]
		rows := visible(t.roots)
		if t.sel >= 0 && t.sel < len(rows) {
			t.selID = rows[t.sel].id
		}
	}
	p.tabs[tabConns].roots = buildConnections(p.conns, p.ic)
	switch {
	case p.cat != nil:
		p.tabs[tabObjects].roots = buildObjects(p.cat, p.ic)
	case p.catErr != nil:
		p.tabs[tabObjects].roots = []*node{top("obj", &node{kind: nInfo, label: "not connected: " + p.catErr.Error()})}
	default:
		p.tabs[tabObjects].roots = []*node{top("obj", &node{kind: nInfo, label: "loading…"})}
	}
	p.tabs[tabStmt].roots = buildStatement(p.ctx, p.stmtRels, p.cursor, p.editor != "" || !p.opt.Standalone, p.ic)
	p.tabs[tabQueries].roots = buildQueryFiles(p.files, p.ic)
	p.tabs[tabHistory].roots = buildHistory(p.hist, p.ic)
	var cmds []tea.Cmd
	for i := range p.tabs {
		t := &p.tabs[i]
		for _, r := range t.roots {
			cmds = append(cmds, p.restore(r)...)
		}
		restoreSelection(t)
	}
	p.tryPendingReveal()
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

func restoreSelection(t *tabState) {
	rows := visible(t.roots)
	for i, n := range rows {
		if n.id == t.selID {
			t.sel = i
			return
		}
	}
	// the nearest ancestor still visible
	id := t.selID
	for id != "" {
		k := strings.LastIndexByte(id, '/')
		if k < 0 {
			break
		}
		id = id[:k]
		for i, n := range rows {
			if n.id == id {
				t.sel = i
				return
			}
		}
	}
	if t.sel >= len(rows) {
		t.sel = max(len(rows)-1, 0)
	}
}

// ensureChildren fills a lazy node from the caches, or starts loading.
func (p *Panel) ensureChildren(n *node) tea.Cmd {
	if !n.lazy || n.loaded {
		return nil
	}
	if n.kind == nHistory {
		if vs, ok := p.versions[n.run.Lineage]; ok {
			fillVersions(n, vs, p.ic)
			return nil
		}
		if len(n.children) == 0 {
			n.add(&node{kind: nInfo, label: "loading…"})
		}
		if n.loading {
			return nil
		}
		n.loading = true
		lineage, be := n.run.Lineage, p.be
		return func() tea.Msg { vs, err := be.Versions(lineage); return versionsMsg{lineage, vs, err} }
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

func (p *Panel) rows() []*node { return visible(p.tab().roots) }

func (p *Panel) selected() *node {
	rows := p.rows()
	t := p.tab()
	if t.sel < 0 || t.sel >= len(rows) {
		return nil
	}
	return rows[t.sel]
}

func (p *Panel) setExpanded(n *node, e bool) tea.Cmd {
	n.expanded = e
	p.expanded[n.id] = e
	if e {
		return p.ensureChildren(n)
	}
	return nil
}

func (p *Panel) switchTab(i int) {
	if i < 0 || i >= numMainTabs {
		return
	}
	p.cur = i
	p.pscroll = 0
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
		}
		var cmds []tea.Cmd
		if p.search {
			cmds = append(cmds, p.runSearch())
		}
		cmds = append(cmds, p.rebuild()...)
		return p, tea.Batch(append(cmds, p.previewCmd())...)
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
	case versionsMsg:
		if msg.err != nil {
			p.flash(msg.err.Error(), true)
			msg.vs = nil
		}
		p.versions[msg.lineage] = msg.vs
		return p, tea.Batch(p.rebuild()...)
	case allColsMsg:
		p.allColsLoading = false
		if msg.err != nil {
			p.flash(msg.err.Error(), true)
			return p, nil
		}
		p.allCols = msg.cols
		if p.search {
			return p, p.runSearch()
		}
		return p, nil
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
		if msg.tok != p.sampleTok || p.ptab != ptSample {
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
	case formSavedMsg:
		if p.form == nil {
			return p, nil
		}
		p.form.busy = false
		if msg.err != nil {
			p.form.status, p.form.err = msg.err.Error(), true
			return p, nil
		}
		p.form = nil
		p.flash("saved "+msg.alias, false)
		alias := msg.alias
		p.switchTab(tabConns)
		p.pendingReveal = func(n *node) bool { return n.kind == nConnItem && n.conn.Alias == alias }
		return p, p.loadConns()
	case formTestedMsg:
		if p.form == nil {
			return p, nil
		}
		p.form.busy = false
		if msg.err != nil {
			p.form.status, p.form.err = "connection failed: "+msg.err.Error(), true
		} else {
			p.form.status, p.form.err = "connected", false
		}
		return p, nil
	case doneMsg:
		if msg.err != nil {
			p.flash(msg.err.Error(), true)
		} else if msg.popup {
			p.popup, p.popupAt = msg.text, time.Now()
			at := p.popupAt
			return p, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return popupDoneMsg{at} })
		} else if msg.text != "" {
			p.flash(msg.text, false)
		}
		if msg.tab > 0 && msg.err == nil {
			p.switchTab(msg.tab - 1)
		}
		if msg.reload {
			return p, p.loadCatalog()
		}
		return p, nil
	case popupDoneMsg:
		if msg.at.Equal(p.popupAt) {
			p.popup = ""
		}
		return p, nil
	case tickMsg:
		return p, tea.Batch(p.tick(), p.poll())
	case aliveMsg:
		if !msg.alive && !p.opt.Standalone {
			return p, tea.Quit
		}
		if msg.editor != p.editor || strings.Join(msg.files, "\n") != strings.Join(p.files, "\n") {
			p.editor, p.files = msg.editor, msg.files
			return p, tea.Batch(p.rebuild()...)
		}
		return p, nil
	case tea.MouseMsg:
		return p, p.onMouse(msg)
	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste && !p.search && p.form == nil {
			// keys arriving together come as one message ("jj"): one at a time
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				cmds = append(cmds, p.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}))
			}
			return p, tea.Batch(cmds...)
		}
		return p, p.onKey(msg)
	}
	if p.form != nil { // the text inputs' cursor blink
		if f := p.form.focused(); f != nil && f.choices == nil {
			var cmd tea.Cmd
			f.input, cmd = f.input.Update(msg)
			return p, cmd
		}
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
		for i := range p.tabs {
			walkAll(p.tabs[i].roots, func(n *node) {
				if n.lazy && n.kind != nHistory {
					n.loaded, n.loading = false, false
				}
			})
		}
		// the background load sends several events: reload once they settle
		p.reloadAt = time.Now().Add(250 * time.Millisecond)
		cmds := []tea.Cmd{func() tea.Msg { time.Sleep(300 * time.Millisecond); return reloadMsg{} }}
		if ev.Kind == control.EventConnection {
			cmds = append(cmds, p.loadContext(), p.loadConns())
		}
		return tea.Batch(cmds...)
	case control.EventConfig:
		return p.loadConns()
	case control.EventRun: // a query may have changed the data
		p.samples = map[string]sampleEntry{}
		return nil
	case control.EventHistory:
		p.versions = map[string][]history.Entry{}
		for _, n := range p.tabs[tabHistory].roots {
			if n.lazy {
				n.loaded, n.loading = false, false
			}
		}
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
	switch p.ptab {
	case ptDDL:
		return p.loadDDL(table)
	case ptSample:
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
	if p.form != nil {
		return p.formKey(k)
	}
	if p.confirm != nil {
		c := p.confirm
		p.confirm = nil
		if key == "y" || key == "Y" {
			return c.yes()
		}
		p.flash("cancelled", false)
		return nil
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
	return p.do(p.km.action(scopeMain, key))
}

// do runs a main action on the selection.
func (p *Panel) do(action string) tea.Cmd {
	t := p.tab()
	rows := p.rows()
	n := p.selected()
	move := func(d int) tea.Cmd {
		t.sel = clamp(t.sel+d, 0, len(rows)-1)
		p.pscroll = 0
		p.row = 0
		return p.previewCmd()
	}
	if strings.HasPrefix(action, "tab") && len(action) == 4 {
		i := int(action[3] - '1')
		if i < numMainTabs {
			p.switchTab(i)
			return p.previewCmd()
		}
		return nil
	}
	switch action {
	case "backToEditor":
		be := p.be
		return func() tea.Msg { return doneMsg{err: be.BackToEditor()} }
	case "quit":
		return tea.Quit
	case "down":
		return move(1)
	case "up":
		return move(-1)
	case "top":
		return move(-len(rows))
	case "bottom":
		return move(len(rows))
	case "pageDown":
		return move(max(p.treeHeight()/2, 1))
	case "pageUp":
		return move(-max(p.treeHeight()/2, 1))
	case "expand":
		if n == nil {
			return nil
		}
		if n.expandable() && !n.expanded {
			return tea.Batch(p.setExpanded(n, true), p.previewCmd())
		}
		if n.expanded && len(n.children) > 0 {
			return move(1)
		}
	case "collapse":
		if n == nil {
			return nil
		}
		if n.expanded {
			return p.setExpanded(n, false)
		}
		if n.parent != nil {
			for i, r := range rows {
				if r == n.parent {
					t.sel = i
					break
				}
			}
			return p.previewCmd()
		}
	case "open":
		return p.open(n)
	case "mark":
		if n != nil && n.kind == nColumn {
			p.marked[n.id] = !p.marked[n.id]
			return move(1)
		}
		if n != nil && n.expandable() {
			return p.setExpanded(n, !n.expanded)
		}
	case "insert":
		return p.insertNode(n)
	case "copy":
		if n != nil {
			return p.copyCmd(copyText(n))
		}
	case "search":
		p.search, p.query, p.rsel, p.rtop = true, "", 0, 0
		return p.runSearch()
	case "actions":
		p.openMenu(n)
	case "previewNext":
		p.ptab = (p.ptab + 1) % numPreviewTabs
		p.pscroll = 0
		return p.previewCmd()
	case "previewPrev":
		p.ptab = (p.ptab + numPreviewTabs - 1) % numPreviewTabs
		p.pscroll = 0
		return p.previewCmd()
	case "sample":
		return p.sampleNow()
	case "nextRow":
		p.row++
		p.pscroll = 0
	case "prevRow":
		p.row = max(p.row-1, 0)
		p.pscroll = 0
	case "previewDown":
		p.pscroll++
	case "previewUp":
		p.pscroll = max(p.pscroll-1, 0)
	case "openResult":
		if n != nil && n.kind == nHistory {
			return p.openCmd(n.run.Result)
		}
	case "newQuery":
		return p.newQueryCmd()
	case "editFile":
		if n != nil && n.kind == nHistory && n.run.File != "" {
			return p.openFileCmd(n.run.File)
		}
	case "newConnection":
		return p.newConnection()
	case "editConnection":
		if n != nil && n.kind == nConnItem {
			return p.editConnection(n.conn.Alias, false)
		}
	case "duplicateConnection":
		if n != nil && n.kind == nConnItem {
			return p.editConnection(n.conn.Alias, true)
		}
	case "deleteConnection":
		if n != nil && n.kind == nConnItem {
			p.deleteConnection(n.conn)
		}
	case "refresh":
		be := p.be
		p.flash("refreshing…", false)
		return func() tea.Msg {
			if err := be.Refresh(); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: "schema cache refreshed"}
		}
	case "help":
		p.help = true
	}
	return nil
}

// sampleNow loads sample rows of the selection (a column: its most common
// values), whatever the policy.
func (p *Panel) sampleNow() tea.Cmd {
	n := p.previewNode()
	if n == nil || previewTable(n) == "" {
		return nil
	}
	if p.samplePolicy() == "off" {
		p.flash("sample rows are off for this connection (sample: off)", true)
		return nil
	}
	p.ptab = ptSample
	table, col := previewTable(n), ""
	if n.kind == nColumn {
		col = n.col.Insert
	}
	delete(p.samples, sampleKey(table, col)) // again: fresh
	return tea.Batch(p.loadTable(table), p.loadSample(table, col))
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

// copyText: what y copies.
func copyText(n *node) string {
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
	case nQueryFile:
		return n.file
	}
	return n.label
}

// open is Enter: navigation only.
func (p *Panel) open(n *node) tea.Cmd {
	if n == nil {
		return nil
	}
	switch n.kind {
	case nConnItem:
		return p.connectCmd(n.conn.Alias)
	case nQueryFile:
		return p.openFileCmd(n.file)
	}
	if n.expandable() {
		return tea.Batch(p.setExpanded(n, !n.expanded), p.previewCmd())
	}
	switch n.kind {
	case nColumn, nObject, nHistory:
		p.flash(p.km.label("insert")+": insert · "+p.km.label("copy")+": copy · "+p.km.label("actions")+": actions", false)
	}
	return nil
}

// insertNode is ctrl+y: insert at the editor's cursor.
func (p *Panel) insertNode(n *node) tea.Cmd {
	if n == nil {
		return nil
	}
	switch n.kind {
	case nTable, nStmtTable:
		return p.insert(control.PanelAction{Kind: control.ActionTable, Table: n.table}, n.table)
	case nRelation:
		return p.insert(control.PanelAction{Kind: control.ActionTable, Table: n.table, Anchor: n.anchor, Path: n.path}, n.table)
	case nColumn:
		cols := p.markedColumns(n)
		return p.insert(control.PanelAction{Kind: control.ActionColumns, Table: n.table, Columns: cols}, strings.Join(cols, ", "))
	case nObject:
		return p.insert(control.PanelAction{Kind: control.ActionText, Text: n.obj.Insert}, n.obj.Insert)
	case nHistory:
		return p.insert(control.PanelAction{Kind: control.ActionText, Text: n.run.SQL}, n.run.SQL)
	case nConnItem, nSchema, nQueryFile:
		return p.copyCmd(copyText(n))
	}
	return nil
}

// insert hands an action to the editor; without one it copies the text.
func (p *Panel) insert(a control.PanelAction, text string) tea.Cmd {
	if p.editor == "" && p.opt.Standalone {
		return p.copyCmd(text)
	}
	be := p.be
	what := "insert " + firstLine(text)
	if a.Kind == control.ActionReplace {
		what = "replaced the statement"
	}
	return func() tea.Msg {
		if err := be.Act(a); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: what}
	}
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

func (p *Panel) openFileCmd(path string) tea.Cmd {
	be := p.be
	return func() tea.Msg {
		if err := be.OpenFile(path); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "opened " + shortPath(path)}
	}
}

// newQueryCmd creates the next query file (a SELECT of the highlighted
// table, if any) and opens it.
func (p *Panel) newQueryCmd() tea.Cmd {
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
	be := p.be
	return func() tea.Msg {
		path, err := be.NewQuery(body)
		if err != nil {
			return doneMsg{err: err}
		}
		if err := be.OpenFile(path); err != nil {
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
		return doneMsg{text: "copied " + firstLine(text), popup: true}
	}
}

// connectCmd switches to a connection and shows its objects.
func (p *Panel) connectCmd(alias string) tea.Cmd {
	be := p.be
	p.flash("connecting to "+alias+"…", false)
	return func() tea.Msg {
		if err := be.Connect(alias); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "connected to " + alias, reload: true, tab: tabObjects + 1}
	}
}

// ---- connections --------------------------------------------------------------

func (p *Panel) newConnection() tea.Cmd {
	after := ""
	if n := p.selected(); n != nil && n.kind == nConnItem && p.cur == tabConns {
		after = n.conn.Alias
	}
	f := map[string]string{}
	if n := p.selected(); n != nil && p.cur == tabConns {
		switch {
		case n.kind == nConnGroup:
			f["group"] = n.label
		case n.kind == nConnItem:
			f["group"] = n.conn.Group
		}
	}
	p.form = newConnForm("New connection", "", after, f)
	return nil
}

// editConnection opens the form on a connection (dup: on a copy of it).
func (p *Panel) editConnection(alias string, dup bool) tea.Cmd {
	f, err := p.be.ConnectionFields(alias)
	if err != nil {
		p.flash(err.Error(), true)
		return nil
	}
	if dup {
		f["alias"] = p.freeAlias(alias + "_copy")
		p.form = newConnForm("Duplicate of "+alias, "", alias, f)
		return nil
	}
	p.form = newConnForm("Edit "+alias, alias, "", f)
	return nil
}

func (p *Panel) freeAlias(base string) string {
	taken := map[string]bool{}
	for _, c := range p.conns {
		taken[c.Alias] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if a := fmt.Sprintf("%s%d", base, i); !taken[a] {
			return a
		}
	}
}

func (p *Panel) deleteConnection(c control.Connection) {
	if c.Current {
		p.flash("that is the current connection: connect to another one first", true)
		return
	}
	alias, be := c.Alias, p.be
	p.confirm = &confirm{question: "Delete connection " + alias + " from the config? y/n", yes: func() tea.Cmd {
		return func() tea.Msg {
			if err := be.DeleteConnection(alias); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: "deleted " + alias}
		}
	}}
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
		add("i", "insert (join after JOIN)", func() tea.Cmd { return p.insert(base, table) })
		name := base
		name.Mode = "name"
		add("n", "insert the name", func() tea.Cmd { return p.insert(name, table) })
		join := base
		join.Mode = "join"
		add("j", "insert a join", func() tea.Cmd { return p.insert(join, table) })
		sel := kw("SELECT") + " *\n" + kw("FROM") + " " + table
		add("s", "insert SELECT * FROM "+table, func() tea.Cmd {
			return p.insert(control.PanelAction{Kind: control.ActionText, Text: sel}, sel)
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
			return p.insert(control.PanelAction{Kind: control.ActionColumns, Table: table, Columns: names}, strings.Join(names, ", "))
		})
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(table) })
		add("q", "new query file: SELECT * FROM "+table, func() tea.Cmd { return p.newQueryCmd() })
		add("d", "show the DDL", func() tea.Cmd { p.ptab = ptDDL; p.pscroll = 0; return p.previewCmd() })
		add("S", "show a sample row", func() tea.Cmd { return p.sampleNow() })
	case nColumn:
		add("i", "insert", func() tea.Cmd { return p.insertNode(n) })
		add("s", "most common values", func() tea.Cmd { return p.sampleNow() })
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(n.col.Insert) })
	case nObject:
		add("i", "insert the name", func() tea.Cmd { return p.insertNode(n) })
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(n.obj.Insert) })
	case nSchema:
		add("y", "copy the name", func() tea.Cmd { return p.copyCmd(n.schema) })
	case nConnItem:
		alias := n.conn.Alias
		add("c", "connect", func() tea.Cmd { return p.connectCmd(alias) })
		add("e", "edit", func() tea.Cmd { return p.editConnection(alias, false) })
		add("d", "duplicate", func() tea.Cmd { return p.editConnection(alias, true) })
		add("n", "new connection", func() tea.Cmd { return p.newConnection() })
		add("x", "delete", func() tea.Cmd { p.deleteConnection(n.conn); return nil })
		add("y", "copy the alias", func() tea.Cmd { return p.copyCmd(alias) })
	case nConnGroup:
		add("n", "new connection in "+n.label, func() tea.Cmd { return p.newConnection() })
	case nQueryFile:
		file := n.file
		add("o", "open in the editor", func() tea.Cmd { return p.openFileCmd(file) })
		add("y", "copy the path", func() tea.Cmd { return p.copyCmd(file) })
		add("n", "new query file", func() tea.Cmd { return p.newQueryCmd() })
	case nHistory:
		e := n.run
		if p.cursor != nil {
			add("r", "replace the statement at the cursor with this", func() tea.Cmd {
				if p.hist != nil && e.SQL == p.hist.Statement {
					return func() tea.Msg { return doneMsg{text: "that is the current text"} }
				}
				return p.insert(control.PanelAction{Kind: control.ActionReplace, Text: e.SQL}, e.SQL)
			})
		}
		add("i", "insert this query at the cursor", func() tea.Cmd {
			return p.insert(control.PanelAction{Kind: control.ActionText, Text: e.SQL}, e.SQL)
		})
		add("y", "copy the SQL", func() tea.Cmd { return p.copyCmd(e.SQL) })
		if e.Result != "" {
			add("o", "open the result", func() tea.Cmd { return p.openCmd(e.Result) })
		}
		if e.File != "" {
			add("e", "open its file in the editor", func() tea.Cmd { return p.openFileCmd(e.File) })
		}
	}
	switch n.kind {
	case nTable, nRelation, nStmtTable, nColumn, nObject, nSchema, nGroup:
		add("r", "refresh the schema cache", func() tea.Cmd { return p.do("refresh") })
	}
	if len(items) == 0 {
		return
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
	t := p.tab()
	switch m.Button {
	case tea.MouseButtonWheelUp:
		t.sel = clamp(t.sel-3, 0, len(p.rows())-1)
		return p.previewCmd()
	case tea.MouseButtonWheelDown:
		t.sel = clamp(t.sel+3, 0, len(p.rows())-1)
		return p.previewCmd()
	case tea.MouseButtonLeft:
		if m.Y == 1 { // the tab bar
			for i, r := range p.tabBarRanges() {
				if m.X >= r[0] && m.X < r[1] {
					p.switchTab(i)
					return p.previewCmd()
				}
			}
		}
	}
	return nil
}
