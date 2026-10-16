package tui

// The inline picker: sqlh's replacement for fzf in one-shot picks run from
// the editor (`:insert-output sqlh tables -p`, `sqlh connect`, …). It draws
// on /dev/tty below the cursor (no alternate screen, like fzf --height), so
// stdout stays free for the result.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// PickItem is a line of the picker.
type PickItem struct {
	Value   string // what is returned
	Display string // what is shown (may contain ANSI); default Value
	Match   string // what typing matches; default Value
	Dim     bool   // shown dimmed (secondary entries)
	// Tail is drawn after Display as is, also on the highlighted row (whose
	// Display loses its colours to the selection's): a coloured chip.
	Tail string
}

func (it PickItem) display() string {
	if it.Display != "" {
		return it.Display
	}
	return it.Value
}

func (it PickItem) match() string {
	if it.Match != "" {
		return it.Match
	}
	return it.Value
}

// PickReload replaces the list (drill-down).
type PickReload struct {
	Items          []PickItem
	Prompt, Header string
}

// PickOptions configure a pick.
type PickOptions struct {
	Prompt string
	Header string
	Multi  bool // Tab marks, Ctrl-A marks all; Enter returns the marked ones
	// NoSort keeps the input order while filtering (presets).
	NoSort bool
	// Preview describes the highlighted item (called off the UI loop).
	Preview func(it PickItem) string
	// Expect: keys that also accept (reported in PickResult.Key).
	Expect []string
	// Bind: keys handled by the caller with the highlighted item (nil when
	// the list is empty); a non-nil reload replaces the list.
	Bind map[string]func(cur *PickItem) (*PickReload, error)
	// AcceptQuery: Enter returns the typed query itself when it says so
	// (a typed number instead of the preset it matches).
	AcceptQuery func(query string) bool
	// Height: the share of the terminal's rows (default 0.8).
	Height float64
	// Keys: action → keys (picker.keys; the defaults for the rest).
	Keys map[string][]string
}

// PickerKeys: the keys of a picker action (with the configured ones).
func PickerKeys(set map[string][]string, action string) []string {
	return pickerKeymap(set).keys[0][action]
}

// PickResult is what was chosen.
type PickResult struct {
	Values []string
	Key    string // the accepting key: "" for Enter, else one of Expect
	Query  string // set instead of Values when AcceptQuery took the query
}

// ErrNoTTY: the picker needs a terminal.
var ErrNoTTY = errors.New("the picker needs a terminal (/dev/tty)")

// PickEnv: when set, Pick runs without a UI and takes the best matches for
// its value (single: the first; multi: all) — for scripts and tests.
const PickEnv = "SQLH_PICK"

// Pick lets the user choose; ok is false when cancelled.
func Pick(items []PickItem, opt PickOptions) (PickResult, bool, error) {
	if q, set := os.LookupEnv(PickEnv); set {
		return pickScripted(items, opt, q)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return PickResult{}, false, ErrNoTTY
	}
	defer tty.Close()
	// the colours the terminal has: stdout may be a pipe (Helix's :pipe-to),
	// lipgloss would decide on none from it
	lipgloss.SetColorProfile(termenv.NewOutput(tty).EnvColorProfile())
	// Helix enables the kitty keyboard protocol: push "no flags" so Esc and
	// Ctrl keys arrive plainly, pop afterwards.
	_, _ = tty.WriteString("\x1b[>0u")
	defer func() { _, _ = tty.WriteString("\x1b[<1u") }()
	m := newPicker(items, opt)
	prog := tea.NewProgram(m, tea.WithInput(tty), tea.WithOutput(tty))
	final, err := prog.Run()
	if err != nil {
		return PickResult{}, false, err
	}
	pm := final.(*picker)
	if pm.err != nil {
		return PickResult{}, false, pm.err
	}
	return pm.result, pm.accepted, nil
}

func pickScripted(items []PickItem, opt PickOptions, q string) (PickResult, bool, error) {
	if opt.AcceptQuery != nil && opt.AcceptQuery(q) {
		return PickResult{Query: q}, true, nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.match()
	}
	ms := fuzzyRank(q, texts)
	if opt.NoSort {
		ms = inOrder(ms)
	}
	if len(ms) == 0 {
		return PickResult{}, false, nil
	}
	if !opt.Multi {
		ms = ms[:1]
	}
	var res PickResult
	for _, m := range ms {
		res.Values = append(res.Values, items[m.index].Value)
	}
	return res, true, nil
}

func inOrder(ms []match) []match {
	out := append([]match(nil), ms...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].index < out[j-1].index; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

type picker struct {
	opt     PickOptions
	items   []PickItem
	prompt  string
	header  string
	query   string
	results []match
	sel     int
	top     int
	marked  map[string]bool
	km      keymap
	gen     int // list generation (previews are cached per list)
	preview map[string]string
	pscroll int

	width, height int
	done          bool
	accepted      bool
	result        PickResult
	err           error
	msg           string
}

func newPicker(items []PickItem, opt PickOptions) *picker {
	if opt.Height <= 0 || opt.Height > 1 {
		opt.Height = 0.8
	}
	p := &picker{opt: opt, items: items, prompt: opt.Prompt, header: opt.Header, marked: map[string]bool{},
		preview: map[string]string{}, km: pickerKeymap(opt.Keys)} // no size until the terminal reports it
	p.filter()
	return p
}

type previewMsg struct {
	key, text string
}

func (p *picker) Init() tea.Cmd { return p.previewCmd() }

func (p *picker) filter() {
	texts := make([]string, len(p.items))
	for i, it := range p.items {
		texts[i] = it.match()
	}
	p.results = fuzzyRank(p.query, texts)
	if p.opt.NoSort {
		p.results = inOrder(p.results)
	}
	p.sel = clamp(p.sel, 0, len(p.results)-1)
}

func (p *picker) current() *PickItem {
	if p.sel < 0 || p.sel >= len(p.results) {
		return nil
	}
	return &p.items[p.results[p.sel].index]
}

func (p *picker) previewKey(it *PickItem) string { return strconv.Itoa(p.gen) + "\x00" + it.Value }

func (p *picker) previewCmd() tea.Cmd {
	it := p.current()
	if p.opt.Preview == nil || it == nil {
		return nil
	}
	k := p.previewKey(it)
	if _, ok := p.preview[k]; ok {
		return nil
	}
	p.preview[k] = "" // loading
	item, fn := *it, p.opt.Preview
	return func() tea.Msg { return previewMsg{key: k, text: fn(item)} }
}

func (p *picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
	case previewMsg:
		p.preview[msg.key] = msg.text
	case tea.KeyMsg:
		return p, p.key(msg)
	}
	return p, nil
}

func (p *picker) finish(accepted bool, key string) tea.Cmd {
	p.done, p.accepted = true, accepted
	if accepted {
		p.result.Key = key
		if p.opt.Multi {
			for _, it := range p.items { // in list order
				if p.marked[it.Value] {
					p.result.Values = append(p.result.Values, it.Value)
				}
			}
		}
		if len(p.result.Values) == 0 {
			if it := p.current(); it != nil {
				p.result.Values = []string{it.Value}
			} else {
				p.accepted = false
			}
		}
	}
	return tea.Quit
}

func (p *picker) move(d int) tea.Cmd {
	p.sel = clamp(p.sel+d, 0, len(p.results)-1)
	p.pscroll = 0
	return p.previewCmd()
}

func (p *picker) key(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	p.msg = ""
	if fn, ok := p.opt.Bind[key]; ok {
		reload, err := fn(p.current())
		if err != nil {
			p.msg = err.Error()
			return nil
		}
		if reload != nil {
			p.items, p.query, p.sel, p.top, p.pscroll = reload.Items, "", 0, 0, 0
			p.marked = map[string]bool{}
			if reload.Prompt != "" {
				p.prompt = reload.Prompt
			}
			if reload.Header != "" {
				p.header = reload.Header
			}
			p.gen++
			p.filter()
			return p.previewCmd()
		}
		return nil
	}
	for _, e := range p.opt.Expect {
		if key == e {
			return p.finish(true, key)
		}
	}
	if key == "ctrl+c" {
		return p.finish(false, "")
	}
	switch p.km.action(0, key) {
	case "cancel":
		return p.finish(false, "")
	case "accept":
		if p.opt.AcceptQuery != nil && p.opt.AcceptQuery(p.query) {
			p.done, p.accepted, p.result = true, true, PickResult{Query: p.query}
			return tea.Quit
		}
		return p.finish(true, "")
	case "up":
		return p.move(-1)
	case "down":
		return p.move(1)
	case "pageUp":
		return p.move(-p.listHeight())
	case "pageDown":
		return p.move(p.listHeight())
	case "previewDown":
		p.pscroll++
		return nil
	case "previewUp":
		p.pscroll = max(p.pscroll-1, 0)
		return nil
	case "toggleDown", "toggleUp":
		if p.opt.Multi {
			if it := p.current(); it != nil {
				p.marked[it.Value] = !p.marked[it.Value]
			}
			if p.km.action(0, key) == "toggleDown" {
				return p.move(1)
			}
			return p.move(-1)
		}
		return nil
	case "toggleAll":
		if p.opt.Multi {
			for _, m := range p.results {
				p.marked[p.items[m.index].Value] = true
			}
		}
		return nil
	case "clearQuery":
		p.query, p.sel, p.top = "", 0, 0
		p.filter()
		return p.previewCmd()
	case "deleteWord":
		q := strings.TrimRight(p.query, " ")
		if i := strings.LastIndexAny(q, " ._"); i >= 0 {
			p.query = q[:i]
		} else {
			p.query = ""
		}
		p.sel, p.top = 0, 0
		p.filter()
		return p.previewCmd()
	}
	switch key {
	case "backspace":
		if r := []rune(p.query); len(r) > 0 {
			p.query = string(r[:len(r)-1])
			p.sel, p.top = 0, 0
			p.filter()
			return p.previewCmd()
		}
	default:
		if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
			p.query += string(k.Runes)
			p.sel, p.top = 0, 0
			p.filter()
			return p.previewCmd()
		}
	}
	return nil
}

// rows the picker uses
func (p *picker) totalHeight() int { return max(int(float64(p.height)*p.opt.Height), 6) }

func (p *picker) headerLines() int {
	if p.header == "" {
		return 1
	}
	return 2
}

func (p *picker) listHeight() int { return max(p.totalHeight()-p.headerLines()-1, 1) }

func (p *picker) View() string {
	if p.done || p.width <= 0 || p.height <= 0 {
		return "" // leave nothing behind; nothing before the size is known
	}
	h := p.totalHeight()
	w := p.width
	var lines []string
	count := stDim.Render(fmt.Sprintf("  %d/%d", len(p.results), len(p.items)))
	if p.opt.Multi && len(p.marked) > 0 {
		count += stMarked.Render(fmt.Sprintf(" (%d)", len(p.marked)))
	}
	lines = append(lines, fit(stKey.Render(p.prompt+"> ")+p.query+stAccent.Render("▏")+count, w))
	if p.header != "" {
		lines = append(lines, fit(stDim.Render(p.header), w))
	}
	if p.msg != "" {
		lines[len(lines)-1] = fit(stErr.Render(p.msg), w)
	}
	lh := h - len(lines)
	listW, prevW := w, 0
	if p.opt.Preview != nil && w >= 70 {
		listW = w / 2
		prevW = w - listW - 1
	}
	p.sel = clamp(p.sel, 0, len(p.results)-1)
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+lh {
		p.top = p.sel - lh + 1
	}
	var list []string
	for i := p.top; i < len(p.results) && len(list) < lh; i++ {
		m := p.results[i]
		it := p.items[m.index]
		mark, plainMark := "  ", "  "
		if p.marked[it.Value] {
			mark, plainMark = stMarked.Render("● "), "● "
		}
		if i == p.sel {
			// the same cells as the other rows: a 1-cell bar, the mark, the text
			text := "▌" + plainMark + ansi.Strip(it.display())
			if it.Tail == "" {
				list = append(list, stSel.Render(fit(text, listW)))
				continue
			}
			tw := lipgloss.Width(it.Tail)
			row := stSel.Render(fit(text, max(listW-tw, 0)))
			if lipgloss.Width(text)+tw <= listW {
				row = stSel.Render(text) + it.Tail + stSel.Render(strings.Repeat(" ", listW-lipgloss.Width(text)-tw))
			}
			list = append(list, fit(row, listW))
			continue
		}
		disp := it.display()
		if it.Display == "" || it.Display == it.Value {
			disp = highlight(disp, m.pos)
		}
		if it.Dim {
			disp = stDim.Render(ansi.Strip(disp))
		}
		list = append(list, fit(" "+mark+disp+it.Tail, listW))
	}
	for len(list) < lh {
		list = append(list, strings.Repeat(" ", listW))
	}
	if prevW > 0 {
		var prev []string
		if it := p.current(); it != nil {
			text, ok := p.preview[p.previewKey(it)]
			if !ok || text == "" {
				text = stDim.Render("…")
			}
			for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
				prev = append(prev, strings.Split(ansi.Wrap(l, max(prevW-1, 10), " "), "\n")...)
			}
		}
		p.pscroll = clamp(p.pscroll, 0, max(len(prev)-lh, 0))
		if p.pscroll < len(prev) {
			prev = prev[p.pscroll:]
		}
		for i := range list {
			pl := ""
			if i < len(prev) {
				pl = prev[i]
			}
			list[i] += stBorder.Render("│") + fit(pl, prevW)
		}
	}
	lines = append(lines, list...)
	return strings.Join(lines, "\n")
}
