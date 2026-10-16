package tui

import (
	"strings"

	"github.com/raikhan/sqlh/internal/config"
)

// keymap resolves keys to actions, per scope (the panel's tabs, its search,
// its form; the picker's one).
type keymap struct {
	keys   []map[string][]string // per scope: action → keys
	lookup []map[string]string   // per scope: key → action
}

// the panel's scopes besides its tabs (0 … numMainTabs-1)
const (
	scopeSearch = numMainTabs
	scopeForm   = numMainTabs + 1
)

func newKeymap(scopes []map[string][]string) keymap {
	var km keymap
	for _, sc := range scopes {
		m := map[string]string{}
		for a, ks := range sc {
			for _, k := range ks {
				m[k] = a
			}
		}
		km.keys = append(km.keys, sc)
		km.lookup = append(km.lookup, m)
	}
	return km
}

// panelKeymap: the scopes of config.PanelScopes (tabs in order, search,
// form); missing ones get the defaults.
func panelKeymap(set map[string]map[string][]string) keymap {
	def := config.PanelKeys{}.Keymaps()
	var scopes []map[string][]string
	for _, name := range config.PanelScopes {
		sc := map[string][]string{}
		for a, k := range def[name] {
			sc[a] = k
		}
		for a, k := range set[name] {
			if _, ok := sc[a]; ok {
				sc[a] = k
			}
		}
		scopes = append(scopes, sc)
	}
	return newKeymap(scopes)
}

// pickerKeymap: one scope, the defaults with set's replaced.
func pickerKeymap(set map[string][]string) keymap {
	sc := map[string][]string{}
	for a, k := range config.DefaultPickerKeys {
		sc[a] = k
	}
	for a, k := range set {
		if len(k) > 0 {
			sc[a] = k
		}
	}
	return newKeymap([]map[string][]string{sc})
}

// action: the action of a key in a scope ("" = none).
func (k keymap) action(scope int, key string) string {
	if scope < 0 || scope >= len(k.lookup) {
		return ""
	}
	return k.lookup[scope][key]
}

// has: the scope has the action.
func (k keymap) has(scope int, action string) bool {
	if scope < 0 || scope >= len(k.keys) {
		return false
	}
	_, ok := k.keys[scope][action]
	return ok
}

// labelIn: the action's first key in a scope ("" = not there).
func (k keymap) labelIn(scope int, action string) string {
	if !k.has(scope, action) || len(k.keys[scope][action]) == 0 {
		return ""
	}
	return keyLabel(k.keys[scope][action][0])
}

// label: the action's first key in the first scope that has it.
func (k keymap) label(action string, prefer ...int) string {
	for _, sc := range prefer {
		if l := k.labelIn(sc, action); l != "" {
			return l
		}
	}
	for sc := range k.keys {
		if l := k.labelIn(sc, action); l != "" {
			return l
		}
	}
	return "?"
}

// labelsIn: all of the action's keys in a scope.
func (k keymap) labelsIn(scope int, action string) string {
	if !k.has(scope, action) {
		return ""
	}
	var out []string
	for _, x := range k.keys[scope][action] {
		out = append(out, keyLabel(x))
	}
	return strings.Join(out, " ")
}

// k: an action's key as the current tab has it (for hints).
func (p *Panel) k(action string) string {
	return p.km.label(action, p.cur, scopeSearch, scopeForm)
}

func keyLabel(k string) string {
	switch k {
	case "backspace":
		return "⌫"
	case " ":
		return "space"
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "left":
		return "←"
	case "right":
		return "→"
	}
	return k
}
