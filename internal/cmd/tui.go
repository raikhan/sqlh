package cmd

// `sqlh tui`: the panel as a standalone app. Without an editor's server for
// the workspace it runs a headless one in-process (handing the socket over
// when Helix starts); `n` creates a query file and opens it in an editor pane
// the app then belongs to, as its panel.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/handler"
	"github.com/raikhan/sqlh/internal/herdr"
	"github.com/raikhan/sqlh/internal/tui"
)

func tuiCommand() *cli.Command {
	return &cli.Command{
		Name:  "tui",
		Usage: "the database browser as an app: object tree, previews, history, query files (n: new query, opened in Helix next to it)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "dir", Aliases: []string{"C"}, Usage: "the workspace: `DIR` (default: the project around the current directory)"},
		},
		Action: tuiCmd,
	}
}

func tuiCmd(c *cli.Context) error {
	ws := workspaceRoot(startDir(c))
	cfg, err := loadConfig(c)
	if err != nil {
		return err
	}
	defer logAway(c)()
	be := &tuiBackend{cfg: cfg, cfgPath: c.String("config")}
	if be.cfgPath == "" {
		be.cfgPath = config.YamlConfigPath
	}
	be.panelBackend = &panelBackend{ws: ws, events: make(chan control.Event, 64)}
	if hd, err := herdr.FromEnv(); err == nil {
		be.hd = hd
	}
	be.startServer = be.startHeadless
	defer be.stopHeadless()
	if _, err := be.connect(); err != nil {
		return err
	}
	st, err := serverStatus(be.client())
	if err != nil {
		return err
	}
	be.settings = *st.Panel
	be.insertKeys = st.Panel.InsertKeys
	go be.watch()
	p := tui.NewPanel(be, be.events, tui.Options{
		Icons: st.Panel.Icons, Keys: st.Panel.Keys, Workspace: ws, Standalone: true,
	})
	_, err = tea.NewProgram(p, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

// logAway sends the log to a file (--log, else {cache}/sqlh/tui.log) while
// a Bubble Tea program owns the terminal: a line on stderr would be drawn
// over the screen. It returns the cleanup.
func logAway(c *cli.Context) func() {
	path := c.String("log")
	if path == "" {
		path = filepath.Join(control.Dir(), "tui.log")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.SetOutput(io.Discard)
		return func() {}
	}
	log.SetOutput(f)
	return func() { log.SetOutput(os.Stderr); f.Close() }
}

// tuiBackend is the panel's backend plus an editor opened on demand (and
// detached when it exits) and a headless server.
type tuiBackend struct {
	*panelBackend
	cfg     *config.Config
	cfgPath string // the config file (--config, else the default)

	hmu      sync.Mutex
	headless *handler.Server
}

func (b *tuiBackend) startHeadless() error {
	b.hmu.Lock()
	defer b.hmu.Unlock()
	if b.headless != nil && !b.headless.Released() {
		return nil
	}
	if b.headless != nil {
		_ = b.headless.Stop()
	}
	// the config as the file has it now (the app may have edited it)
	cfg := b.cfg
	if fresh, err := config.GetConfig(b.cfgPath); err == nil {
		cfg = fresh
	}
	s, err := handler.RunHeadless(cfg, b.cfgPath, b.ws)
	if err != nil {
		return err
	}
	b.headless = s
	return nil
}

func (b *tuiBackend) stopHeadless() {
	b.hmu.Lock()
	defer b.hmu.Unlock()
	if b.headless != nil {
		_ = b.headless.Stop()
		b.headless = nil
	}
}

// EditorAlive: the app outlives its editor; a closed one is detached.
func (b *tuiBackend) EditorAlive() bool {
	if pane, _ := b.editor(); pane != "" && !b.editorRuns() {
		b.setEditor("", 0)
		b.setFile("")
		b.resubscribe()
	}
	return true
}

// Act: without an editor the panel copies instead (it doesn't get here).
func (b *tuiBackend) Act(a control.PanelAction) error {
	if pane, _ := b.editor(); pane == "" {
		return errors.New("no editor: open a query (4) or create one (n)")
	}
	return b.panelBackend.Act(a)
}

// queryFiles lists a folder's .sql files, newest first.
func queryFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{filepath.Join(dir, e.Name()), fi.ModTime()})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out, nil
}

var queryFileRe = regexp.MustCompile(`^query_([0-9]+)\.sql$`)

// nextQueryNumber: 1 + the highest N of the folder's query_<N>.sql files
// (1 when there are none; gaps aren't filled).
func nextQueryNumber(dir string) int {
	n := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if m := queryFileRe.FindStringSubmatch(e.Name()); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil && v > n {
				n = v
			}
		}
	}
	return n + 1
}

// newQueryFile creates the next query_<N>.sql of dir: the connection
// header, then body.
func newQueryFile(dir, alias, body string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	head := ""
	if alias != "" {
		head = "-- sqlh: conn=" + alias + "\n\n"
	}
	for tries := 0; tries < 10; tries++ {
		path := filepath.Join(dir, fmt.Sprintf("query_%d.sql", nextQueryNumber(dir)))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue // created meanwhile
		}
		if err != nil {
			return "", err
		}
		_, werr := f.WriteString(head + body)
		if err := f.Close(); werr == nil {
			werr = err
		}
		return path, werr
	}
	return "", errors.New("could not create a query file")
}

// OpenFile opens path in the attached editor, or in a new editor pane next
// to the app (which then belongs to it).
func (b *tuiBackend) OpenFile(path string) error {
	if b.hd == nil {
		return errors.New("opening files needs Herdr: open " + path + " in your editor")
	}
	if pane, _ := b.editor(); pane != "" && b.editorRuns() {
		return b.panelBackend.OpenFile(path)
	}
	self := herdr.CurrentPane()
	if self == "" {
		return errors.New("no Herdr pane")
	}
	argv := append(append([]string(nil), b.settings.Editor...), path)
	if len(b.settings.Editor) == 0 {
		argv = append(b.cfg.TUI.EditorOrDefault(), path)
	}
	pane, err := splitEditor(b.hd, self, b.settings.Placement, b.ws, argv, self)
	if err != nil {
		return err
	}
	// the startup hook runs it; else type it
	if !waitProcess(b.hd, pane, filepath.Base(argv[0]), 3*time.Second) {
		if err := b.hd.SendText(pane, "exec "+shellLine(argv)); err != nil {
			return err
		}
		if err := b.hd.SendKeys(pane, "enter"); err != nil {
			return err
		}
	}
	b.setEditor(pane, 0)
	b.setFile(path)
	b.resubscribe()
	return b.hd.Focus(pane)
}

// splitEditor opens an editor pane next to the app, on the side opposite
// the panel's placement (half of the app's pane).
func splitEditor(hd *herdr.Client, self, placement, cwd string, argv []string, origin string) (string, error) {
	dir, swap := "right", true // the panel on the right: the editor left of it
	switch placement {
	case "left":
		swap = false
	case "down":
		dir = "down"
	case "up":
		dir, swap = "down", false
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return "", err
	}
	pane, err := hd.Split(herdr.SplitOptions{Target: self, Direction: dir, Ratio: 0.5, Cwd: cwd, Focus: true,
		// the startup hook needs both (HELIX_ORIGIN_PANE_ID: the pane it came from)
		Env: map[string]string{"HELIX_HELPER_ARGV": string(argvJSON), "HELIX_ORIGIN_PANE_ID": origin}})
	if err != nil {
		return "", err
	}
	if swap {
		if err := hd.Swap(pane, self); err != nil {
			return pane, err
		}
	}
	return pane, nil
}

// waitProcess waits for a pane's foreground to run name.
func waitProcess(hd *herdr.Client, pane, name string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if info, err := hd.ProcessInfo(pane); err == nil && info.HasProcess(name, 0) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// helixQuote quotes a path for Helix's command line (single quotes doubled).
func helixQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

var _ tui.Backend = (*tuiBackend)(nil)
