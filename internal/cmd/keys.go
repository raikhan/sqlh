package cmd

// `sqlh keys`: every key binding action of the config (panel.keys,
// picker.keys), its keys as configured and its default.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v2"

	"github.com/raikhan/sqlh/internal/config"
)

func keysCommand() *cli.Command {
	return &cli.Command{
		Name:      "keys",
		Usage:     "list the key binding actions for the config (panel.keys, picker.keys): keys as configured, defaults, what they do",
		ArgsUsage: "[panel | picker | connections | objects | statement | queries | history | search | form]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "machine-readable output"},
			&cli.BoolFlag{Name: "defaults", Usage: "the defaults, ignoring the config"},
		},
		Action: keysCmd,
	}
}

// keyRow is an action of a section.
type keyRow struct {
	Action  string   `json:"action"`
	Keys    []string `json:"keys"`
	Default []string `json:"default"`
	Help    string   `json:"help"`
}

// keySection: where the actions are set in the config.
type keySection struct {
	Config string   `json:"config"` // panel.keys, panel.keys.connections, …
	About  string   `json:"about"`
	Rows   []keyRow `json:"actions"`
}

var scopeAbout = map[string]string{
	"connections": "the Connections tab (1)", "objects": "the Objects tab (2)", "statement": "the Statement tab (3)",
	"queries": "the Queries tab (4)", "history": "the History tab (5)", "search": "while searching (/)",
	"form": "the connection form",
}

func keysCmd(c *cli.Context) error {
	cfg := &config.Config{}
	if !c.Bool("defaults") {
		loaded, err := loadConfig(c)
		switch {
		case err == nil:
			cfg = loaded
		case errors.Is(err, config.ErrNotFoundConfig) || strings.HasPrefix(err.Error(), "no config"):
		default:
			return err
		}
	}
	sections := keySections(cfg)
	if want := c.Args().First(); want != "" {
		var keep []keySection
		for _, s := range sections {
			if want == "panel" && strings.HasPrefix(s.Config, "panel.") || s.Config == want+".keys" ||
				strings.HasSuffix(s.Config, "."+want) {
				keep = append(keep, s)
			}
		}
		if len(keep) == 0 {
			return fmt.Errorf("no section %q (panel, picker, %s)", want, strings.Join(config.PanelScopes, ", "))
		}
		sections = keep
	}
	if c.Bool("json") {
		return printJSON(sections)
	}
	printKeySections(os.Stdout, sections)
	return nil
}

// keySections: the panel's actions in every tab, then each scope's own
// (and the common ones a scope sets differently), then the picker's.
func keySections(cfg *config.Config) []keySection {
	maps := cfg.Panel.KeyMaps()
	row := func(a string, keys []string, def []string, help string) keyRow {
		return keyRow{Action: a, Keys: keys, Default: def, Help: help}
	}
	common := keySection{Config: "panel.keys", About: "every tab (a key or a list; per tab: panel.keys.<tab>.<action>)"}
	// the common actions as the first tab has them (the same unless a tab sets its own)
	for _, a := range config.PanelCommonActions {
		common.Rows = append(common.Rows, row(a, maps[config.PanelScopes[0]][a], config.DefaultPanelKeys[a], config.PanelActionHelp[a]))
	}
	out := []keySection{common}
	for _, scope := range config.PanelScopes {
		s := keySection{Config: "panel.keys." + scope, About: scopeAbout[scope]}
		for _, a := range config.PanelOwnActions(scope) {
			s.Rows = append(s.Rows, row(a, maps[scope][a], config.DefaultPanelKeys[a], config.PanelActionHelp[a]))
		}
		if scope != "search" && scope != "form" {
			for _, a := range config.PanelCommonActions {
				if strings.Join(maps[scope][a], " ") != strings.Join(maps[config.PanelScopes[0]][a], " ") {
					s.Rows = append(s.Rows, row(a, maps[scope][a], config.DefaultPanelKeys[a], config.PanelActionHelp[a]+" (set for this tab)"))
				}
			}
		}
		out = append(out, s)
	}
	pk := cfg.Picker.KeyMap()
	picker := keySection{Config: "picker.keys", About: "the inline picker (tables -p, connect, …)"}
	for _, a := range config.PickerKeyScopes[0] {
		picker.Rows = append(picker.Rows, row(a, pk[a], config.DefaultPickerKeys[a], config.PickerActionHelp[a]))
	}
	return append(out, picker)
}

func printKeySections(w io.Writer, sections []keySection) {
	actionW, keysW := 0, 0
	for _, s := range sections {
		for _, r := range s.Rows {
			actionW = max(actionW, len(r.Action))
			keysW = max(keysW, len(strings.Join(r.Keys, ", ")))
		}
	}
	keysW = min(keysW, 24)
	for i, s := range sections {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s — %s\n", s.Config, s.About)
		for _, r := range s.Rows {
			keys := strings.Join(r.Keys, ", ")
			help := r.Help
			if keys != strings.Join(r.Default, ", ") {
				help += "  [default: " + strings.Join(r.Default, ", ") + "]"
			}
			fmt.Fprintf(w, "  %-*s  %-*s  %s\n", actionW, r.Action, keysW, keys, help)
		}
	}
}
