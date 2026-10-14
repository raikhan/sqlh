package cmd

// Relationship commands and the table picker:
//
//   - describe TABLE: columns (foreign keys marked), relationships, a view's
//     definition; the pickers' preview;
//   - relations TABLE: the relationships alone;
//   - tables -p [--file F --line L --col C]: the table picker. The list shows
//     table names only; the preview how the highlighted one joins, then its
//     description. Given the cursor, the tables related to the statement's come
//     first (the others dimmed); on a table of the statement, the list starts
//     with that table's relationships. After JOIN, Enter inserts the table with
//     an alias and its ON condition (relations.insert: name → the name only;
//     Ctrl-T does the other). Ctrl-R lists the relationships of the highlighted
//     table (drill down; accepting then inserts the whole join chain), Ctrl-O
//     goes back. The drill-down state lives in a temp file that fzf's bindings
//     update through the hidden `_nav` command.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/control"
)

func relationCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "describe",
			Usage:     "show a table or view: columns, relationships (foreign keys, views, guesses), definition",
			ArgsUsage: "TABLE",
			Flags:     catalogFlags(),
			Action:    editorAction(describeCmd),
		},
		{
			Name:      "relations",
			Usage:     "list the tables and views related to TABLE (-p: pick one)",
			ArgsUsage: "TABLE",
			Flags:     catalogFlags(),
			Action:    editorAction(relationsCmd),
		},
		{
			// fzf bindings of the table picker (drill-down state)
			Name:      "_nav",
			Hidden:    true,
			ArgsUsage: "lines|preview KEY|push KEY|pop",
			Flags: append(catalogFlags(),
				&cli.StringFlag{Name: "state", Required: true}),
			Action: navCmd,
		},
	}
}

// tablePickFlags are the tables command's picker flags.
func tablePickFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{Name: "line", Aliases: []string{"l"}, Usage: "with -p: 1-based cursor line in --file (Helix %{cursor_line})"},
		&cli.IntFlag{Name: "col", Usage: "with -p: 1-based cursor column in --file (Helix %{cursor_column})"},
		&cli.StringFlag{Name: "insert", Usage: "with -p after JOIN: `join` (table, alias, ON condition) or name (default: relations.insert)"},
		&cli.BoolFlag{Name: "related", Usage: "with -p and --file: only the tables related to the statement's"},
	}
}

// ---- describe / relations -----------------------------------------------------

func describeCmd(c *cli.Context) error {
	table := c.Args().First()
	if table == "" {
		return errors.New("give a table")
	}
	api, err := openCatalog(c)
	if err != nil {
		return err
	}
	defer api.close()
	cols, err := api.columns(table)
	if err != nil {
		return err
	}
	rel, err := api.relations(table)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return printJSON(struct {
			*control.RelationsResult
			Columns []control.ColumnItem `json:"columns"`
		}{rel, cols})
	}
	fmt.Print(describeText(rel, cols))
	return nil
}

// textStyle colours parts of describe output (the picker preview; plain for
// a pipe).
type textStyle struct{ color bool }

func (t textStyle) table(s string) string {
	if t.color {
		return ansiCyan + s + ansiReset
	}
	return s
}

func (t textStyle) note(s string) string {
	if t.color && s != "" {
		return ansiDim + s + ansiReset
	}
	return s
}

// colorTables highlights the related table in a condition (other.col = col).
func (t textStyle) condition(cond, table string) string {
	if !t.color || cond == "" {
		return cond
	}
	return strings.ReplaceAll(cond, table+".", t.table(table)+".")
}

// relationLine renders a relationship: → other.col = col  notes.
func (t textStyle) relationLine(r control.RelationItem) string {
	line := r.Marker + " " + t.condition(r.Condition, r.Insert)
	if r.Condition == "" {
		line = r.Marker + " " + t.table(r.Insert)
	}
	if r.Note != "" {
		line += "  " + t.note(r.Note)
	}
	return line
}

// describeText renders a table like the editor's hover: columns, then its
// relationships (the picker preview).
func describeText(rel *control.RelationsResult, cols []control.ColumnItem) string {
	return textStyle{}.describe(rel, cols)
}

func (t textStyle) describe(rel *control.RelationsResult, cols []control.ColumnItem) string {
	var b strings.Builder
	kind := rel.Kind
	if rel.Writes != "" {
		kind += " (" + rel.Writes + ")"
	}
	fmt.Fprintf(&b, "%s %s\n\n", rel.Insert, kind)
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Name\tType\tPK")
	for _, col := range cols {
		key := ""
		if isPK(col.Key) {
			key = "PK"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", col.Insert, col.Type, key)
	}
	_ = w.Flush()
	b.WriteString(t.relations(rel))
	if rel.Definition != "" {
		b.WriteString("\nDefinition\n\n" + rel.Definition + "\n")
	}
	return trimLines(b.String())
}

// trimLines drops trailing blanks (tabwriter pads empty last cells).
func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// relationsText renders the relationships like the hover: one list of joins
// (→ references, ← referenced by), then the view dependencies.
func relationsText(rel *control.RelationsResult) string { return textStyle{}.relations(rel) }

func (t textStyle) relations(rel *control.RelationsResult) string {
	var b strings.Builder
	if rel.Note != "" {
		fmt.Fprintf(&b, "\n(%s)\n", rel.Note)
	}
	var reads, usedBy []string
	n := 0
	for _, r := range rel.Relations {
		switch {
		case r.Edge == "uses" && r.Out:
			reads = append(reads, r.Insert)
		case r.Edge == "uses":
			usedBy = append(usedBy, r.Insert)
		default:
			if n == 0 {
				b.WriteString("\nRelationships\n\n")
			}
			n++
			b.WriteString(t.relationLine(r) + "\n")
		}
	}
	if len(reads) > 0 {
		b.WriteString("\nReads: " + strings.Join(reads, ", ") + "\n")
	}
	if len(usedBy) > 0 {
		b.WriteString("\nUsed by views: " + strings.Join(usedBy, ", ") + "\n")
	}
	if !rel.ViewsLoaded && rel.Note == "" {
		b.WriteString("\n(views are still loading)\n")
	}
	return trimLines(b.String())
}

func relationsCmd(c *cli.Context) error {
	table := c.Args().First()
	if table == "" {
		return errors.New("give a table")
	}
	api, err := openCatalog(c)
	if err != nil {
		return err
	}
	defer api.close()
	rel, err := api.relations(table)
	if err != nil {
		return err
	}
	switch {
	case c.Bool("json"):
		return printJSON(rel)
	case c.Bool("pick"):
		st := &navState{Path: []navItem{{Table: rel.Insert}}}
		return runTablePicker(c, api, st)
	case c.Bool("long"):
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, r := range rel.Relations {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Marker, r.Insert, r.Edge, r.Condition, r.Note)
		}
		return w.Flush()
	default:
		fmt.Print(strings.TrimPrefix(relationsText(rel), "\n"))
	}
	return nil
}

// ---- the table picker ---------------------------------------------------------

// navItem is a picker line's value (base64 JSON in fzf's first field): a table,
// and how it joins the table it was listed for.
type navItem struct {
	Table  string      `json:"t"`           // as typed
	Cols   [][2]string `json:"c,omitempty"` // (anchor's column, table's column)
	Ref    string      `json:"r,omitempty"` // the anchor's reference in the statement (top level)
	Anchor string      `json:"a,omitempty"` // the table it was listed for
	// for the preview
	Marker string `json:"m,omitempty"`
	Edge   string `json:"e,omitempty"`
	Out    bool   `json:"o,omitempty"`
	Note   string `json:"n,omitempty"`
}

func (n navItem) key() string {
	b, err := json.Marshal(n)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func parseNavItem(key string) (navItem, error) {
	var n navItem
	b, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return n, err
	}
	err = json.Unmarshal(b, &n)
	return n, err
}

// navState is the picker's state: the statement's context and the tables
// drilled into.
type navState struct {
	Ctx     control.ContextResult `json:"ctx"`
	Path    []navItem             `json:"path"`
	Related bool                  `json:"related"`
	// Copy: accepting copies the name to the clipboard instead of inserting
	// (opened on a table name: inserting would split it)
	Copy   bool   `json:"copy,omitempty"`
	Schema string `json:"schema,omitempty"` // other tables: only this schema
}

func loadNavState(path string) (*navState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st navState
	return &st, json.Unmarshal(b, &st)
}

func (st *navState) save(path string) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// inJoin: accepting inserts a join (the cursor is after JOIN, with tables to
// join to).
func (st *navState) inJoin() bool { return st.Ctx.InJoin && len(st.Ctx.Tables) > 0 }

func (st *navState) prompt() string {
	if len(st.Path) == 0 {
		if st.inJoin() {
			return "join"
		}
		return "table"
	}
	var names []string
	if st.Path[0].Ref != "" {
		names = append(names, st.Path[0].Ref) // the statement's table it was reached from
	}
	for _, p := range st.Path {
		names = append(names, p.Table)
	}
	return strings.Join(names, " › ")
}

func (st *navState) header() string {
	enter, other := "insert", ""
	switch {
	case st.Copy:
		enter = "copy name"
	case st.inJoin():
		if st.Ctx.Insert == "name" {
			enter, other = "name", "Ctrl-T: JOIN"
		} else {
			enter, other = "JOIN", "Ctrl-T: name"
		}
	}
	parts := []string{"Enter: " + enter}
	if other != "" {
		parts = append(parts, other)
	}
	return strings.Join(append(parts, "Ctrl-R: relations", "Ctrl-O: back"), " · ")
}

// navEntry is a picker line before rendering.
type navEntry struct {
	item navItem
	// related: listed for a relationship (else one of the other tables)
	related bool
	// detail tells apart lines for the same table: the anchor's columns
	detail string
}

func relationEntry(r control.RelationItem, ref, anchor string, top bool) navEntry {
	item := navItem{Table: r.Insert, Cols: r.Cols, Anchor: anchor, Marker: r.Marker, Edge: r.Edge, Out: r.Out, Note: r.Note}
	if top {
		item.Ref = ref
	}
	var cols []string
	for _, c := range r.Cols {
		cols = append(cols, ref+"."+c[0])
	}
	detail := strings.Join(cols, ", ")
	if detail == "" {
		detail = ref
	}
	return navEntry{item: item, related: true, detail: detail}
}

const (
	ansiDim   = "\x1b[2m"
	ansiCyan  = "\x1b[36m"
	ansiReset = "\x1b[0m"
)

// renderEntries makes fzf lines "key<TAB>table<TAB>display": the display is
// the table's name, with the anchor's columns when the table is listed more
// than once; the other tables are dimmed when related ones are listed.
func renderEntries(entries []navEntry) []string {
	count := map[string]int{}
	anyRelated := false
	for _, e := range entries {
		count[e.item.Table]++
		anyRelated = anyRelated || e.related
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		display := e.item.Table
		if count[e.item.Table] > 1 && e.related {
			display += " " + ansiDim + "(" + e.detail + ")" + ansiReset
		}
		if anyRelated && !e.related {
			display = ansiDim + display + ansiReset
		}
		lines = append(lines, e.item.key()+"\t"+e.item.Table+"\t"+display)
	}
	return lines
}

// navLines lists the picker's lines for the state.
func navLines(api catalogAPI, st *navState) ([]string, error) {
	entries, err := navEntries(api, st)
	if err != nil {
		return nil, err
	}
	return renderEntries(entries), nil
}

func navEntries(api catalogAPI, st *navState) ([]navEntry, error) {
	if len(st.Path) > 0 {
		cur := st.Path[len(st.Path)-1]
		rel, err := api.relations(cur.Table)
		if err != nil {
			return nil, err
		}
		var entries []navEntry
		for _, r := range rel.Relations {
			entries = append(entries, relationEntry(r, cur.Table, cur.Table, false))
		}
		if len(entries) == 0 {
			// accepting it inserts the table itself
			entries = append(entries, navEntry{item: navItem{Table: cur.Table}})
		}
		return entries, nil
	}

	var entries []navEntry
	listed := map[string]bool{}
	scope := map[string]bool{}
	for _, t := range st.Ctx.Tables {
		scope[strings.ToUpper(t.Name)] = true
	}
	// the focused table first, then the others in statement order
	tables := append([]control.ScopeTable(nil), st.Ctx.Tables...)
	for i, t := range tables {
		if st.Ctx.Focus != "" && strings.EqualFold(t.Name, st.Ctx.Focus) && i > 0 {
			tables = append([]control.ScopeTable{t}, append(tables[:i:i], tables[i+1:]...)...)
			break
		}
	}
	var deps []navEntry
	for _, t := range tables {
		name := t.Name
		if t.Schema != "" {
			name = t.Schema + "." + t.Name
		}
		rel, err := api.relations(name)
		if err != nil {
			continue // e.g. a table that doesn't exist (yet)
		}
		for _, r := range rel.Relations {
			if scope[strings.ToUpper(r.Table)] {
				continue // already in the statement
			}
			e := relationEntry(r, t.Ref, rel.Insert, true)
			if len(r.Cols) == 0 {
				deps = append(deps, e)
				continue
			}
			entries = append(entries, e)
			listed[strings.ToUpper(r.Table)] = true
		}
	}
	// view dependencies without join columns: once, unless joinable above
	for _, e := range deps {
		if u := strings.ToUpper(e.item.Table); !listed[u] {
			entries = append(entries, e)
			listed[u] = true
		}
	}
	if st.Related && len(entries) > 0 {
		return entries, nil
	}
	all, err := api.tables(st.Schema, false)
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if t.Default && listed[strings.ToUpper(t.Name)] {
			continue
		}
		entries = append(entries, navEntry{item: navItem{Table: t.Insert}})
	}
	return entries, nil
}

// previewText describes a picker item: how it joins the table it is listed
// for, then the table itself.
func previewText(api catalogAPI, item navItem) (string, error) {
	cols, err := api.columns(item.Table)
	if err != nil {
		return "", err
	}
	rel, err := api.relations(item.Table)
	if err != nil {
		return "", err
	}
	t := textStyle{color: true}
	var b strings.Builder
	if item.Anchor != "" {
		// how it joins the table it is listed for: sdst → ai_model.id = sdst.translation_model_id
		ref := item.Ref
		if ref == "" {
			ref = item.Anchor
		}
		var cond []string
		for _, c := range item.Cols {
			cond = append(cond, t.table(item.Table)+"."+c[1]+" = "+ref+"."+c[0])
		}
		line := ref + " " + item.Marker + " "
		switch {
		case len(cond) > 0:
			line += strings.Join(cond, " and ")
		case item.Out:
			line += t.table(item.Table) + "  " + t.note(ref+" reads it")
		default:
			line += t.table(item.Table) + "  " + t.note("reads "+ref)
		}
		if item.Note != "" && item.Edge != "uses" {
			line += "  " + t.note(item.Note)
		}
		b.WriteString(line + "\n\n")
	}
	b.WriteString(t.describe(rel, cols))
	return b.String(), nil
}

// navActions are fzf actions showing the state's level.
func navActions(st *navState, self string) string {
	clean := strings.NewReplacer("(", "", ")", "")
	return "change-prompt(" + clean.Replace(st.prompt()) + "> )+clear-query+first+reload:" + self
}

func navCmd(c *cli.Context) error {
	if c.Args().First() == "preview" { // stateless
		item, err := parseNavItem(c.Args().Get(1))
		if err != nil {
			return nil
		}
		api, err := openCatalog(c)
		if err != nil {
			return err
		}
		defer api.close()
		text, err := previewText(api, item)
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil
	}
	path := c.String("state")
	st, err := loadNavState(path)
	if err != nil {
		return err
	}
	selfLines := self() + " _nav lines --state " + shellQuote(path) + passThrough(c)
	switch c.Args().First() {
	case "lines":
		api, err := openCatalog(c)
		if err != nil {
			return err
		}
		defer api.close()
		lines, err := navLines(api, st)
		if err != nil {
			return err
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil
	case "push":
		item, err := parseNavItem(c.Args().Get(1))
		if err != nil {
			return nil // e.g. an empty list
		}
		st.Path = append(st.Path, item)
	case "pop":
		if len(st.Path) == 0 {
			return nil
		}
		st.Path = st.Path[:len(st.Path)-1]
	default:
		return fmt.Errorf("unknown _nav action %q", c.Args().First())
	}
	if err := st.save(path); err != nil {
		return err
	}
	fmt.Print(navActions(st, selfLines))
	return nil
}

// runTablePicker runs fzf for a state and prints the choice.
func runTablePicker(c *cli.Context, api catalogAPI, st *navState) error {
	if st.Ctx.Insert == "" {
		st.Ctx.Insert = "join"
	}
	if v := c.String("insert"); v != "" {
		if v != "join" && v != "name" {
			return fmt.Errorf("--insert: join or name, not %q", v)
		}
		st.Ctx.Insert = v
	}
	lines, err := navLines(api, st)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "sqlh-pick-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := dir + "/state.json"
	if err := st.save(path); err != nil {
		return err
	}
	nav := self() + " _nav --state " + shellQuote(path) + passThrough(c)
	key, vals, err := pickKey(lines, pickOpts{
		prompt:  st.prompt(),
		header:  st.header(),
		display: "3",
		preview: nav + " preview {1}",
		expect:  []string{"ctrl-t"},
		binds: []string{
			"ctrl-r:transform:" + nav + " push {1}",
			"ctrl-o:transform:" + nav + " pop",
		},
	})
	if err != nil || len(vals) == 0 {
		return err
	}
	item, err := parseNavItem(vals[0])
	if err != nil {
		return err
	}
	if st.Copy {
		if err := copyToClipboard(item.Table); err != nil {
			return err
		}
		api.notify("copied " + item.Table + " to the clipboard")
		return nil
	}
	// the drill-down path as it was when accepting
	if final, err := loadNavState(path); err == nil {
		st.Path = final.Path
	}
	emit([]string{chooseInsert(st, item, key == "ctrl-t")}, "")
	return nil
}

// clipboardCommands: the first one found is used.
var clipboardCommands = [][]string{
	{"pbcopy"},
	{"wl-copy"},
	{"xclip", "-selection", "clipboard"},
	{"xsel", "--clipboard", "--input"},
}

// copyToClipboard puts text on the system clipboard.
func copyToClipboard(text string) error {
	for _, c := range clipboardCommands {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(context.Background(), path, c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w %s", c[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return errors.New("no clipboard command found (pbcopy, wl-copy, xclip or xsel)")
}

// chooseInsert is the text to insert for an accepted item: the join chain
// from the statement's tables to it after JOIN, else (or flipped: the other
// mode) its name.
func chooseInsert(st *navState, item navItem, flip bool) string {
	join := st.Ctx.Insert != "name"
	if flip {
		join = !join
	}
	space := ""
	if st.Ctx.NeedSpace {
		space = " "
	}
	if !join || !st.inJoin() {
		return space + item.Table
	}
	chain := append(append([]navItem(nil), st.Path...), item)
	if chain[0].Ref == "" || len(chain[0].Cols) == 0 {
		// not reached from the statement's tables: no condition to write
		return space + item.Table
	}
	steps := make([]catalog.JoinStep, len(chain))
	for i, n := range chain {
		steps[i] = catalog.JoinStep{Insert: n.Table, Cols: n.Cols}
		if i == 0 {
			steps[i].AnchorRef = n.Ref
		}
	}
	taken := map[string]bool{}
	for _, t := range st.Ctx.Tables {
		taken[strings.ToLower(t.Ref)] = true
		taken[strings.ToLower(t.Name)] = true
	}
	return space + catalog.ComposeJoin(steps, taken, st.Ctx.LowercaseKeywords, st.Ctx.Indent)
}
