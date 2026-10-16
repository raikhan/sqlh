package tui

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/history"
)

// Search kinds. The search finds the current tab's kind; a leading
// character picks another.
const (
	skTable = iota
	skColumn
	skIndex
	skSequence
	skConn
	skFile
	skRun
	skAll
)

var searchPrefixes = map[rune]int{':': skTable, '.': skColumn, '#': skIndex, '~': skSequence, '@': skConn, '*': skAll}

var searchKindNames = []string{"tables", "columns", "indexes", "sequences", "connections", "queries", "runs", "everything"}

// searchItem is a search candidate.
type searchItem struct {
	kind   int
	text   string // matched
	detail string
	icon   string
	obj    control.CatalogObject // tables, indexes, sequences
	schema string
	col    control.ColumnRef
	conn   control.Connection
	file   string
	run    *history.Entry
}

// tabKind: what a tab searches by default.
func tabKind(tab int) int {
	switch tab {
	case tabConns:
		return skConn
	case tabQueries:
		return skFile
	case tabHistory:
		return skRun
	}
	return skTable
}

// parseSearch splits a leading kind character off the query.
func parseSearch(q string, tab int) (int, string) {
	for _, r := range q {
		if k, ok := searchPrefixes[r]; ok {
			return k, q[len(string(r)):]
		}
		break
	}
	return tabKind(tab), q
}

// searchItems lists the candidates of a kind.
func (p *Panel) searchItems(kind int) []searchItem {
	var out []searchItem
	want := func(k int) bool { return kind == skAll || kind == k }
	if p.cat != nil && (want(skTable) || want(skIndex) || want(skSequence)) {
		for _, s := range p.cat.Schemas {
			for _, o := range s.Objects {
				k := -1
				switch {
				case isTableKind(o.Kind) && o.Table == "":
					k = skTable
				case o.Kind == "index" || o.Kind == "constraint":
					k = skIndex
				case o.Kind == "sequence":
					k = skSequence
				}
				if k < 0 || !want(k) {
					continue
				}
				kindName := o.Kind
				if kindName == "" {
					kindName = "table/view"
				}
				detail := kindName
				if o.Table != "" {
					detail = kindName + " on " + o.Table
				}
				if !s.Default && s.Name != "" {
					detail = s.Name + " · " + detail
				}
				text := o.Name
				if k == skTable {
					text = o.Insert
				}
				out = append(out, searchItem{kind: k, text: text, detail: detail, icon: p.ic.kind(o.Kind), obj: o, schema: s.Name})
			}
		}
	}
	if want(skColumn) {
		for _, c := range p.allCols {
			out = append(out, searchItem{kind: skColumn, text: c.Name, detail: c.TableInsert + " · " + c.Type, icon: p.ic.column, col: c})
		}
	}
	if want(skConn) {
		for _, c := range p.conns {
			d := connTarget(c)
			if c.Group != "" {
				d = c.Group + " · " + d
			}
			out = append(out, searchItem{kind: skConn, text: c.Alias, detail: d, icon: p.ic.connection, conn: c})
		}
	}
	if want(skFile) {
		for _, f := range p.files {
			out = append(out, searchItem{kind: skFile, text: shortPath(f), detail: "query", icon: p.ic.file, file: f})
		}
	}
	if want(skRun) && p.hist != nil {
		seen := map[string]bool{}
		for i := range p.hist.Recent {
			e := p.hist.Recent[i]
			if e.Lineage != "" && seen[e.Lineage] {
				continue
			}
			seen[e.Lineage] = true
			out = append(out, searchItem{kind: skRun, text: firstLine(e.SQL), detail: runLabel(e), icon: p.ic.history, run: &e})
		}
	}
	if kind != skRun && kind != skFile {
		sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].text) < strings.ToLower(out[j].text) })
	}
	return out
}

// runSearch filters the candidates by the query.
func (p *Panel) runSearch() tea.Cmd {
	kind, q := parseSearch(p.query, p.cur)
	var cmd tea.Cmd
	if (kind == skColumn || kind == skAll) && p.allCols == nil && !p.allColsLoading {
		p.allColsLoading = true
		be := p.be
		cmd = func() tea.Msg { c, err := be.AllColumns(); return allColsMsg{c, err} }
	}
	p.skind = kind
	p.items = p.searchItems(kind)
	texts := make([]string, len(p.items))
	for i, it := range p.items {
		texts[i] = it.text
		if it.kind == skColumn && strings.Contains(q, ".") {
			texts[i] = unquote(lastPart(it.col.TableInsert)) + "." + it.col.Name
		}
	}
	p.results = fuzzyRank(strings.TrimSpace(q), texts)
	p.rsel = clamp(p.rsel, 0, len(p.results)-1)
	return cmd
}

type allColsMsg struct {
	cols []control.ColumnRef
	err  error
}

func (p *Panel) searchSelected() *searchItem {
	if p.rsel < 0 || p.rsel >= len(p.results) {
		return nil
	}
	return &p.items[p.results[p.rsel].index]
}

func (p *Panel) searchKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	switch p.km.action(scopeSearch, key) {
	case "searchCancel":
		p.search = false
		return p.previewCmd()
	case "searchOpen":
		if it := p.searchSelected(); it != nil {
			p.search = false
			return p.reveal(it)
		}
		return nil
	case "searchInsert":
		if it := p.searchSelected(); it != nil {
			p.search = false
			return p.insertSearched(it)
		}
		return nil
	case "searchUp":
		p.rsel = clamp(p.rsel-1, 0, len(p.results)-1)
		return p.previewCmd()
	case "searchDown":
		p.rsel = clamp(p.rsel+1, 0, len(p.results)-1)
		return p.previewCmd()
	}
	switch key {
	case "backspace":
		if r := []rune(p.query); len(r) > 0 {
			p.query = string(r[:len(r)-1])
			return tea.Batch(p.runSearch(), p.previewCmd())
		}
		p.search = false // backspace on an empty query: back to the tree
		return p.previewCmd()
	case "ctrl+u":
		p.query = ""
		return p.runSearch()
	case "ctrl+w":
		q := strings.TrimRight(p.query, " ")
		if i := strings.LastIndexAny(q, " ._"); i >= 0 {
			p.query = q[:i]
		} else {
			p.query = ""
		}
		return p.runSearch()
	}
	if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
		p.query += string(k.Runes)
		p.rsel, p.rtop = 0, 0
		return tea.Batch(p.runSearch(), p.previewCmd())
	}
	return nil
}

// insertSearched: ctrl+y on a result.
func (p *Panel) insertSearched(it *searchItem) tea.Cmd {
	switch it.kind {
	case skTable:
		return p.insert(control.PanelAction{Kind: control.ActionTable, Table: it.obj.Insert}, it.obj.Insert)
	case skColumn:
		return p.insert(control.PanelAction{Kind: control.ActionColumns, Table: it.col.TableInsert, Columns: []string{it.col.Insert}}, it.col.Insert)
	case skConn:
		return p.copyCmd(it.conn.Alias)
	case skFile:
		return p.copyCmd(it.file)
	case skRun:
		return p.insert(control.PanelAction{Kind: control.ActionText, Text: it.run.SQL}, it.run.SQL)
	}
	return p.insert(control.PanelAction{Kind: control.ActionText, Text: it.obj.Insert}, it.obj.Insert)
}

// reveal goes to a result in its tab: opens the way to it and selects it.
func (p *Panel) reveal(it *searchItem) tea.Cmd {
	switch it.kind {
	case skConn:
		p.switchTab(tabConns)
		return p.revealWhere(func(n *node) bool { return n.kind == nConnItem && n.conn.Alias == it.conn.Alias })
	case skFile:
		p.switchTab(tabQueries)
		return p.revealWhere(func(n *node) bool { return n.kind == nQueryFile && n.file == it.file })
	case skRun:
		p.switchTab(tabHistory)
		return p.revealWhere(func(n *node) bool { return n.kind == nHistory && n.run.ID == it.run.ID })
	case skColumn:
		p.switchTab(tabObjects)
		table := it.col.TableInsert
		cmd := p.revealWhere(func(n *node) bool { return n.kind == nTable && n.table == table })
		if t := p.selected(); t != nil && t.kind == nTable && t.table == table {
			p.pendingReveal = func(n *node) bool {
				return n.kind == nColumn && n.table == table && n.col.Name == it.col.Name
			}
			c := p.setExpanded(t, true)
			p.tryPendingReveal()
			return tea.Batch(cmd, c)
		}
		return cmd
	case skIndex:
		p.switchTab(tabObjects)
		owner := strings.ToUpper(it.obj.Table)
		cmd := p.revealWhere(func(n *node) bool {
			return n.kind == nTable && strings.EqualFold(n.obj.Name, owner) && strings.EqualFold(n.schema, it.schema)
		})
		if t := p.selected(); t != nil && t.kind == nTable {
			name := it.obj.Name
			p.pendingReveal = func(n *node) bool {
				return n.kind == nObject && n.obj.Name == name && n.parent != nil && n.parent.parent == t
			}
			c := p.setExpanded(t, true)
			p.tryPendingReveal()
			return tea.Batch(cmd, c)
		}
		return cmd
	}
	p.switchTab(tabObjects)
	return p.revealWhere(func(n *node) bool {
		return (n.kind == nTable || n.kind == nObject) && n.parent != nil && n.parent.kind == nGroup &&
			n.schema == it.schema && n.obj.Name == it.obj.Name && n.obj.Kind == it.obj.Kind
	})
}

// revealWhere expands the way to the first node matching and selects it.
func (p *Panel) revealWhere(match func(*node) bool) tea.Cmd {
	t := p.tab()
	var target *node
	walkAll(t.roots, func(n *node) {
		if target == nil && match(n) {
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
			t.sel = i
		}
	}
	return p.previewCmd()
}

// tryPendingReveal selects the node a search result (or a saved
// connection) waits for, once it is there: opens the way to it.
func (p *Panel) tryPendingReveal() {
	if p.pendingReveal == nil {
		return
	}
	match := p.pendingReveal
	var target *node
	walkAll(p.tab().roots, func(n *node) {
		if target == nil && match(n) {
			target = n
		}
	})
	if target == nil {
		return
	}
	for a := target.parent; a != nil; a = a.parent {
		a.expanded = true
		p.expanded[a.id] = true
	}
	for i, r := range p.rows() {
		if r == target {
			p.tab().sel = i
			p.pendingReveal = nil
			return
		}
	}
}
