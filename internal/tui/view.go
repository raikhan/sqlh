package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/raikhan/sqlh/internal/control"
)

// fit truncates or pads s (which may contain ANSI) to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// wide: tree and preview side by side.
func (p *Panel) wide() bool { return p.width >= 110 }

// subtitle: a line above the tree about the tab's content ("" = none).
func (p *Panel) subtitle() string {
	switch p.cur {
	case tabStmt:
		return statementTitle(p.ctx, p.cursor)
	case tabQueries:
		return "n: new query"
	}
	return ""
}

func (p *Panel) bodyHeight() int { return max(p.height-3, 3) }

func (p *Panel) treeHeight() int {
	h := p.bodyHeight()
	if !p.wide() {
		h = max(h*55/100, 3)
	}
	if p.subtitle() != "" {
		h--
	}
	return max(h, 1)
}

// View renders the panel.
func (p *Panel) View() string {
	if p.width <= 0 || p.height <= 0 {
		// before the terminal's size is known: a frame of a guessed size
		// taller than the pane scrolls it and leaves stale rows behind
		return ""
	}
	lines := []string{p.viewHeader(), p.viewTabBar()}
	bodyH := p.bodyHeight()
	var body []string
	tree := func(w, h int) []string {
		var out []string
		if sub := p.subtitle(); sub != "" && !p.search {
			out = append(out, fit(stDim.Render(" "+sub), w))
			h--
		}
		return append(out, p.viewTree(w, h)...)
	}
	if p.wide() {
		tw := p.width * 45 / 100
		pw := p.width - tw - 1
		left := tree(tw, bodyH)
		prev := append([]string{p.viewPreviewTabs(pw)}, p.viewPreview(pw, bodyH-1)...)
		for i := 0; i < bodyH; i++ {
			body = append(body, left[i]+stBorder.Render("│")+prev[i])
		}
	} else {
		th := max(bodyH*55/100, 3)
		ph := bodyH - th - 1
		body = append(body, tree(p.width, th)...)
		body = append(body, p.viewPreviewTabs(p.width))
		body = append(body, p.viewPreview(p.width, ph)...)
	}
	switch {
	case p.form != nil:
		body = overlay(body, p.viewForm(p.width, bodyH), p.width)
	case p.help:
		body = overlay(body, p.viewHelp(), p.width)
	case p.menu != nil:
		body = overlay(body, p.viewMenu(), p.width)
	case p.confirm != nil:
		body = overlay(body, strings.Split(stPopup.Render(p.confirm.question), "\n"), p.width)
	}
	if p.popup != "" {
		box := strings.Split(stPopup.Render(ansi.Truncate(p.popup, max(p.width-8, 10), "…")), "\n")
		body = overlayAt(body, box, p.width, max(len(body)-len(box)-1, 0))
	}
	lines = append(lines, body...)
	lines = append(lines, p.viewFooter())
	return strings.Join(lines, "\n")
}

func (p *Panel) viewHeader() string {
	var left string
	if p.cat != nil {
		title := p.cat.Connection
		for _, c := range p.conns {
			if c.Current {
				if t := connTarget(c); t != "" {
					title += " › " + t
				}
			}
		}
		left = stTitle.Render(p.ic.connection+" "+title) + " " + envStyle(p.cat.Environment).Render(p.cat.Environment)
		if !p.cat.ViewsLoaded {
			left += stDim.Render("  loading…")
		}
	} else {
		left = stTitle.Render("sqlh")
	}
	if p.disconnected {
		left += "  " + stErr.Render("server gone")
	}
	if p.opt.Standalone {
		if p.editor != "" {
			left += stDim.Render("  editor: " + p.editor)
		} else {
			left += stDim.Render("  no editor")
		}
	}
	return fit(left, p.width)
}

// tabBarLabels: the main tabs' labels, short when the full ones don't fit.
func (p *Panel) tabBarLabels() []string {
	labels := func(names []string) []string {
		out := make([]string, len(names))
		for i, n := range names {
			out[i] = " " + p.km.label(fmt.Sprintf("tab%d", i+1)) + " " + n + " "
		}
		return out
	}
	full := labels(mainTabNames)
	if lipgloss.Width(strings.Join(full, "")) <= p.width {
		return full
	}
	return labels(mainTabShort)
}

// tabBarRanges: the columns of each tab label (mouse).
func (p *Panel) tabBarRanges() [][2]int {
	var out [][2]int
	x := 0
	for _, l := range p.tabBarLabels() {
		w := lipgloss.Width(l)
		out = append(out, [2]int{x, x + w})
		x += w
	}
	return out
}

func (p *Panel) viewTabBar() string {
	var b strings.Builder
	for i, l := range p.tabBarLabels() {
		if i == p.cur {
			b.WriteString(stMainTabOn.Render(l))
		} else {
			b.WriteString(stMainTabOff.Render(l))
		}
	}
	return fit(b.String(), p.width)
}

func (p *Panel) viewPreviewTabs(w int) string {
	var parts []string
	for i, name := range previewTabNames {
		if i == p.ptab {
			parts = append(parts, stTabOn.Render(name))
		} else {
			parts = append(parts, stTabOff.Render(name))
		}
	}
	s := stBorder.Render("─ ") + strings.Join(parts, stBorder.Render(" │ ")) + " "
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += stBorder.Render(strings.Repeat("─", pad))
	}
	return fit(s, w)
}

func (p *Panel) viewTree(w, h int) []string {
	if p.search {
		return p.viewSearch(w, h)
	}
	rows := p.rows()
	t := p.tab()
	t.sel = clamp(t.sel, 0, len(rows)-1)
	if t.sel < t.top {
		t.top = t.sel
	}
	if t.sel >= t.top+h {
		t.top = t.sel - h + 1
	}
	t.top = clamp(t.top, 0, max(len(rows)-h, 0))
	out := make([]string, 0, h)
	for i := t.top; i < len(rows) && len(out) < h; i++ {
		out = append(out, p.viewRow(rows[i], i == t.sel, w))
	}
	for len(out) < h {
		out = append(out, strings.Repeat(" ", w))
	}
	return out
}

func (p *Panel) viewRow(n *node, sel bool, w int) string {
	indent := strings.Repeat("  ", n.depth)
	arrow := p.ic.leaf
	if n.expandable() {
		arrow = p.ic.closed
		if n.expanded {
			arrow = p.ic.open
		}
	}
	icon := n.icon
	if icon != "" {
		icon += " "
	}
	label := n.label
	if n.kind == nColumn && p.marked[n.id] {
		label = "✓ " + label
	}
	env := ""
	if n.kind == nConnItem && n.conn.Environment != "" {
		env = " " + envStyle(n.conn.Environment).Render(n.conn.Environment)
	}
	if sel {
		text := indent + arrow + " " + icon + label
		if n.detail != "" {
			text += "  " + n.detail
		}
		if env != "" {
			tw := lipgloss.Width(text) + 1
			return fit(stSel.Render(text+" ")+env+stSel.Render(strings.Repeat(" ", max(w-tw-lipgloss.Width(env), 0))), w)
		}
		return stSel.Render(fit(text, w))
	}
	var styled string
	switch {
	case n.kind == nInfo:
		styled = stDim.Render(label)
	case n.kind == nColumn && p.marked[n.id]:
		styled = stMarked.Render(label)
	case n.kind == nConnGroup || n.kind == nSchema:
		styled = stBold.Render(label)
	case n.kind == nConnItem && n.conn.Current:
		styled = stBold.Render(label)
	case n.kind == nRelation:
		styled = stRelTable.Render(label)
	default:
		styled = label
	}
	text := indent + stDim.Render(arrow) + " " + stAccent.Render(icon) + styled
	if n.detail != "" {
		text += "  " + stDim.Render(n.detail)
	}
	return fit(text+env, w)
}

func (p *Panel) viewSearch(w, h int) []string {
	kind := searchKindNames[p.skind]
	prompt := stKey.Render("/ ") + p.query + stAccent.Render("▏") + stDim.Render(fmt.Sprintf("  %s · %d", kind, len(p.results)))
	out := []string{fit(prompt, w)}
	lh := h - 2
	p.rsel = clamp(p.rsel, 0, len(p.results)-1)
	if p.rsel < p.rtop {
		p.rtop = p.rsel
	}
	if p.rsel >= p.rtop+lh {
		p.rtop = p.rsel - lh + 1
	}
	for i := p.rtop; i < len(p.results) && len(out) < h-1; i++ {
		m := p.results[i]
		it := p.items[m.index]
		icon := it.icon + " "
		text := it.text
		pos := m.pos
		if it.kind == skColumn {
			// matched against table.column when the query has a dot
			if _, q := parseSearch(p.query, p.cur); strings.Contains(q, ".") {
				prefix := unquote(lastPart(it.col.TableInsert)) + "."
				text = prefix + it.col.Name
			}
		}
		if i == p.rsel {
			out = append(out, stSel.Render(fit(" "+icon+text+"  "+it.detail, w)))
			continue
		}
		out = append(out, fit(" "+stAccent.Render(icon)+highlight(text, pos)+"  "+stDim.Render(it.detail), w))
	}
	switch {
	case len(p.results) == 0 && p.allColsLoading:
		out = append(out, fit(stDim.Render(" loading the columns…"), w))
	case len(p.results) == 0:
		out = append(out, fit(stDim.Render(" no match"), w))
	}
	for len(out) < h-1 {
		out = append(out, strings.Repeat(" ", w))
	}
	return append(out, fit(stDim.Render(" : tables  . columns  # indexes  ~ sequences  @ connections  * all"), w))
}

// highlight marks matched runes.
func highlight(s string, pos []int) string {
	if len(pos) == 0 {
		return s
	}
	at := map[int]bool{}
	for _, i := range pos {
		at[i] = true
	}
	var b strings.Builder
	for i, r := range []rune(s) {
		if at[i] {
			b.WriteString(stMatch.Render(string(r)))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (p *Panel) viewPreview(w, h int) []string {
	lines := p.previewLines(w)
	p.pscroll = clamp(p.pscroll, 0, max(len(lines)-h, 0))
	out := make([]string, 0, h)
	for i := p.pscroll; i < len(lines) && len(out) < h; i++ {
		out = append(out, fit(lines[i], w))
	}
	for len(out) < h {
		out = append(out, strings.Repeat(" ", w))
	}
	return out
}

// previewNode: the node the preview describes (the search result's while
// searching).
func (p *Panel) previewNode() *node {
	if p.search {
		it := p.searchSelected()
		if it == nil {
			return nil
		}
		switch it.kind {
		case skColumn:
			return &node{kind: nColumn, table: it.col.TableInsert, label: it.col.Insert,
				col: control.ColumnItem{Name: it.col.Name, Insert: it.col.Insert, Type: it.col.Type}}
		case skConn:
			return &node{kind: nConnItem, label: it.conn.Alias, conn: it.conn}
		case skFile:
			return &node{kind: nQueryFile, label: it.text, file: it.file}
		case skRun:
			return &node{kind: nHistory, label: it.detail, run: it.run}
		}
		if isTableKind(it.obj.Kind) && it.obj.Table == "" {
			return &node{kind: nTable, table: it.obj.Insert, label: it.obj.Name, tableKind: it.obj.Kind, schema: it.schema, obj: it.obj}
		}
		return &node{kind: nObject, label: it.obj.Name, detail: it.obj.Detail, obj: it.obj, schema: it.schema}
	}
	return p.selected()
}

func (p *Panel) previewLines(w int) []string {
	n := p.previewNode()
	if n == nil {
		return nil
	}
	switch n.kind { // the same on every preview tab
	case nHistory:
		return p.historyLines(n, w)
	case nConnItem:
		return p.connectionLines(n.conn, w)
	case nQueryFile:
		return []string{stBold.Render(shortPath(n.file)), stDim.Render(n.file), "",
			stDim.Render("enter: open in the editor · n: new query")}
	}
	switch p.ptab {
	case ptDDL:
		return p.ddlLines(n, w)
	case ptSample:
		return p.sampleLines(n, w)
	}
	switch n.kind {
	case nTable, nRelation, nStmtTable, nColumn, nGroup, nObject:
		if t := previewTable(n); t != "" {
			return p.describeLines(n, t, w)
		}
	}
	switch n.kind {
	case nObject:
		lines := []string{stBold.Render(n.obj.Name) + "  " + stDim.Render(n.obj.Kind)}
		if n.obj.Detail != "" {
			lines = append(lines, "", n.obj.Detail)
		}
		if n.obj.Table != "" {
			lines = append(lines, "", "on "+n.obj.Table)
		}
		return lines
	case nSchema:
		lines := []string{stBold.Render(n.schema) + "  " + stDim.Render("schema")}
		for _, c := range n.children {
			if c.kind == nGroup {
				lines = append(lines, "  "+c.label+"  "+stDim.Render(c.detail))
			}
		}
		return lines
	case nConnGroup:
		lines := []string{stBold.Render(n.label) + "  " + stDim.Render("group")}
		for _, c := range n.children {
			if c.kind == nConnItem {
				lines = append(lines, "  "+c.label+"  "+stDim.Render(connTarget(c.conn)))
			}
		}
		return lines
	}
	return nil
}

// connectionLines: a connection as a form, field by field.
func (p *Panel) connectionLines(c control.Connection, w int) []string {
	head := stBold.Render(c.Alias)
	if c.Environment != "" {
		head += "  " + envStyle(c.Environment).Render(c.Environment)
	}
	if c.Current {
		head += "  " + stAccent.Render("● current")
	}
	lines := []string{head, ""}
	fields := append([]control.Field(nil), c.Fields...)
	if c.Group != "" {
		fields = append(fields, control.Field{Label: "Group", Value: c.Group})
	}
	if c.Sample != "" {
		fields = append(fields, control.Field{Label: "Sample rows", Value: c.Sample})
	}
	labelW := 8
	for _, f := range fields {
		labelW = max(labelW, lipgloss.Width(f.Label))
	}
	labelW = min(labelW, max(w/3, 8))
	for _, f := range fields {
		wrapped := strings.Split(ansi.Wrap(f.Value, max(w-labelW-3, 10), " ,&"), "\n")
		lines = append(lines, "  "+stLabel.Render(fit(f.Label, labelW))+" "+wrapped[0])
		for _, more := range wrapped[1:] {
			lines = append(lines, strings.Repeat(" ", labelW+3)+more)
		}
	}
	hint := p.km.label("open") + ": connect · " + p.km.label("editConnection") + ": edit · " +
		p.km.label("duplicateConnection") + ": duplicate · " + p.km.label("deleteConnection") + ": delete · " +
		p.km.label("newConnection") + ": new"
	if c.Current {
		hint = p.km.label("editConnection") + ": edit · " + p.km.label("duplicateConnection") + ": duplicate · " +
			p.km.label("newConnection") + ": new"
	}
	return append(lines, "", stDim.Render(hint))
}

func (p *Panel) describeLines(n *node, table string, w int) []string {
	var lines []string
	if n.kind == nRelation && n.rel != nil {
		prev := n.anchor
		if len(n.path) > 1 {
			prev = n.path[len(n.path)-2].Table
		}
		var cond []string
		for _, c := range n.rel.Cols {
			cond = append(cond, stRelTable.Render(n.table)+"."+c[1]+" = "+prev+"."+c[0])
		}
		line := prev + " " + n.rel.Marker + " "
		if len(cond) > 0 {
			line += strings.Join(cond, " and ")
		} else {
			line += stRelTable.Render(n.table)
		}
		if n.rel.Note != "" {
			line += "  " + stDim.Render(n.rel.Note)
		}
		lines = append(lines, line)
		if len(n.path) > 1 {
			chain := []string{n.anchor}
			for _, hop := range n.path {
				chain = append(chain, hop.Table)
			}
			lines = append(lines, stDim.Render("chain: "+strings.Join(chain, " › ")))
		}
		lines = append(lines, "")
	}
	if p.loading[table] {
		return append(lines, stDim.Render("loading "+table+"…"))
	}
	if err := p.errs[table]; err != nil {
		return append(lines, stErr.Render(err.Error()))
	}
	cols, ok := p.cols[table]
	if !ok {
		return append(lines, stDim.Render(table))
	}
	rel := p.rels[table]
	kind := n.tableKind
	if rel != nil {
		kind = rel.Kind
		if rel.Writes != "" {
			kind += " (" + rel.Writes + ")"
		}
	}
	lines = append(lines, stBold.Render(table)+"  "+stDim.Render(kind))
	nameW := 4
	for _, c := range cols {
		nameW = max(nameW, lipgloss.Width(c.Insert))
	}
	nameW = min(nameW, max(w/2, 8))
	lines = append(lines, "")
	for _, c := range cols {
		name := fit(c.Insert, nameW)
		if n.kind == nColumn && c.Insert == n.col.Insert {
			name = stMatch.Render(name)
		}
		line := "  " + name + "  " + stDim.Render(c.Type)
		if isPK(c.Key) {
			line += "  " + stAccent.Render("PK")
		}
		lines = append(lines, line)
	}
	if rel == nil {
		return lines
	}
	if rel.Note != "" {
		lines = append(lines, "", stDim.Render("("+rel.Note+")"))
	}
	var reads, usedBy []string
	first := true
	for _, r := range rel.Relations {
		switch {
		case r.Edge == "uses" && r.Out:
			reads = append(reads, r.Insert)
		case r.Edge == "uses" && len(r.Cols) == 0:
			usedBy = append(usedBy, r.Insert)
		default:
			if first {
				lines = append(lines, "", stBold.Render("Relationships"))
				first = false
			}
			line := "  " + r.Marker + " "
			if r.Condition != "" {
				line += strings.Replace(r.Condition, r.Insert+".", stRelTable.Render(r.Insert)+".", 1)
			} else {
				line += stRelTable.Render(r.Insert)
			}
			if r.Note != "" {
				line += "  " + stDim.Render(r.Note)
			}
			lines = append(lines, line)
		}
	}
	if len(reads) > 0 {
		lines = append(lines, "", stDim.Render("Reads: ")+strings.Join(reads, ", "))
	}
	if len(usedBy) > 0 {
		lines = append(lines, "", stDim.Render("Used by views: ")+strings.Join(usedBy, ", "))
	}
	if !rel.ViewsLoaded && rel.Note == "" {
		lines = append(lines, "", stDim.Render("(views are still loading)"))
	}
	if rel.Definition != "" {
		lines = append(lines, "", stBold.Render("Definition"))
		for _, l := range strings.Split(ansi.Wrap(rel.Definition, max(w-2, 10), ""), "\n") {
			lines = append(lines, "  "+l)
		}
	}
	return lines
}

func (p *Panel) ddlLines(n *node, w int) []string {
	table := previewTable(n)
	if table == "" {
		return []string{stDim.Render("DDL: select a table or view")}
	}
	e, ok := p.ddl[table]
	switch {
	case !ok || e.loading:
		return []string{stDim.Render("loading the definition of " + table + "…")}
	case e.err != nil:
		return []string{stErr.Render(e.err.Error())}
	}
	r := e.res
	head := "-- " + r.Name + " (" + r.Kind + ")"
	if r.Source != "" {
		head += ": " + r.Source
	}
	lines := []string{stDim.Render(head)}
	if r.Note != "" {
		lines = append(lines, stDim.Render("-- "+r.Note))
	}
	for _, l := range strings.Split(strings.TrimRight(r.DDL, "\n"), "\n") {
		lines = append(lines, strings.Split(ansi.Wrap(l, max(w, 10), ""), "\n")...)
	}
	return lines
}

func (p *Panel) sampleLines(n *node, w int) []string {
	table := previewTable(n)
	if table == "" {
		return []string{stDim.Render("Sample: select a table or view")}
	}
	policy := p.samplePolicy()
	if policy == "off" {
		return []string{stDim.Render("sample rows are off for this connection (sample: off)")}
	}
	var lines []string
	if n.kind == nColumn {
		lines = append(lines, p.topValueLines(table, n.col.Insert, w)...)
	}
	sk := p.km.label("sample")
	e, ok := p.samples[sampleKey(table, "")]
	switch {
	case !ok:
		why := sk + ": load a sample row"
		switch {
		case policy == "manual" && p.cat != nil && p.cat.Environment == "prod":
			why += " (on request on prod)"
		case policy == "manual":
			why += " (on request: sample: manual)"
		case !autoSample(n):
			why += " (views on request)"
		}
		return append(lines, stDim.Render(why))
	case e.loading:
		return append(lines, stDim.Render("sampling "+table+"…"))
	case e.err != nil:
		return append(lines, stErr.Render(e.err.Error()))
	}
	r := e.res
	if len(r.Rows) == 0 {
		return append(lines, stBold.Render(table)+"  "+stDim.Render("(no rows)"))
	}
	p.row = clamp(p.row, 0, len(r.Rows)-1)
	more := ""
	if r.Truncated {
		more = "+"
	}
	lines = append(lines, stBold.Render(table)+"  "+stDim.Render(fmt.Sprintf("row %d of %d%s · %s/%s: next/previous",
		p.row+1, len(r.Rows), more, p.km.label("nextRow"), p.km.label("prevRow"))), "")
	nameW := 4
	for _, c := range r.Columns {
		nameW = max(nameW, lipgloss.Width(c))
	}
	nameW = min(nameW, max(w/3, 8))
	for i, c := range r.Columns {
		v := stDim.Render("NULL")
		if cell := r.Rows[p.row][i]; cell != nil {
			v = *cell
		}
		name := stLabel.Render(fit(c, nameW))
		if n.kind == nColumn && strings.EqualFold(c, unquote(n.col.Insert)) {
			name = stMatch.Render(fit(c, nameW))
		}
		wrapped := strings.Split(ansi.Wrap(v, max(w-nameW-3, 10), " "), "\n")
		lines = append(lines, "  "+name+" "+wrapped[0])
		for _, more := range wrapped[1:] {
			lines = append(lines, strings.Repeat(" ", nameW+3)+more)
		}
	}
	return lines
}

// historyLines describes a past run: when, where, how it went; for a
// version of the statement at the cursor, what Enter would change.
func (p *Panel) historyLines(n *node, w int) []string {
	e := n.run
	where := e.Conn
	if e.Schema != "" {
		where += " › " + e.Schema
	}
	lines := []string{stBold.Render(e.Time.Local().Format("Mon Jan 2 15:04:05")) + "  " + where + "  " + envStyle(e.Env).Render(e.Env)}
	var facts []string
	switch e.Status {
	case "ok":
		if e.Affected >= 0 {
			facts = append(facts, plural(e.Affected, "row")+" affected")
		} else {
			more := ""
			if e.Truncated {
				more = "+"
			}
			facts = append(facts, plural(e.Rows, "row")+more)
		}
	default:
		facts = append(facts, stErr.Render(e.Status))
	}
	facts = append(facts, fmt.Sprintf("%d ms", e.ElapsedMS), e.Kind)
	if e.File != "" {
		facts = append(facts, fmt.Sprintf("%s:%d", shortPath(e.File), e.Line))
	}
	lines = append(lines, stDim.Render(strings.Join(facts, " · ")))
	if e.Error != "" {
		for _, l := range strings.Split(ansi.Wrap(e.Error, max(w, 10), " "), "\n") {
			lines = append(lines, stErr.Render(l))
		}
	}
	if e.Result != "" {
		lines = append(lines, stDim.Render(p.km.label("openResult")+": open "+shortPath(e.Result)))
	}
	lines = append(lines, "")
	if n.version && p.hist != nil && p.hist.Statement != "" && e.SQL != p.hist.Statement {
		lines = append(lines, stBold.Render("against the statement at the cursor")+stDim.Render(" (. r: replace it)"))
		for _, d := range lineDiff(p.hist.Statement, e.SQL) {
			switch d[0] {
			case '-':
				lines = append(lines, stDel.Render(d))
			case '+':
				lines = append(lines, stAdd.Render(d))
			default:
				lines = append(lines, stDim.Render(d))
			}
		}
		return lines
	}
	for _, l := range strings.Split(e.SQL, "\n") {
		lines = append(lines, strings.Split(ansi.Wrap(l, max(w, 10), " "), "\n")...)
	}
	return lines
}

// lineDiff: the lines of a turned into b ("- " removed, "+ " added, "  "
// kept), from their longest common subsequence.
func lineDiff(a, b string) []string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			out = append(out, "  "+x[i])
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] > lcs[i+1][j]):
			out = append(out, "+ "+y[j])
			j++
		default:
			out = append(out, "- "+x[i])
			i++
		}
	}
	return out
}

// topValueLines: the most common values of a column, when loaded.
func (p *Panel) topValueLines(table, column string, w int) []string {
	e, ok := p.samples[sampleKey(table, column)]
	switch {
	case !ok:
		return []string{stDim.Render("s: most common values of " + column), ""}
	case e.loading:
		return []string{stDim.Render("counting " + column + "…"), ""}
	case e.err != nil:
		return []string{stErr.Render(e.err.Error()), ""}
	}
	lines := []string{stBold.Render(column) + "  " + stDim.Render("most common values")}
	for _, row := range e.res.Rows {
		v := stDim.Render("NULL")
		if row[0] != nil {
			v = *row[0]
		}
		cnt := ""
		if len(row) > 1 && row[1] != nil {
			cnt = *row[1]
		}
		lines = append(lines, "  "+stAccent.Render(fit(cnt, 8))+" "+v)
	}
	if e.res.Truncated {
		lines = append(lines, stDim.Render("  …"))
	}
	return append(lines, "")
}

func (p *Panel) viewFooter() string {
	if p.msg != "" && time.Since(p.msgAt) < 4*time.Second {
		if p.msgErr {
			return fit(stErr.Render(p.msg), p.width)
		}
		return fit(stAccent.Render(p.msg), p.width)
	}
	k := p.km.label
	var hints [][2]string
	switch {
	case p.form != nil:
		hints = [][2]string{{k("formSave"), "save"}, {k("formTest"), "test"}, {k("formCancel"), "cancel"}}
	case p.confirm != nil:
		hints = [][2]string{{"y", "yes"}, {"n", "no"}}
	case p.help:
		hints = [][2]string{{"any key", "close"}}
	case p.menu != nil:
		hints = [][2]string{{"enter", "run"}, {"esc", "close"}}
	case p.search:
		hints = [][2]string{{k("searchOpen"), "go to"}, {k("searchInsert"), "insert"}, {k("searchCancel"), "back"}}
	default:
		n := p.selected()
		switch {
		case n == nil:
		case n.kind == nTable || n.kind == nRelation || n.kind == nStmtTable:
			hints = append(hints, [2]string{k("insert"), "insert"}, [2]string{k("open"), "open"})
		case n.kind == nColumn:
			hints = append(hints, [2]string{k("insert"), "insert"}, [2]string{k("mark"), "mark"})
		case n.kind == nConnItem:
			hints = append(hints, [2]string{k("open"), "connect"}, [2]string{k("editConnection"), "edit"})
		case n.kind == nQueryFile:
			hints = append(hints, [2]string{k("open"), "edit"}, [2]string{k("newQuery"), "new query"})
		case n.kind == nHistory && n.head:
			hints = append(hints, [2]string{k("open"), "versions"}, [2]string{k("insert"), "insert"})
		case n.kind == nHistory:
			hints = append(hints, [2]string{k("insert"), "insert"}, [2]string{k("openResult"), "result"})
		case n.expandable():
			hints = append(hints, [2]string{k("open"), "open"})
		}
		hints = append(hints, [2]string{k("search"), "search"}, [2]string{k("actions"), "actions"},
			[2]string{k("previewNext"), previewTabNames[(p.ptab+1)%numPreviewTabs]})
		if !p.opt.Standalone || p.editor != "" {
			hints = append(hints, [2]string{k("backToEditor"), "editor"})
		}
		hints = append(hints, [2]string{k("help"), "help"})
	}
	var parts []string
	for _, h := range hints {
		parts = append(parts, stKey.Render(h[0])+" "+stDim.Render(h[1]))
	}
	return fit(strings.Join(parts, stDim.Render(" · ")), p.width)
}

func plural(n int64, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func (p *Panel) viewMenu() []string {
	m := p.menu
	var rows []string
	for i, it := range m.items {
		row := stKey.Render(it.key) + "  " + it.label
		if i == m.sel {
			row = stMenuSel.Render(it.key + "  " + it.label)
		}
		rows = append(rows, row)
	}
	title := stTitle.Render(ansi.Truncate(m.title, max(p.width-10, 10), "…"))
	return strings.Split(stMenuBox.Render(title+"\n"+strings.Join(rows, "\n")), "\n")
}

// helpRows: the help screen, from the keymap.
var helpRows = [][2]string{
	{"tab1 tab2 tab3 tab4 tab5", "Connections · Objects · Statement · Queries · History"},
	{"up down", "move"}, {"collapse expand", "close / open"},
	{"open", "open, connect, open the file (navigation)"},
	{"insert", "insert at the editor's cursor (no editor: copy)"},
	{"copy", "copy the name"}, {"mark", "mark columns (insert them together)"},
	{"actions", "everything for the selection"},
	{"search", "search (:tables .columns #indexes ~sequences @connections *all)"},
	{"previewNext previewPrev", "Describe / DDL / Sample"}, {"previewDown previewUp", "scroll the preview"},
	{"sample", "sample row (a column: most common values)"}, {"nextRow prevRow", "Sample: next / previous row"},
	{"newQuery", "new query file (SELECT of the table)"}, {"editFile", "History: open the run's file"},
	{"openResult", "History: open the run's result"},
	{"newConnection editConnection duplicateConnection deleteConnection", "connections: new, edit, duplicate, delete"},
	{"refresh", "refresh the schema cache"}, {"backToEditor", "back to the editor"}, {"quit", "quit"},
}

func (p *Panel) viewHelp() []string {
	rows := []string{stTitle.Render("sqlh panel")}
	keyW := 0
	var keys []string
	for _, h := range helpRows {
		var ks []string
		for _, a := range strings.Fields(h[0]) {
			ks = append(ks, p.km.label(a))
		}
		s := strings.Join(ks, " ")
		keys = append(keys, s)
		keyW = max(keyW, lipgloss.Width(s))
	}
	keyW = min(keyW, 12)
	for i, h := range helpRows {
		rows = append(rows, stKey.Render(fit(keys[i], keyW))+" "+h[1])
	}
	return strings.Split(stMenuBox.Render(strings.Join(rows, "\n")), "\n")
}

// overlay draws box centred over body.
func overlay(body, box []string, w int) []string {
	return overlayAt(body, box, w, max((len(body)-len(box))/2, 0))
}

// overlayAt draws box centred horizontally at row top.
func overlayAt(body, box []string, w, top int) []string {
	bw := 0
	for _, l := range box {
		bw = max(bw, lipgloss.Width(l))
	}
	left := max((w-bw)/2, 0)
	out := append([]string(nil), body...)
	for i, l := range box {
		if top+i >= len(out) {
			break
		}
		row := out[top+i]
		prefix := ansi.Truncate(row, left, "")
		if pad := left - lipgloss.Width(prefix); pad > 0 {
			prefix += strings.Repeat(" ", pad)
		}
		rest := ansi.TruncateLeft(row, left+lipgloss.Width(l), "")
		out[top+i] = fit(prefix+l+rest, w)
	}
	return out
}
