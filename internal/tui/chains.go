package tui

// The Statement tab's `:` search: tables a chain of joins reaches from the
// statement's tables; Enter opens the tree along the shortest chain.

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
	insert bool // ctrl+y: insert it; else open the tree along it
	chain  *control.Chain
	err    error
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
		detail := plural(int64(t.Joins), "join") + " · " + strings.Join(t.Via, " → ")
		if t.Chains > 1 {
			detail += fmt.Sprintf("  +%d more", t.Chains-1)
		}
		out = append(out, searchItem{kind: skChain, text: t.Table, detail: detail, icon: p.ic.kind(t.Kind),
			obj: control.CatalogObject{Insert: t.Table, Name: unquote(lastPart(t.Table))}})
	}
	return out
}

// chainTo fetches the first shortest chain to a table.
func (p *Panel) chainTo(target string, insert bool) tea.Cmd {
	from, be := p.stmtTables(), p.be
	return func() tea.Msg {
		r, err := be.Chains(from, target)
		if err != nil {
			return chainToMsg{target: target, err: err}
		}
		if len(r.Chains) == 0 {
			return chainToMsg{target: target, err: fmt.Errorf("no chain of joins to %s from the statement", target)}
		}
		c := r.Chains[0]
		return chainToMsg{target: target, insert: insert, chain: &c}
	}
}

// onChainTo: insert the chain, or open the Statement tree along it.
func (p *Panel) onChainTo(msg chainToMsg) tea.Cmd {
	if msg.err != nil {
		p.flash(msg.err.Error(), true)
		return nil
	}
	c := msg.chain
	if msg.insert {
		a := control.PanelAction{Kind: control.ActionTable, Table: msg.target, Anchor: c.Anchor, Path: c.Hops, Mode: "join"}
		return p.insert(a, msg.target)
	}
	p.switchTab(tabStmt)
	p.pendingPath = c
	return tea.Batch(append(p.walkPath(), p.previewCmd())...)
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

// walkPath opens the Statement tree along the pending chain as far as it is
// loaded (opening a node loads its children; the next rebuild continues),
// and selects its last table once there.
func (p *Panel) walkPath() []tea.Cmd {
	c := p.pendingPath
	if c == nil {
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
	var cur *node
	for _, n := range p.tabs[tabStmt].roots {
		if n.kind == nStmtTable && sameName(n.table, c.Anchor) {
			cur = n
			break
		}
	}
	if cur == nil {
		p.pendingPath = nil
		p.flash(c.Anchor+" is no longer in the statement", true)
		return nil
	}
	for _, hop := range c.Hops {
		if !open(cur) {
			return cmds
		}
		var group *node
		for _, ch := range cur.children {
			if ch.kind == nGroup && ch.groupKind == "relations" {
				group = ch
			}
		}
		if group == nil {
			p.pendingPath = nil
			return cmds
		}
		open(group)
		var next *node
		for _, ch := range group.children {
			if ch.kind == nRelation && sameName(ch.table, hop.Table) && ch.rel != nil && sameCols(ch.rel.Cols, hop.Cols) {
				next = ch
				break
			}
		}
		if next == nil {
			p.pendingPath = nil
			return cmds
		}
		cur = next
	}
	// there: select it
	p.pendingPath = nil
	t := &p.tabs[tabStmt]
	for i, r := range visible(t.roots) {
		if r == cur {
			t.sel = i
		}
	}
	return cmds
}
