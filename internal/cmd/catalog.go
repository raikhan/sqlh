package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
)

// catalogAPI is implemented by the server (schema cache) and standalone
// (queries the database).
type catalogAPI interface {
	schemas(all bool) ([]control.SchemaItem, error)
	tables(schema string, all bool) ([]control.TableItem, error)
	columns(table string) ([]control.ColumnItem, error)
	databases() ([]string, error)
	relations(table string) (*control.RelationsResult, error)
	context(file string, line, col int) (*control.ContextResult, error)
	// notify shows a message in the editor's status line (standalone: stderr)
	notify(msg string)
	close()
}

type serverCatalog struct{ cl *control.Client }

func (s serverCatalog) schemas(all bool) (out []control.SchemaItem, err error) {
	err = call(s.cl, control.MethodSchemas, control.SchemasParams{All: all}, &out)
	return
}
func (s serverCatalog) tables(schema string, all bool) (out []control.TableItem, err error) {
	err = call(s.cl, control.MethodTables, control.TablesParams{Schema: schema, All: all}, &out)
	return
}
func (s serverCatalog) columns(table string) (out []control.ColumnItem, err error) {
	err = call(s.cl, control.MethodColumns, control.ColumnsParams{Table: table}, &out)
	return
}
func (s serverCatalog) databases() (out []string, err error) {
	err = call(s.cl, control.MethodDatabases, nil, &out)
	return
}
func (s serverCatalog) relations(table string) (out *control.RelationsResult, err error) {
	err = call(s.cl, control.MethodRelations, control.RelationsParams{Table: table}, &out)
	return
}
func (s serverCatalog) context(file string, line, col int) (out *control.ContextResult, err error) {
	abs, err := absPath(file)
	if err != nil {
		return nil, err
	}
	err = call(s.cl, control.MethodContext, control.ContextParams{File: abs, Line: line, Col: col}, &out)
	return
}
func (s serverCatalog) notify(msg string) {
	var ok string
	_ = call(s.cl, control.MethodNotify, control.NotifyParams{Message: msg}, &ok)
}
func (s serverCatalog) close() { s.cl.Close() }

// standaloneCatalog reads metadata through a repository on its own connection.
type standaloneCatalog struct {
	conn *database.DBConnection
	repo database.DBRepository
	src  *repoSource
	opt  catalog.Options
	cfg  *config.Config
}

func (s *standaloneCatalog) schemas(all bool) ([]control.SchemaItem, error) {
	o := s.opt
	o.All = all
	return catalog.Schemas(s.src, o), s.src.err
}
func (s *standaloneCatalog) tables(schema string, all bool) ([]control.TableItem, error) {
	o := s.opt
	o.All = all
	return catalog.Tables(s.src, schema, o), s.src.err
}
func (s *standaloneCatalog) columns(table string) ([]control.ColumnItem, error) {
	return catalog.Columns(s.src, table, s.opt)
}
func (s *standaloneCatalog) databases() ([]string, error) {
	return s.repo.Databases(context.Background())
}
func (s *standaloneCatalog) relations(table string) (*control.RelationsResult, error) {
	return catalog.Relations(s.src, table, s.opt, s.cfg.Relations.MaybeEnabled())
}
func (s *standaloneCatalog) context(file string, line, col int) (*control.ContextResult, error) {
	b, err := os.ReadFile(expandHome(file))
	if err != nil {
		return nil, err
	}
	res := catalog.Context(string(b), line, col)
	res.LowercaseKeywords = s.cfg.LowercaseKeywords
	res.Insert = s.cfg.Relations.InsertMode()
	res.Maybe = s.cfg.Relations.MaybeEnabled()
	return &res, nil
}
func (s *standaloneCatalog) notify(msg string) { fmt.Fprintln(os.Stderr, msg) }
func (s *standaloneCatalog) close()            { s.conn.Close() }

// repoSource implements catalog.Source with lazy, per-schema column loading.
type repoSource struct {
	repo    database.DBRepository
	def     string
	names   []string
	byTable map[string][]string
	cols    map[string]map[string][]*database.ColumnDesc // UPPER schema → UPPER table
	err     error
	graph   *database.RelGraph
}

// Relations builds the default schema's relationship graph (as the server's
// cache does, views included; this takes a few seconds on big schemas).
func (r *repoSource) Relations() (*database.RelGraph, error) {
	if r.graph != nil {
		return r.graph, nil
	}
	ctx := context.Background()
	gen := database.NewDBCacheUpdater(r.repo)
	cache, err := gen.GenerateDBCachePrimary(ctx)
	if err != nil {
		return nil, err
	}
	views, err := gen.GenerateViews(ctx, cache.DefaultSchema())
	if err != nil {
		views = []*database.ViewDef{}
	}
	r.graph = cache.BuildRelations(views)
	return r.graph, nil
}

func (r *repoSource) DefaultSchema() string             { return r.def }
func (r *repoSource) Schemas() []string                 { return r.names }
func (r *repoSource) SchemaTables() map[string][]string { return r.byTable }
func (r *repoSource) Columns(schema, table string) ([]*database.ColumnDesc, bool) {
	us := strings.ToUpper(schema)
	m, ok := r.cols[us]
	if !ok {
		descs, err := r.repo.DescribeDatabaseTableBySchema(context.Background(), schema)
		if err != nil {
			r.err = err
			return nil, false
		}
		m = map[string][]*database.ColumnDesc{}
		for _, d := range descs {
			k := strings.ToUpper(d.Table)
			m[k] = append(m[k], d)
		}
		r.cols[us] = m
	}
	c, ok := m[strings.ToUpper(table)]
	return c, ok
}

func openStandaloneCatalog(c *cli.Context) (*standaloneCatalog, error) {
	cfg, err := loadConfig(c)
	if err != nil {
		return nil, err
	}
	_, conn, err := chooseConnection(cfg, workspaceRoot(startDir(c)), c.String("conn"))
	if err != nil {
		return nil, err
	}
	dbc, err := database.Open(conn)
	if err != nil {
		return nil, err
	}
	repo, err := database.CreateRepository(conn.Driver, dbc.Conn)
	if err != nil {
		dbc.Close()
		return nil, err
	}
	if f, ok := repo.(database.SchemaFilterer); ok {
		f.SetSchemaFilter(conn.Schemas)
	}
	ctx := context.Background()
	src := &repoSource{repo: repo, cols: map[string]map[string][]*database.ColumnDesc{}}
	if src.def, err = repo.CurrentSchema(ctx); err != nil {
		dbc.Close()
		return nil, err
	}
	if src.names, err = repo.Schemas(ctx); err != nil {
		dbc.Close()
		return nil, err
	}
	if src.byTable, err = repo.SchemaTables(ctx); err != nil {
		dbc.Close()
		return nil, err
	}
	return &standaloneCatalog{conn: dbc, repo: repo, src: src, cfg: cfg,
		opt: catalog.Options{Driver: conn.Driver, Lower: cfg.LowercaseIdentifiers}}, nil
}

// openCatalog prefers the workspace's server.
func openCatalog(c *cli.Context) (catalogAPI, error) {
	if !c.Bool("standalone") && c.String("conn") == "" {
		cl, _, err := server(c)
		if err == nil {
			return serverCatalog{cl}, nil
		}
		if !errors.Is(err, control.ErrNoServer) {
			return nil, err
		}
	}
	return openStandaloneCatalog(c)
}

// passThrough are the flags a preview subprocess must inherit.
func passThrough(c *cli.Context) string {
	var b strings.Builder
	if d := startDir(c); d != "" {
		b.WriteString(" -C " + shellQuote(d))
	}
	if v := c.String("conn"); v != "" {
		b.WriteString(" --conn " + shellQuote(v))
	}
	if c.Bool("standalone") {
		b.WriteString(" --standalone")
	}
	if v := c.String("config"); v != "" {
		return " --config " + shellQuote(v) + b.String()
	}
	return b.String()
}

func catalogFlags(extra ...cli.Flag) []cli.Flag {
	return append([]cli.Flag{
		&cli.BoolFlag{Name: "pick", Aliases: []string{"p"}, Usage: "choose with fzf and print the choice (for Helix :insert-output)"},
		&cli.BoolFlag{Name: "long", Usage: "tab-separated details"},
		&cli.BoolFlag{Name: "json", Usage: "machine-readable output"},
		&cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "find the workspace from `DIR`"},
		&cli.StringFlag{Name: "conn", Usage: "standalone: use connection `ALIAS`"},
		&cli.BoolFlag{Name: "standalone", Usage: "do not use a running server"},
		&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "the SQL file being edited: finds the workspace; for tables -p with --line, the statement's context (the editor's buffer if open)"},
		editorFlag(""),
	}, extra...)
}

func catalogCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "tables",
			Usage:     "list tables and views as you would type them (-p: pick one; with --file/--line/--col related tables first, JOIN clauses after JOIN)",
			ArgsUsage: " ",
			Flags: catalogFlags(append([]cli.Flag{
				&cli.StringFlag{Name: "schema", Aliases: []string{"s"}, Usage: "only `SCHEMA`"},
				&cli.BoolFlag{Name: "all", Usage: "include system schemas"},
			}, tablePickFlags()...)...),
			Action: editorAction(tablesCmd),
		},
		{
			Name:      "columns",
			Usage:     "list a table's columns (-p: multi-pick, no TABLE: pick the table first)",
			ArgsUsage: "[TABLE]",
			Flags: catalogFlags(
				&cli.StringFlag{Name: "prefix", Usage: "prepend `STR` to picked columns, e.g. p."},
				&cli.StringFlag{Name: "sep", Value: ", ", Usage: "separator between picked columns"},
			),
			Action: editorAction(columnsCmd),
		},
		{
			Name:   "schemas",
			Usage:  "list schemas (-p: pick one)",
			Flags:  catalogFlags(&cli.BoolFlag{Name: "all", Usage: "include system schemas"}),
			Action: editorAction(schemasCmd),
		},
		{
			Name:   "databases",
			Usage:  "list databases on the server (-p: pick one)",
			Flags:  catalogFlags(),
			Action: editorAction(databasesCmd),
		},
		{
			Name:      "db",
			Usage:     "switch the editor's connection to another database (no NAME: pick)",
			ArgsUsage: "[NAME]",
			Flags:     []cli.Flag{&cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "find the workspace from `DIR`"}, headerFileFlag, editorFlag("")},
			Action:    editorAction(dbCmd),
		},
		{
			Name:      "schema",
			Usage:     "switch the editor's current schema among the connection's `schemas:` (no NAME: pick)",
			ArgsUsage: "[NAME]",
			Flags:     []cli.Flag{&cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "find the workspace from `DIR`"}, headerFileFlag, editorFlag("")},
			Action:    editorAction(schemaCmd),
		},
	}
}

func tablesCmd(c *cli.Context) error {
	api, err := openCatalog(c)
	if err != nil {
		return err
	}
	defer api.close()
	tables, err := api.tables(c.String("schema"), c.Bool("all"))
	if err != nil {
		return err
	}
	switch {
	case c.Bool("json"):
		return printJSON(tables)
	case c.Bool("pick"):
		st := &navState{Related: c.Bool("related"), Schema: c.String("schema")}
		if f := c.String("file"); f != "" && c.Int("line") > 0 {
			ctx, err := api.context(f, c.Int("line"), max(c.Int("col"), 1))
			if err != nil {
				return err
			}
			st.Ctx = *ctx
			if ctx.Focus != "" && !ctx.InJoin {
				// on a table of the statement: start with its relationships
				// (Ctrl-O: the full list); accepting copies the name, since
				// inserting at the cursor would split the one under it. A
				// name that isn't a known table (a typo, a schema) gets the
				// full list.
				if _, err := api.relations(focusName(ctx)); err == nil {
					st.Path = []navItem{{Table: focusName(ctx)}}
					st.Copy = true
				}
			}
		}
		return runTablePicker(c, api, st)
	case c.Bool("long"):
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, t := range tables {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d cols\n", t.Insert, t.Schema, t.Name, t.Columns)
		}
		return w.Flush()
	default:
		for _, t := range tables {
			fmt.Println(t.Insert)
		}
	}
	return nil
}

func columnsCmd(c *cli.Context) error {
	api, err := openCatalog(c)
	if err != nil {
		return err
	}
	defer api.close()
	table := c.Args().First()
	if table == "" {
		if !c.Bool("pick") {
			return errors.New("give a table (or use -p to pick one)")
		}
		tables, err := api.tables("", false)
		if err != nil {
			return err
		}
		lines := make([]string, len(tables))
		for i, t := range tables {
			lines[i] = fmt.Sprintf("%s\t%-40s %d cols", t.Insert, t.Insert, t.Columns)
		}
		vals, err := pick(lines, pickOpts{prompt: "table", header: "pick a table, then its columns",
			preview: self() + " describe" + passThrough(c) + " {1}"})
		if err != nil || len(vals) == 0 {
			return err
		}
		table = vals[0]
	}
	cols, err := api.columns(table)
	if err != nil {
		return err
	}
	switch {
	case c.Bool("json"):
		return printJSON(cols)
	case c.Bool("pick"):
		lines := make([]string, len(cols))
		for i, col := range cols {
			pk := ""
			if isPK(col.Key) {
				pk = "  PK"
			}
			lines[i] = fmt.Sprintf("%s\t%-32s %s%s", c.String("prefix")+col.Insert, col.Insert, col.Type, pk)
		}
		vals, err := pick(lines, pickOpts{prompt: table, header: "Tab: select · Ctrl-A: all · Enter: insert", multi: true})
		if err != nil {
			return err
		}
		emit(vals, c.String("sep"))
	case c.Bool("long"):
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, col := range cols {
			pk := ""
			if isPK(col.Key) {
				pk = "PK"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", col.Insert, col.Type, pk)
		}
		return w.Flush()
	default:
		for _, col := range cols {
			fmt.Println(c.String("prefix") + col.Insert)
		}
	}
	return nil
}

func isPK(key string) bool {
	k := strings.ToUpper(key)
	return k == "YES" || k == "PRI" || k == "P"
}

func schemasCmd(c *cli.Context) error {
	api, err := openCatalog(c)
	if err != nil {
		return err
	}
	defer api.close()
	schemas, err := api.schemas(c.Bool("all"))
	if err != nil {
		return err
	}
	switch {
	case c.Bool("json"):
		return printJSON(schemas)
	case c.Bool("pick"):
		lines := make([]string, len(schemas))
		for i, s := range schemas {
			def := ""
			if s.Default {
				def = "  (default)"
			}
			lines[i] = s.Name + "\t" + s.Name + def
		}
		vals, err := pick(lines, pickOpts{prompt: "schema", preview: self() + " tables --long" + passThrough(c) + " --schema {1}"})
		if err != nil {
			return err
		}
		emit(vals, "\n")
	default:
		for _, s := range schemas {
			if c.Bool("long") && s.Default {
				fmt.Println(s.Name + "\t(default)")
			} else {
				fmt.Println(s.Name)
			}
		}
	}
	return nil
}

func databasesCmd(c *cli.Context) error {
	api, err := openCatalog(c)
	if err != nil {
		return err
	}
	defer api.close()
	dbs, err := api.databases()
	if err != nil {
		return err
	}
	switch {
	case c.Bool("json"):
		return printJSON(dbs)
	case c.Bool("pick"):
		lines := make([]string, len(dbs))
		for i, d := range dbs {
			lines[i] = d + "\t" + d
		}
		vals, err := pick(lines, pickOpts{prompt: "database"})
		if err != nil {
			return err
		}
		emit(vals, "\n")
	default:
		for _, d := range dbs {
			fmt.Println(d)
		}
	}
	return nil
}

func dbCmd(c *cli.Context) error {
	cl, _, err := server(c)
	if err != nil {
		if errors.Is(err, control.ErrNoServer) {
			return errors.New("no running sqlh server for this workspace (open a .sql file in the editor first)")
		}
		return err
	}
	defer cl.Close()
	name := c.Args().First()
	if name == "" {
		var dbs []string
		if err := call(cl, control.MethodDatabases, nil, &dbs); err != nil {
			return err
		}
		lines := make([]string, len(dbs))
		for i, d := range dbs {
			lines[i] = d + "\t" + d
		}
		vals, err := pick(lines, pickOpts{prompt: "database"})
		if err != nil || len(vals) == 0 {
			return err
		}
		name = vals[0]
	}
	file, err := optionalAbs(c.String("file"))
	if err != nil {
		return err
	}
	var msg string
	if err := call(cl, control.MethodUseDatabase, control.DatabaseParams{Name: name, File: file}, &msg); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, msg)
	return nil
}

func schemaCmd(c *cli.Context) error {
	cl, _, err := server(c)
	if err != nil {
		if errors.Is(err, control.ErrNoServer) {
			return errors.New("no running sqlh server for this workspace (open a .sql file in the editor first; standalone runs take --schema)")
		}
		return err
	}
	defer cl.Close()
	name := c.Args().First()
	if name == "" {
		var choices []control.SchemaItem
		if err := call(cl, control.MethodSchemaChoices, nil, &choices); err != nil {
			return err
		}
		lines := make([]string, len(choices))
		for i, sc := range choices {
			mark := " "
			if sc.Default {
				mark = "*"
			}
			lines[i] = sc.Name + "\t" + mark + " " + sc.Name
		}
		vals, err := pick(lines, pickOpts{prompt: "schema"})
		if err != nil || len(vals) == 0 {
			return err
		}
		name = vals[0]
	}
	file, err := optionalAbs(c.String("file"))
	if err != nil {
		return err
	}
	var msg string
	if err := call(cl, control.MethodUseSchema, control.DatabaseParams{Name: name, File: file}, &msg); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, msg)
	return nil
}

// focusName is the focused table as the statement names it (schema.table).
func focusName(ctx *control.ContextResult) string {
	for _, t := range ctx.Tables {
		if strings.EqualFold(t.Name, ctx.Focus) && t.Schema != "" {
			return t.Schema + "." + t.Name
		}
	}
	return ctx.Focus
}
