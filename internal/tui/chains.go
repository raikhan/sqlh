package tui

// The Statement tab's `:` search: tables chains of joins reach from the
// statement's tables. Enter opens the tree along the chains to one (one at a
// time, or all: panel.chains); n / N step through them.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/raikhan/sqlh/internal/control"
)

type chainsMsg struct {
	from string // the statement's tables they were found from
	res  *control.ChainsResult
	err  error
}

type chainToMsg struct {
	target string
	insert bool // ctrl+y: insert the first; else open the tree along them
	res    *control.ChainsResult
	err    error
}

// chainSet: the chains to a table being stepped through (n / N).
type chainSet struct {
	target string
	chains []control.Chain
	more   bool
	cur    int
	// path: per chain, the ids of its rows (the anchor table, then each
	// relation), once walked
	path [][]string
	// walking: the chain the tree is being opened along (-1: none); queue:
	// those still to open (all at once)
	walking int
	queue   []int
}

// stmtTables: the statement's tables as typed, in the tab's order (the
// focused one first).
func (p *Panel) stmtTables() []string {
	var out []string
	for _, n := range p.tabs[tabStmt].roots {
		if n.kind == nStmtTable {
			out = append(out, n.table)
		}
	}
	return out
}

// loadChains asks for the reachable tables (once per set of statement
// tables).
func (p *Panel) loadChains() tea.Cmd {
	from := p.stmtTables()
	key := strings.Join(from, "\x00")
	if len(from) == 0 || p.chainsFrom == key && (p.chains != nil || p.chainsLoading) {
		return nil
	}
	p.chainsFrom, p.chains, p.chainsLoading = key, nil, true
	be := p.be
	return func() tea.Msg {
		r, err := be.Chains(from, "")
		return chainsMsg{key, r, err}
	}
}

// chainItems: the search candidates of `:` in Statement.
func (p *Panel) chainItems() []searchItem {
	var out []searchItem
	if p.chains == nil {
		return out
	}
	for _, t := range p.chains.Targets {
		n := fmt.Sprint(t.Chains)
		if t.More {
			n += "+"
		}
		word := " chains"
		if t.Chains == 1 && !t.More {
			word = " chain"
		}
		detail := n + word + " · " + plural(int64(t.Joins), "join")
		out = append(out, searchItem{kind: skChain, text: t.Table, detail: detail, icon: p.ic.kind(t.Kind),
			obj: control.CatalogObject{Insert: t.Table, Name: unquote(lastPart(t.Table))}})
	}
	return out
}

// chainTo fetches the chains to a table.
func (p *Panel) chainTo(target string, insert bool) tea.Cmd {
	from, be := p.stmtTables(), p.be
	return func() tea.Msg {
		r, err := be.Chains(from, target)
		if err == nil && len(r.Chains) == 0 {
			err = fmt.Errorf("no chain of joins to %s from the statement", target)
		}
		return chainToMsg{target: target, insert: insert, res: r, err: err}
	}
}

// onChainTo: insert the first chain, or open the Statement tree along the
// chains and select the first one's table.
func (p *Panel) onChainTo(msg chainToMsg) tea.Cmd {
	if msg.err != nil {
		p.flash(msg.err.Error(), true)
		return nil
	}
	if msg.insert {
		c := msg.res.Chains[0]
		a := control.PanelAction{Kind: control.ActionTable, Table: msg.target, Anchor: c.Anchor, Path: c.Hops, Mode: "join"}
		return p.insert(a, msg.target)
	}
	p.switchTab(tabStmt)
	cs := &chainSet{target: msg.target, chains: msg.res.Chains, more: msg.res.More,
		path: make([][]string, len(msg.res.Chains)), walking: 0}
	if p.opt.ChainsAll {
		for i := 1; i < len(cs.chains); i++ {
			cs.queue = append(cs.queue, i)
		}
	}
	p.chainSet = cs
	return tea.Batch(append(p.walkPath(), p.previewCmd())...)
}

// stepChain: n / N — the next (d 1) or previous (-1) chain.
func (p *Panel) stepChain(d int) tea.Cmd {
	cs := p.chainSet
	if cs == nil {
		p.flash("no chains: / then : finds the tables chains of joins reach", false)
		return nil
	}
	if len(cs.chains) < 2 {
		p.selectChain(cs.cur)
		return p.previewCmd()
	}
	prev := cs.cur
	cs.cur = (cs.cur + d + len(cs.chains)) % len(cs.chains)
	if cs.path[cs.cur] != nil && (p.opt.ChainsAll || prev == cs.cur) {
		p.selectChain(cs.cur)
		return p.previewCmd()
	}
	if !p.opt.ChainsAll {
		p.closeChain(prev, cs.cur)
	}
	cs.walking = cs.cur
	return tea.Batch(append(p.walkPath(), p.previewCmd())...)
}

// closeChain closes chain i's branch from where it leaves chain j's path.
func (p *Panel) closeChain(i, j int) {
	cs := p.chainSet
	a, b := cs.chains[i], cs.chains[j]
	shared := 0 // relations both chains go through, from the same table
	if sameName(a.Anchor, b.Anchor) {
		for shared < len(a.Hops) && shared < len(b.Hops) &&
			sameName(a.Hops[shared].Table, b.Hops[shared].Table) && sameCols(a.Hops[shared].Cols, b.Hops[shared].Cols) {
			shared++
		}
	}
	ids := cs.path[i]
	if ids == nil || shared+1 >= len(ids) {
		return
	}
	id := ids[shared+1] // the first relation row of i that j doesn't share
	walkAll(p.tabs[tabStmt].roots, func(n *node) {
		if n.id == id {
			n.expanded = false
		}
	})
	p.expanded[id] = false
	cs.path[i] = nil // walked again when it comes back
}

// selectChain selects chain i's table and marks its rows.
func (p *Panel) selectChain(i int) {
	cs := p.chainSet
	if cs == nil || cs.path[i] == nil {
		return
	}
	id := cs.path[i][len(cs.path[i])-1]
	t := &p.tabs[tabStmt]
	for k, r := range visible(t.roots) {
		if r.id == id {
			t.sel = k
		}
	}
	p.pscroll = 0
}

// onChain: n is on the current chain's rows (marked in the tree).
func (p *Panel) onChain(n *node) bool {
	cs := p.chainSet
	if cs == nil || p.cur != tabStmt || cs.path[cs.cur] == nil {
		return false
	}
	for _, id := range cs.path[cs.cur] {
		if id == n.id {
			return true
		}
	}
	return false
}

func sameName(a, b string) bool {
	return strings.EqualFold(unquote(lastPart(a)), unquote(lastPart(b)))
}

func sameCols(a, b [][2]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i][0], b[i][0]) || !strings.EqualFold(a[i][1], b[i][1]) {
			return false
		}
	}
	return true
}

// walkPath opens the Statement tree along the chain being walked as far as
// it is loaded (opening a node loads its children; the next rebuild
// continues), records its rows, then goes on with the queue; the current
// chain's table is selected once its rows are known.
func (p *Panel) walkPath() []tea.Cmd {
	cs := p.chainSet
	if cs == nil || cs.walking < 0 {
		return nil
	}
	var cmds []tea.Cmd
	open := func(n *node) bool { // true: its children are there
		if !n.expanded {
			n.expanded = true
			p.expanded[n.id] = true
		}
		if n.lazy && !n.loaded {
			if cmd := p.ensureChildren(n); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return n.loaded
		}
		return true
	}
	for cs.walking >= 0 {
		c := cs.chains[cs.walking]
		var cur *node
		for _, n := range p.tabs[tabStmt].roots {
			if n.kind == nStmtTable && sameName(n.table, c.Anchor) {
				cur = n
				break
			}
		}
		if cur == nil {
			p.flash(c.Anchor+" is no longer in the statement", true)
			p.chainSet = nil
			return cmds
		}
		ids := []string{cur.id}
		for _, hop := range c.Hops {
			if !open(cur) {
				return cmds // continued once it has loaded
			}
			var group *node
			for _, ch := range cur.children {
				if ch.kind == nGroup && ch.groupKind == "relations" {
					group = ch
				}
			}
			var next *node
			if group != nil {
				open(group)
				for _, ch := range group.children {
					if ch.kind == nRelation && sameName(ch.table, hop.Table) && ch.rel != nil && sameCols(ch.rel.Cols, hop.Cols) {
						next = ch
						break
					}
				}
			}
			if next == nil {
				ids = nil // the tree doesn't have it (yet): skip this chain
				break
			}
			cur = next
			ids = append(ids, cur.id)
		}
		cs.path[cs.walking] = ids
		cs.walking = -1
		if len(cs.queue) > 0 {
			cs.walking, cs.queue = cs.queue[0], cs.queue[1:]
		}
	}
	p.selectChain(cs.cur)
	return cmds
}
