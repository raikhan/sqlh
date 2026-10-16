package tui

import (
	"strings"

	"github.com/raikhan/sqlh/internal/config"
)

// keymap resolves keys to actions, per scope (the panel's main keys, its
// search, its form; the picker's).
type keymap struct {
	keys   map[string][]string // action → keys
	scopes []map[string]string // key → action
}

const (
	scopeMain = iota
	scopeSearch
	scopeForm
)

func newKeymap(set map[string][]string, def config.Keymap, scopes [][]string) keymap {
	km := keymap{keys: map[string][]string{}}
	for a, k := range def {
		km.keys[a] = k
	}
	for a, k := range set {
		if len(k) > 0 {
			km.keys[a] = k
		}
	}
	for _, sc := range scopes {
		m := map[string]string{}
		for _, a := range sc {
			for _, k := range km.keys[a] {
				m[k] = a
			}
		}
		km.scopes = append(km.scopes, m)
	}
	return km
}

func panelKeymap(set map[string][]string) keymap {
	return newKeymap(set, config.DefaultPanelKeys, config.PanelKeyScopes)
}

func pickerKeymap(set map[string][]string) keymap {
	return newKeymap(set, config.DefaultPickerKeys, config.PickerKeyScopes)
}

// action: the action of a key in a scope ("" = none).
func (k keymap) action(scope int, key string) string {
	if scope >= len(k.scopes) {
		return ""
	}
	return k.scopes[scope][key]
}

// label: the action's first key, as shown in hints.
func (k keymap) label(action string) string {
	ks := k.keys[action]
	if len(ks) == 0 {
		return "?"
	}
	return keyLabel(ks[0])
}

// labels: all of the action's keys.
func (k keymap) labels(action string) string {
	var out []string
	for _, x := range k.keys[action] {
		out = append(out, keyLabel(x))
	}
	return strings.Join(out, " ")
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
