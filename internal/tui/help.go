package tui

// The keys palette (`?`): the actions of the current tab, grouped,
// filtered as you type (`@` filters on the keys); Enter runs one.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/raikhan/sqlh/internal/config"
)

// actionDesc: what an action does, as the palette lists it.
var actionDesc = map[string]string{
	"up": "Move up", "down": "Move down", "top": "Go to the top", "bottom": "Go to the bottom",
	"pageUp": "Half a page up", "pageDown": "Half a page down",
	"scrollTop":   "Scroll the selected row to the top",
	"expand":      "Open the selected item",
	"collapse":    "Close it (on a closed one: go to its parent)",
	"collapseAll": "Collapse all",
	"expandAll":   "Expand all tables (and follow new ones open)",
	"open":        "Enter: open/close, connect, open the file",
	"tab1":        "Tab: Connections", "tab2": "Tab: Objects", "tab3": "Tab: Statement",
	"tab4": "Tab: Queries", "tab5": "Tab: History",
	"previewNext": "Preview: next tab", "previewPrev": "Preview: previous tab",
	"previewDown": "Preview: scroll down", "previewUp": "Preview: scroll up",
	"copy":         "Copy the name (a run: its SQL; a file: its path)",
	"actions":      "Actions of the selection (menu)",
	"search":       "Search (: tables  . columns  # indexes  ~ sequences  @ connections  * all)",
	"help":         "Keys (this list)",
	"quit":         "Quit",
	"backToEditor": "Back to the editor",
	"insert":       "Insert at the editor's cursor (no editor: copy)",
	"mark":         "Mark a column (insert the marked ones together)",
	"sample":       "Sample a row (a column: its most common values)",
	"nextRow":      "Sample: next row", "prevRow": "Sample: previous row",
	"newQuery":            "New query file (SELECT * FROM the selected table)",
	"refresh":             "Refresh the schema cache",
	"newConnection":       "New connection",
	"editConnection":      "Edit the connection",
	"duplicateConnection": "Duplicate the connection",
	"deleteConnection":    "Delete the connection",
	"openResult":          "Open the run's result in the viewer",
	"editFile":            "Open the run's file in the editor",
	"renameQuery":         "Rename the query file",
	"nextChain":           "Join chains: the next one's table (after / :)",
	"prevChain":           "Join chains: the previous one's table",
	"clearChains":         "Join chains: stop stepping through them",
	"editConfig":          "Edit the config file in $EDITOR (reloads after)",
	"reloadConfig":        "Reload the config file (keys, colours, connections, …)",
	"historyAllConns":     "Recent runs: all connections / the current one's",
	"deleteQuery":         "Delete the query file",
	"searchUp":            "Search: previous result", "searchDown": "Search: next result",
	"searchOpen": "Search: go to the result", "searchInsert": "Search: insert the result",
	"searchCancel": "Search: back to the tree", "searchNextKind": "Search: next kind (prefix)",
	"searchPrevKind": "Search: previous kind",
}

// actionTitle: an action's description, lower case (for notes).
func actionTitle(a string) string {
	d := actionDesc[a]
	if d == "" {
		return a
	}
	if i := strings.Index(d, " ("); i > 0 {
		d = d[:i]
	}
	return strings.ToLower(d[:1]) + d[1:]
}

// palette groups, after the tab's own actions
var paletteGroups = []struct {
	name    string
	actions []string
}{
	{"Navigation", []string{"open", "up", "down", "expand", "collapse", "collapseAll", "top", "bottom", "pageUp", "pageDown",
		"scrollTop", "tab1", "tab2", "tab3", "tab4", "tab5"}},
	{"Preview", []string{"previewNext", "previewPrev", "previewDown", "previewUp"}},
	{"Global", []string{"search", "actions", "copy", "backToEditor", "editConfig", "reloadConfig", "help", "quit"}},
}

type paletteEntry struct {
	action, keys, desc, group string
}

type paletteRow struct {
	header string
	entry  int // -1: a group header ("" header: a gap)
}

type palette struct {
	title   string
	entries []paletteEntry
	query   string
	rows    []paletteRow
	cursor  int // index into rows (an entry row when there is one)
	offset  int
	listH   int
}

// openPalette builds the palette of the current tab.
func (p *Panel) openPalette() {
	scope := p.cur
	pal := &palette{title: mainTabNames[p.cur] + " · keys"}
	add := func(group string, actions []string) {
		for _, a := range actions {
			if !p.km.has(scope, a) {
				continue
			}
			if a == "backToEditor" && p.opt.Standalone && p.editor == "" {
				continue
			}
			pal.entries = append(pal.entries, paletteEntry{action: a, keys: p.km.labelsIn(scope, a), desc: actionDesc[a], group: group})
		}
	}
	add(mainTabNames[p.cur], config.PanelOwnActions(config.PanelScopes[scope]))
	for _, g := range paletteGroups {
		add(g.name, g.actions)
	}
	pal.refilter()
	p.pal = pal
}

func (pal *palette) refilter() {
	q := strings.TrimSpace(pal.query)
	pal.rows = pal.rows[:0]
	switch {
	case q == "":
		last := ""
		for i, e := range pal.entries {
			if e.group != last {
				if last != "" {
					pal.rows = append(pal.rows, paletteRow{entry: -1})
				}
				pal.rows = append(pal.rows, paletteRow{header: e.group, entry: -1})
				last = e.group
			}
			pal.rows = append(pal.rows, paletteRow{entry: i})
		}
	case strings.HasPrefix(q, "@"):
		k := strings.TrimSpace(q[1:])
		for i, e := range pal.entries {
			if k == "" || strings.Contains(" "+e.keys+" ", " "+k) {
				pal.rows = append(pal.rows, paletteRow{entry: i})
			}
		}
	default:
		// substring hits first (in list order), then fuzzy ones
		seen := map[int]bool{}
		lq := strings.ToLower(q)
		for i, e := range pal.entries {
			if strings.Contains(strings.ToLower(e.desc), lq) {
				pal.rows = append(pal.rows, paletteRow{entry: i})
				seen[i] = true
			}
		}
		texts := make([]string, len(pal.entries))
		for i, e := range pal.entries {
			texts[i] = e.desc
		}
		for _, m := range fuzzyRank(q, texts) {
			if !seen[m.index] {
				pal.rows = append(pal.rows, paletteRow{entry: m.index})
			}
		}
	}
	pal.cursor = pal.nextEntry(0, 1)
	pal.offset = 0
}

// nextEntry: the first entry row from i walking by dir (-1: none).
func (pal *palette) nextEntry(i, dir int) int {
	for ; i >= 0 && i < len(pal.rows); i += dir {
		if pal.rows[i].entry >= 0 {
			return i
		}
	}
	return -1
}

func (pal *palette) move(n int) {
	if pal.cursor < 0 {
		return
	}
	dir := 1
	if n < 0 {
		dir, n = -1, -n
	}
	for ; n > 0; n-- {
		next := pal.nextEntry(pal.cursor+dir, dir)
		if next < 0 {
			return
		}
		pal.cursor = next
	}
}

func (pal *palette) count() (pos, total int) {
	for i, r := range pal.rows {
		if r.entry >= 0 {
			total++
			if i <= pal.cursor {
				pos = total
			}
		}
	}
	return
}

func (p *Panel) paletteKey(k tea.KeyMsg) tea.Cmd {
	pal := p.pal
	key := k.String()
	switch key {
	case "up", "ctrl+p", "ctrl+k":
		pal.move(-1)
	case "down", "ctrl+n", "ctrl+j":
		pal.move(1)
	case "pgup":
		pal.move(-max(pal.listH-1, 1))
	case "pgdown":
		pal.move(max(pal.listH-1, 1))
	case "home":
		pal.cursor = pal.nextEntry(0, 1)
	case "end":
		pal.cursor = pal.nextEntry(len(pal.rows)-1, -1)
	case "esc":
		if pal.query != "" {
			pal.query = ""
			pal.refilter()
			return nil
		}
		p.pal = nil
	case "enter":
		if pal.cursor < 0 {
			return nil
		}
		a := pal.entries[pal.rows[pal.cursor].entry].action
		p.pal = nil
		if a == "help" {
			return nil
		}
		return p.do(a)
	case "backspace":
		if r := []rune(pal.query); len(r) > 0 {
			pal.query = string(r[:len(r)-1])
			pal.refilter()
		}
	case "ctrl+u":
		pal.query = ""
		pal.refilter()
	default:
		if pal.query == "" && p.km.action(p.cur, key) == "help" {
			p.pal = nil
			return nil
		}
		if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
			pal.query += string(k.Runes)
			pal.refilter()
		}
	}
	return nil
}

// viewPalette: the box (w, h: the body's size).
func (p *Panel) viewPalette(w, h int) []string {
	pal := p.pal
	bw := min(max(w-6, 30), 76)
	inner := bw - 2 // the box's padding
	pal.listH = max(min(h-7, len(pal.rows)), 3)
	keyW := 0
	for _, e := range pal.entries {
		keyW = max(keyW, lipgloss.Width(e.keys))
	}
	keyW = min(keyW, 14)
	if pal.cursor >= 0 {
		if pal.cursor < pal.offset {
			pal.offset = pal.cursor
		}
		if pal.cursor >= pal.offset+pal.listH {
			pal.offset = pal.cursor - pal.listH + 1
		}
		// a group's first entry shows its header too
		if pal.offset > 0 && pal.cursor == pal.offset && pal.rows[pal.offset-1].entry < 0 {
			pal.offset--
		}
	}
	pal.offset = clamp(pal.offset, 0, max(len(pal.rows)-pal.listH, 0))
	var lines []string
	for i := pal.offset; i < len(pal.rows) && len(lines) < pal.listH; i++ {
		r := pal.rows[i]
		if r.entry < 0 {
			if r.header == "" {
				lines = append(lines, "")
			} else {
				lines = append(lines, stTitle.Render("── "+r.header+" ──"))
			}
			continue
		}
		e := pal.entries[r.entry]
		keys := fit(e.keys, keyW)
		if i == pal.cursor {
			lines = append(lines, stMenuSel.Render(fit("▸ "+keys+"  "+e.desc, inner)))
		} else {
			lines = append(lines, fit("  "+stKey.Render(keys)+"  "+e.desc, inner))
		}
	}
	if len(pal.rows) == 0 {
		lines = append(lines, stDim.Render("  no match"))
	}
	for len(lines) < pal.listH {
		lines = append(lines, "")
	}
	pos, total := pal.count()
	input := stKey.Render("filter ") + pal.query + stAccent.Render("▏") + stDim.Render("  @ for keys")
	count := stDim.Render(fmt.Sprintf("%d of %d", pos, total))
	gap := max(inner-lipgloss.Width(input)-lipgloss.Width(count), 1)
	body := append([]string{stTitle.Render(pal.title), ""}, lines...)
	body = append(body, "", input+strings.Repeat(" ", gap)+count)
	return strings.Split(stMenuBox.Width(bw).Render(strings.Join(body, "\n")), "\n")
}
