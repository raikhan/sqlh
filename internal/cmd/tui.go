package cmd

// `sqlh tui`: the panel as a standalone app. Without an editor's server for
// the workspace it runs a headless one in-process (handing the socket over
// when Helix starts); `n` creates a query file and opens it in an editor pane
// the app then belongs to, as its panel.

import (
	"encoding/json"
	"errors"
	"fmt"
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
	be := &tuiBackend{cfg: cfg}
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
		Icons: st.Panel.Icons, BackToEditorKey: st.Panel.BackToEditor, Workspace: ws, Standalone: true,
	})
	_, err = tea.NewProgram(p, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

// tuiBackend is the panel's backend plus query files, an editor opened on
// demand and a headless server.
type tuiBackend struct {
	*panelBackend
	cfg      *config.Config
	settings control.PanelSettings

	hmu      sync.Mutex
	headless *handler.Server
	file     string // the file last opened in the editor (under hmu)
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
	s, err := handler.RunHeadless(b.cfg, b.ws)
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

func (b *tuiBackend) setFile(f string) {
	b.hmu.Lock()
	b.file = f
	b.hmu.Unlock()
}

func (b *tuiBackend) Editor() string {
	pane, _ := b.editor()
	if pane == "" {
		return ""
	}
	b.hmu.Lock()
	defer b.hmu.Unlock()
	if b.file != "" {
		return shortName(b.file)
	}
	return pane
}

func shortName(p string) string { return filepath.Base(p) }

func (b *tuiBackend) queriesDir() string {
	if b.settings.QueriesDir != "" {
		return b.settings.QueriesDir
	}
	return b.cfg.TUI.QueriesDirIn(b.ws)
}

// QueryFiles lists the query folder's .sql files, newest first.
func (b *tuiBackend) QueryFiles() ([]string, error) {
	dir := b.queriesDir()
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

// NewQuery creates the next query_<N>.sql: the connection header, then body.
func (b *tuiBackend) NewQuery(body string) (string, error) {
	dir := b.queriesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	st, err := serverStatus(b.client())
	if err != nil {
		return "", err
	}
	head := ""
	if st.Connection != nil && st.Connection.Alias != "" {
		head = "-- sqlh: conn=" + st.Connection.Alias + "\n\n"
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
		cmd := "open " + helixQuote(path)
		if err := b.hd.SendKeys(pane, "esc", ":"); err != nil {
			return err
		}
		if err := b.hd.SendText(pane, cmd); err != nil {
			return err
		}
		if err := b.hd.SendKeys(pane, "enter"); err != nil {
			return err
		}
		b.setFile(path)
		return b.hd.Focus(pane)
	}
	self := herdr.CurrentPane()
	if self == "" {
		return errors.New("no Herdr pane")
	}
	argv := append(append([]string(nil), b.settings.Editor...), path)
	if len(b.settings.Editor) == 0 {
		argv = append(b.cfg.TUI.EditorOrDefault(), path)
	}
	pane, err := splitEditor(b.hd, self, b.settings.Placement, b.ws, argv)
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
func splitEditor(hd *herdr.Client, self, placement, cwd string, argv []string) (string, error) {
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
		Env: map[string]string{"HELIX_HELPER_ARGV": string(argvJSON)}})
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

var _ tui.Workspace = (*tuiBackend)(nil)
