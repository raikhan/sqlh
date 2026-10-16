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

func (p *Panel) bodyHeight() int { return max(p.height-2, 3) }

func (p *Panel) treeHeight() int {
	if p.wide() {
		return p.bodyHeight()
	}
	return max(p.bodyHeight()*55/100, 3)
}

// View renders the panel.
func (p *Panel) View() string {
	if p.width <= 0 || p.height <= 0 {
		// before the terminal's size is known: a frame of a guessed size
		// taller than the pane scrolls it and leaves stale rows behind
		return ""
	}
	var lines []string
	lines = append(lines, p.viewHeader())
	bodyH := p.bodyHeight()
	var body []string
	if p.wide() {
		tw := p.width * 45 / 100
		pw := p.width - tw - 1
		tree := p.viewTree(tw, bodyH)
		prev := p.viewPreview(pw, bodyH-1)
		prev = append([]string{p.viewTabs(pw)}, prev...)
		for i := 0; i < bodyH; i++ {
			body = append(body, tree[i]+stBorder.Render("│")+prev[i])
		}
	} else {
		th := p.treeHeight()
		ph := bodyH - th - 1
		body = append(body, p.viewTree(p.width, th)...)
		body = append(body, p.viewTabs(p.width))
		body = append(body, p.viewPreview(p.width, ph)...)
	}
	switch {
	case p.help:
		body = overlay(body, p.viewHelp(), p.width)
	case p.menu != nil:
		body = overlay(body, p.viewMenu(), p.width)
	}
	lines = append(lines, body...)
	lines = append(lines, p.viewFooter())
	return strings.Join(lines, "\n")
}

func (p *Panel) viewHeader() string {
	var left string
	if p.cat != nil {
		title := p.cat.Connection
		if p.cat.Schema != "" {
			title += " › " + p.cat.Schema
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
	if p.workspace() != nil {
		if p.editor != "" {
			left += stDim.Render("  editor: " + p.editor)
		} else {
			left += stDim.Render("  no editor · n: new query")
		}
	}
	return fit(left, p.width)
}

func (p *Panel) viewTabs(w int) string {
	var parts []string
	for i, name := range tabNames {
		if i == p.tab {
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
	p.sel = clamp(p.sel, 0, len(rows)-1)
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+h {
		p.top = p.sel - h + 1
	}
	p.top = clamp(p.top, 0, max(len(rows)-h, 0))
	out := make([]string, 0, h)
	for i := p.top; i < len(rows) && len(out) < h; i++ {
		out = append(out, p.viewRow(rows[i], i == p.sel, w))
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
	if sel {
		text := indent + arrow + " " + icon + label
		if n.detail != "" {
			text += "  " + n.detail
		}
		return stSel.Render(fit(text, w))
	}
	var styled string
	switch {
	case n.kind == nInfo:
		styled = stDim.Render(label)
	case n.kind == nColumn && p.marked[n.id]:
		styled = stMarked.Render(label)
	case n.kind == nStatement || n.kind == nConnection || n.kind == nConnections:
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
	return fit(text, w)
}

func (p *Panel) viewSearch(w, h int) []string {
	out := []string{fit(stKey.Render("/ ")+p.query+stAccent.Render("▏")+stDim.Render(fmt.Sprintf("  %d", len(p.results))), w)}
	lh := h - 1
	p.rsel = clamp(p.rsel, 0, len(p.results)-1)
	if p.rsel < p.rtop {
		p.rtop = p.rsel
	}
	if p.rsel >= p.rtop+lh {
		p.rtop = p.rsel - lh + 1
	}
	for i := p.rtop; i < len(p.results) && len(out) < h; i++ {
		m := p.results[i]
		it := p.items[m.index]
		icon := p.ic.kind(it.obj.Kind) + " "
		detail := it.detail
		if it.schema != "" {
			detail = it.schema + " · " + detail
		}
		if i == p.rsel {
			out = append(out, stSel.Render(fit(" "+icon+it.text+"  "+detail, w)))
			continue
		}
		out = append(out, fit(" "+stAccent.Render(icon)+highlight(it.text, m.pos)+"  "+stDim.Render(detail), w))
	}
	if len(p.results) == 0 {
		out = append(out, fit(stDim.Render(" no match"), w))
	}
	for len(out) < h {
		out = append(out, strings.Repeat(" ", w))
	}
	return out
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
		if isTableKind(it.obj.Kind) {
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
	if n.kind == nHistory { // the same on every tab
		return p.historyLines(n, w)
	}
	switch p.tab {
	case tabDDL:
		return p.ddlLines(n, w)
	case tabSample:
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
	case nConnItem:
		c := n.conn
		lines := []string{stBold.Render(c.Alias) + "  " + envStyle(c.Environment).Render(c.Environment), "", c.Driver + "  " + c.Desc}
		if c.Current {
			lines = append(lines, "", stDim.Render("current connection"))
		} else {
			lines = append(lines, "", stDim.Render("enter / u: connect"))
		}
		return lines
	case nSchema:
		lines := []string{stBold.Render(n.schema) + "  " + stDim.Render("schema")}
		for _, c := range n.children {
			if c.kind == nGroup {
				lines = append(lines, "  "+c.label+"  "+stDim.Render(c.detail))
			}
		}
		if n.detail != "current" {
			lines = append(lines, "", stDim.Render("u: use this schema (one of the connection's schemas:)"))
		}
		return lines
	case nStatement:
		if p.cursor == nil {
			return []string{stDim.Render("no cursor yet")}
		}
		lines := []string{stBold.Render(shortPath(p.cursor.File)) + fmt.Sprintf(":%d:%d", p.cursor.Line, p.cursor.Col)}
		if p.ctx != nil {
			if p.ctx.InJoin {
				lines = append(lines, stDim.Render("after JOIN: Enter on a related table inserts the join"))
			}
			for _, t := range p.ctx.Tables {
				lines = append(lines, "  "+t.Ref+"  "+stDim.Render(t.Name))
			}
		}
		return lines
	case nQueryFile:
		return []string{stBold.Render(shortPath(n.file)), stDim.Render(n.file), "", stDim.Render("enter: open in the editor")}
	case nConnection:
		if p.cat != nil {
			return []string{stBold.Render(p.cat.Connection) + "  " + envStyle(p.cat.Environment).Render(p.cat.Environment),
				"", p.cat.Driver, stDim.Render("current schema: ") + p.cat.Schema}
		}
	}
	return nil
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
	e, ok := p.samples[sampleKey(table, "")]
	switch {
	case !ok:
		why := "s: load sample rows"
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
	more := ""
	if r.Truncated {
		more = "+"
	}
	head := stBold.Render(table) + "  " + stDim.Render(fmt.Sprintf("%d%s rows · %d ms", len(r.Rows), more, r.ElapsedMS))
	lines = append(lines, head)
	if len(r.Rows) == 0 {
		return append(lines, "", stDim.Render("(no rows)"))
	}
	if p.record {
		p.recordRow = clamp(p.recordRow, 0, len(r.Rows)-1)
		lines = append(lines, stDim.Render(fmt.Sprintf("row %d/%d  n/p: next/previous · v: by column", p.recordRow+1, len(r.Rows))), "")
		nameW := 4
		for _, c := range r.Columns {
			nameW = max(nameW, lipgloss.Width(c))
		}
		nameW = min(nameW, max(w/3, 8))
		for i, c := range r.Columns {
			v := stDim.Render("NULL")
			if cell := r.Rows[p.recordRow][i]; cell != nil {
				v = *cell
			}
			name := fit(c, nameW)
			if n.kind == nColumn && strings.EqualFold(c, unquote(n.col.Insert)) {
				name = stMatch.Render(name)
			}
			wrapped := strings.Split(ansi.Wrap(v, max(w-nameW-3, 10), " "), "\n")
			lines = append(lines, "  "+name+" "+wrapped[0])
			for _, more := range wrapped[1:] {
				lines = append(lines, strings.Repeat(" ", nameW+3)+more)
			}
		}
		return lines
	}
	lines = append(lines, stDim.Render("distinct values per column · v: by row"), "")
	nameW := 4
	for _, c := range r.Columns {
		nameW = max(nameW, lipgloss.Width(c))
	}
	nameW = min(nameW, max(w/3, 8))
	for i, c := range r.Columns {
		seen := map[string]bool{}
		var vals []string
		nulls := 0
		for _, row := range r.Rows {
			if row[i] == nil {
				nulls++
				continue
			}
			v := strings.ReplaceAll(*row[i], "\n", "⏎")
			if !seen[v] {
				seen[v] = true
				vals = append(vals, v)
			}
		}
		text := strings.Join(vals, stDim.Render(" · "))
		if nulls > 0 {
			if text != "" {
				text += stDim.Render(" · ")
			}
			text += stDim.Render(fmt.Sprintf("NULL×%d", nulls))
		}
		name := fit(c, nameW)
		if n.kind == nColumn && strings.EqualFold(c, unquote(n.col.Insert)) {
			name = stMatch.Render(name)
		}
		lines = append(lines, "  "+name+" "+text)
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
		lines = append(lines, stDim.Render("o: open "+shortPath(e.Result)))
	}
	lines = append(lines, "")
	if n.version && p.hist != nil && p.hist.Statement != "" && e.SQL != p.hist.Statement {
		lines = append(lines, stBold.Render("Enter replaces the statement:"))
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
	var hints [][2]string
	switch {
	case p.help:
		hints = [][2]string{{"any key", "close"}}
	case p.menu != nil:
		hints = [][2]string{{"enter", "run"}, {"esc", "close"}}
	case p.search:
		hints = [][2]string{{"enter", "insert"}, {"tab", "show in tree"}, {"esc", "back"}}
	default:
		n := p.selected()
		switch {
		case n == nil:
		case n.kind == nTable || n.kind == nRelation:
			hints = append(hints, [2]string{"enter", "insert"}, [2]string{"l", "open"})
		case n.kind == nColumn:
			hints = append(hints, [2]string{"enter", "insert"}, [2]string{"space", "mark"})
		case n.kind == nConnItem:
			hints = append(hints, [2]string{"enter", "connect"})
		case n.kind == nSchema:
			hints = append(hints, [2]string{"u", "use"})
		case n.kind == nQueryFile:
			hints = append(hints, [2]string{"enter", "edit"}, [2]string{"n", "new query"})
		case n.kind == nHistory && n.version:
			hints = append(hints, [2]string{"enter", "replace"}, [2]string{"o", "result"})
		case n.kind == nHistory:
			hints = append(hints, [2]string{"enter", "insert"}, [2]string{"o", "result"})
		case n.expandable():
			hints = append(hints, [2]string{"enter", "open"})
		}
		hints = append(hints, [2]string{"/", "search"}, [2]string{".", "actions"}, [2]string{"tab", tabNames[(p.tab+1)%numTabs]},
			[2]string{keyLabel(p.opt.BackToEditorKey), "editor"}, [2]string{"?", "help"})
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

func keyLabel(k string) string {
	if k == "backspace" {
		return "⌫"
	}
	return k
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

func (p *Panel) viewHelp() []string {
	keys := [][2]string{
		{"j/k ↑/↓", "move"}, {"h/l ←/→", "close / open"}, {"enter", "insert at the editor's cursor (or open)"},
		{"space", "mark columns (enter inserts them)"}, {"/", "search all objects"}, {". a", "actions"},
		{"tab", "Describe / DDL / Sample"}, {"s", "sample rows (a column: most common values)"},
		{"v n p", "Sample: by row, next, previous"}, {"J/K", "scroll the preview"}, {"y", "copy the name"}, {"u", "use schema / connect"},
		{"r", "refresh the schema cache"}, {"o", "History: open the run's result"},
		{"n", "sqlh tui: new query file (a SELECT of the table)"}, {"e", "sqlh tui: open a run's file"}, {keyLabel(p.opt.BackToEditorKey), "back to the editor"}, {"q", "quit"},
	}
	rows := []string{stTitle.Render("sqlh panel")}
	for _, k := range keys {
		rows = append(rows, stKey.Render(fit(k[0], 9))+" "+k[1])
	}
	return strings.Split(stMenuBox.Render(strings.Join(rows, "\n")), "\n")
}

// overlay draws box centred over body.
func overlay(body, box []string, w int) []string {
	bw := 0
	for _, l := range box {
		bw = max(bw, lipgloss.Width(l))
	}
	left := max((w-bw)/2, 0)
	top := max((len(body)-len(box))/2, 0)
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

var _ = control.Event{}
