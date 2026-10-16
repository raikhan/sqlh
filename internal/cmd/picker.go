package cmd

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/tui"
)

// pick lets the user choose among items with sqlh's inline picker (drawn on
// /dev/tty, so it works from Helix's :insert-output as well as from a
// terminal) and returns the chosen values; nil when cancelled.
// SQLH_PICK=<query> picks without a UI (scripts, tests).
func pick(items []tui.PickItem, o tui.PickOptions) ([]string, error) {
	if o.Keys == nil {
		o.Keys = pickerKeymap()
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("nothing to pick from")
	}
	res, ok, err := tui.Pick(items, o)
	if err != nil || !ok {
		return nil, err
	}
	return res.Values, nil
}

// items makes picker items of values (shown as they are).
func items(values ...string) []tui.PickItem {
	out := make([]tui.PickItem, len(values))
	for i, v := range values {
		out[i] = tui.PickItem{Value: v}
	}
	return out
}

// emit prints picked values. Into a pipe (Helix :insert-output) there is no
// trailing newline, so exactly the text is inserted.
func emit(vals []string, sep string) {
	if len(vals) == 0 {
		return
	}
	s := strings.Join(vals, sep)
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		s += "\n"
	}
	fmt.Print(s)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// appCtx: the command line (set before any command runs), for the config's
// picker keys.
var appCtx *cli.Context

// Before records the command line; the app runs it before any command.
func Before(c *cli.Context) error {
	appCtx = c
	return nil
}

var (
	pickerKeysOnce sync.Once
	pickerKeysMap  map[string][]string
)

// pickerKeymap: picker.keys of the config (none when it can't be read).
func pickerKeymap() map[string][]string {
	pickerKeysOnce.Do(func() {
		var cfg *config.Config
		var err error
		if appCtx != nil {
			cfg, err = loadConfig(appCtx)
		} else {
			cfg, err = config.GetDefaultConfig()
		}
		if err == nil && cfg != nil {
			pickerKeysMap = cfg.Picker.KeyMap()
		}
	})
	return pickerKeysMap
}

// pickKeys: the keys of a picker action.
func pickKeys(action string) []string { return tui.PickerKeys(pickerKeymap(), action) }

// pickKeyLabel: the first key of a picker action, for headers (Ctrl-T).
func pickKeyLabel(action string) string {
	ks := pickKeys(action)
	if len(ks) == 0 {
		return "?"
	}
	k := ks[0]
	if strings.HasPrefix(k, "ctrl+") {
		return "Ctrl-" + strings.ToUpper(k[5:])
	}
	if k == "enter" {
		return "Enter"
	}
	return k
}

// bindKeys: picker bindings by action → by key.
func bindKeys(byAction map[string]func(*tui.PickItem) (*tui.PickReload, error)) map[string]func(*tui.PickItem) (*tui.PickReload, error) {
	out := map[string]func(*tui.PickItem) (*tui.PickReload, error){}
	for a, fn := range byAction {
		for _, k := range pickKeys(a) {
			out[k] = fn
		}
	}
	return out
}
