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
func (s serverCatalog) close() { s.cl.Close() }

// standaloneCatalog reads metadata through a repository on its own connection.
type standaloneCatalog struct {
	conn *database.DBConnection
	repo database.DBRepository
	src  *repoSource
	opt  catalog.Options
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
func (s *standaloneCatalog) close() { s.conn.Close() }

// repoSource implements catalog.Source with lazy, per-schema column loading.
type repoSource struct {
	repo    database.DBRepository
	def     string
	names   []string
	byTable map[string][]string
	cols    map[string]map[string][]*database.ColumnDesc // UPPER schema → UPPER table
	err     error
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
	return &standaloneCatalog{conn: dbc, repo: repo, src: src,
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
	}, extra...)
}

func catalogCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "tables",
			Usage:     "list tables and views as you would type them (-p: pick one, columns preview)",
			ArgsUsage: " ",
			Flags: catalogFlags(
				&cli.StringFlag{Name: "schema", Aliases: []string{"s"}, Usage: "only `SCHEMA`"},
				&cli.BoolFlag{Name: "all", Usage: "include system schemas"},
			),
			Action: tablesCmd,
		},
		{
			Name:      "columns",
			Usage:     "list a table's columns (-p: multi-pick, no TABLE: pick the table first)",
			ArgsUsage: "[TABLE]",
			Flags: catalogFlags(
				&cli.StringFlag{Name: "prefix", Usage: "prepend `STR` to picked columns, e.g. p."},
				&cli.StringFlag{Name: "sep", Value: ", ", Usage: "separator between picked columns"},
			),
			Action: columnsCmd,
		},
		{
			Name:   "schemas",
			Usage:  "list schemas (-p: pick one)",
			Flags:  catalogFlags(&cli.BoolFlag{Name: "all", Usage: "include system schemas"}),
			Action: schemasCmd,
		},
		{
			Name:   "databases",
			Usage:  "list databases on the server (-p: pick one)",
			Flags:  catalogFlags(),
			Action: databasesCmd,
		},
		{
			Name:      "db",
			Usage:     "switch the editor's connection to another database (no NAME: pick)",
			ArgsUsage: "[NAME]",
			Flags:     []cli.Flag{&cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "find the workspace from `DIR`"}},
			Action:    dbCmd,
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
		lines := make([]string, len(tables))
		for i, t := range tables {
			where := t.Schema
			if t.Default {
				where = "default schema"
			}
			lines[i] = fmt.Sprintf("%s\t%-40s %s · %d cols", t.Insert, t.Insert, where, t.Columns)
		}
		vals, err := pick(lines, pickOpts{prompt: "table", header: "Enter: insert · Esc: cancel",
			preview: self() + " columns --long" + passThrough(c) + " {1}"})
		if err != nil {
			return err
		}
		emit(vals, "\n")
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
			preview: self() + " columns --long" + passThrough(c) + " {1}"})
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
	var msg string
	if err := call(cl, control.MethodUseDatabase, control.DatabaseParams{Name: name}, &msg); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, msg)
	return nil
}
