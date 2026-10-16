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
//     goes back. It runs in sqlh's inline picker (internal/tui).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/database"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/tui"
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
			Name:      "ddl",
			Usage:     "print a table's or view's CREATE statement(s), as go to definition shows it",
			ArgsUsage: "TABLE",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "json", Usage: "machine-readable output"},
				&cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "find the workspace from `DIR`"},
				&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "the SQL file being edited: finds the workspace"},
				&cli.StringFlag{Name: "conn", Usage: "standalone: use connection `ALIAS`"},
				&cli.StringFlag{Name: "schema", Usage: "standalone: current schema `NAME` (one of the connection's schemas:)"},
				&cli.BoolFlag{Name: "standalone", Usage: "do not use a running server"},
			},
			Action: ddlCmd,
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

// navItem is a picker entry: a table, and how it joins the table it was
// listed for.
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

// pickItems makes picker lines: the table's name (what typing matches), with
// the anchor's columns when the table is listed more than once; the other
// tables are dimmed when related ones are listed. Values are indexes into
// entries.
func pickItems(entries []navEntry) []tui.PickItem {
	count := map[string]int{}
	anyRelated := false
	for _, e := range entries {
		count[e.item.Table]++
		anyRelated = anyRelated || e.related
	}
	items := make([]tui.PickItem, 0, len(entries))
	for i, e := range entries {
		display := e.item.Table
		if count[e.item.Table] > 1 && e.related {
			display += " " + ansiDim + "(" + e.detail + ")" + ansiReset
		}
		items = append(items, tui.PickItem{Value: strconv.Itoa(i), Display: display, Match: e.item.Table,
			Dim: anyRelated && !e.related})
	}
	return items
}

// navLines lists the picker's lines for the state.
func navLines(api catalogAPI, st *navState) ([]navEntry, []tui.PickItem, error) {
	entries, err := navEntries(api, st)
	if err != nil {
		return nil, nil, err
	}
	return entries, pickItems(entries), nil
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
	if len(all) == 0 && st.Schema == "" {
		// only system schemas have tables (e.g. PostgreSQL's postgres
		// database): list those rather than nothing
		if all, err = api.tables("", true); err != nil {
			return nil, err
		}
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

// runTablePicker runs the picker for a state and prints the choice.
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
	entries, items, err := navLines(api, st)
	if err != nil {
		return err
	}
	entryOf := func(it *tui.PickItem) (navItem, bool) {
		if it == nil {
			return navItem{}, false
		}
		i, err := strconv.Atoi(it.Value)
		if err != nil || i < 0 || i >= len(entries) {
			return navItem{}, false
		}
		return entries[i].item, true
	}
	reload := func() (*tui.PickReload, error) {
		es, its, err := navLines(api, st)
		if err != nil {
			return nil, err
		}
		entries = es
		return &tui.PickReload{Items: its, Prompt: st.prompt(), Header: st.header()}, nil
	}
	res, ok, err := tui.Pick(items, tui.PickOptions{
		Prompt: st.prompt(),
		Header: st.header(),
		Preview: func(it tui.PickItem) string {
			item, ok := entryOf(&it)
			if !ok {
				return ""
			}
			text, err := previewText(api, item)
			if err != nil {
				return err.Error()
			}
			return text
		},
		Expect: []string{"ctrl+t"},
		Bind: map[string]func(*tui.PickItem) (*tui.PickReload, error){
			"ctrl+r": func(it *tui.PickItem) (*tui.PickReload, error) {
				item, ok := entryOf(it)
				if !ok {
					return nil, nil
				}
				st.Path = append(st.Path, item)
				return reload()
			},
			"ctrl+o": func(*tui.PickItem) (*tui.PickReload, error) {
				if len(st.Path) == 0 {
					return nil, nil
				}
				st.Path = st.Path[:len(st.Path)-1]
				return reload()
			},
		},
	})
	if err != nil || !ok || len(res.Values) == 0 {
		return err
	}
	item, found := entryOf(&tui.PickItem{Value: res.Values[0]})
	if !found {
		return nil
	}
	if st.Copy {
		if err := copyToClipboard(item.Table); err != nil {
			return err
		}
		api.notify("copied " + item.Table + " to the clipboard")
		return nil
	}
	emit([]string{chooseInsert(st, item, res.Key == "ctrl+t")}, "")
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

func ddlCmd(c *cli.Context) error {
	name := strings.TrimSpace(c.Args().First())
	if name == "" {
		return errors.New("usage: sqlh ddl TABLE (or schema.table)")
	}
	var res *control.DDLResult
	var err error
	if c.Bool("standalone") || c.String("conn") != "" || c.String("schema") != "" {
		res, err = ddlStandalone(c, name)
	} else {
		cl, _, serr := server(c)
		switch {
		case errors.Is(serr, control.ErrNoServer):
			res, err = ddlStandalone(c, name)
		case serr != nil:
			return serr
		default:
			defer cl.Close()
			res = &control.DDLResult{}
			err = call(cl, control.MethodDDL, control.DDLParams{Table: name}, res)
		}
	}
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return printJSON(res)
	}
	fmt.Print(ddlText(res))
	return nil
}

// ddlText: the statements under a comment saying what and from where.
func ddlText(r *control.DDLResult) string {
	what := r.Name
	if r.Schema != "" {
		what = r.Schema + "." + r.Name
	}
	head := "-- " + what + " (" + r.Kind + ")"
	if r.Source != "" {
		head += ": " + r.Source
	}
	head += "\n"
	if r.Note != "" {
		head += "-- " + r.Note + "\n"
	}
	return head + r.DDL
}

// ddlStandalone asks the database on a connection of its own.
func ddlStandalone(c *cli.Context, name string) (*control.DDLResult, error) {
	cfg, err := loadConfig(c)
	if err != nil {
		return nil, err
	}
	_, conn, err := chooseConnection(cfg, workspaceRoot(startDir(c)), c.String("conn"))
	if err != nil {
		return nil, err
	}
	if conn, err = withSchema(conn, c.String("schema")); err != nil {
		return nil, err
	}
	dbc, err := database.Open(conn)
	if err != nil {
		return nil, err
	}
	defer dbc.Close()
	repo, err := database.CreateRepository(conn.Driver, dbc.Conn)
	if err != nil {
		return nil, err
	}
	if f, ok := repo.(database.SchemaFilterer); ok {
		f.SetSchemaFilter(conn.Schemas)
	}
	f, ok := repo.(database.DefinitionFetcher)
	if !ok {
		return nil, fmt.Errorf("%s: definitions are not supported", conn.Driver)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema, table := database.SplitQualified(name, conn.Driver)
	qualified := schema != ""
	if !qualified {
		if schema, err = repo.CurrentSchema(ctx); err != nil {
			return nil, err
		}
	}
	d, err := f.Definition(ctx, schema, table, database.DefinitionOptions{})
	if errors.Is(err, database.ErrNoObject) && !qualified {
		// not in the current schema: a synonym, as the database resolves it
		if sr, ok := repo.(database.SynonymResolver); ok {
			if owner, target, serr := sr.ResolveSynonym(ctx, table); serr == nil && owner != "" {
				d, err = f.Definition(ctx, owner, target, database.DefinitionOptions{})
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &control.DDLResult{Schema: d.Schema, Name: d.Name, Kind: string(d.Kind), Source: d.Source, Note: d.Note, DDL: d.DDL}, nil
}
