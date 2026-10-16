package config

import (
	"fmt"
	"sort"
	"strings"
)

// KeyList is one key or a list of keys (Bubble Tea key names: enter, esc,
// backspace, tab, shift+tab, ctrl+y, up, pgdown, a, G, /, …).
type KeyList []string

// UnmarshalYAML takes `key` or `[key, key]`.
func (k *KeyList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var one string
	if err := unmarshal(&one); err == nil {
		*k = KeyList{one}
		return nil
	}
	var many []string
	if err := unmarshal(&many); err != nil {
		return err
	}
	*k = many
	return nil
}

// Keymap: action → keys.
type Keymap map[string][]string

// DefaultPanelKeys are the panel's (and sqlh tui's) keys, by action.
var DefaultPanelKeys = Keymap{
	"up": {"k", "up", "ctrl+p"}, "down": {"j", "down", "ctrl+n"},
	"top": {"g", "home"}, "bottom": {"G", "end"},
	"pageDown": {"ctrl+d", "pgdown"}, "pageUp": {"ctrl+u", "pgup"},
	"expand": {"l", "right"}, "collapse": {"h", "left"},
	"open":   {"enter"}, // navigate: open/close, connect, open the file
	"mark":   {" "},     // mark columns for one insert
	"insert": {"ctrl+y"}, "copy": {"y"}, "actions": {".", "a"},
	"search":      {"/"},
	"previewNext": {"tab"}, "previewPrev": {"shift+tab"},
	"previewDown": {"J"}, "previewUp": {"K"},
	"sample": {"s"}, "nextRow": {"]"}, "prevRow": {"["},
	"newQuery": {"n"}, "openResult": {"o"}, "editFile": {"e"},
	"newConnection": {"C"}, "editConnection": {"E"}, "duplicateConnection": {"D"}, "deleteConnection": {"X"},
	"refresh": {"r"}, "help": {"?"}, "quit": {"q"},
	"backToEditor": {"backspace"},
	"tab1":         {"1"}, "tab2": {"2"}, "tab3": {"3"}, "tab4": {"4"}, "tab5": {"5"},
	"tab6": {"6"}, "tab7": {"7"}, "tab8": {"8"}, "tab9": {"9"},
	// while searching (other keys type into the query)
	"searchUp": {"up", "ctrl+p", "ctrl+k"}, "searchDown": {"down", "ctrl+n", "ctrl+j"},
	"searchOpen": {"enter"}, "searchInsert": {"ctrl+y"}, "searchCancel": {"esc"},
	// the connection form
	"formSave": {"ctrl+s"}, "formTest": {"ctrl+t"}, "formCancel": {"esc"},
	"formNext": {"tab", "down"}, "formPrev": {"shift+tab", "up"},
}

// PanelKeyScopes: actions whose keys must differ from each other (main,
// search, form).
var PanelKeyScopes = [][]string{
	{"up", "down", "top", "bottom", "pageDown", "pageUp", "expand", "collapse", "open", "mark", "insert", "copy",
		"actions", "search", "previewNext", "previewPrev", "previewDown", "previewUp", "sample", "nextRow", "prevRow",
		"newQuery", "openResult", "editFile", "newConnection", "editConnection", "duplicateConnection", "deleteConnection",
		"refresh", "help", "quit", "backToEditor", "tab1", "tab2", "tab3", "tab4", "tab5", "tab6", "tab7", "tab8", "tab9"},
	{"searchUp", "searchDown", "searchOpen", "searchInsert", "searchCancel"},
	{"formSave", "formTest", "formCancel", "formNext", "formPrev"},
}

// DefaultPickerKeys are the inline picker's keys.
var DefaultPickerKeys = Keymap{
	"accept": {"enter"}, "cancel": {"esc", "ctrl+c", "ctrl+g", "ctrl+q"},
	"up": {"up", "ctrl+p", "ctrl+k"}, "down": {"down", "ctrl+n", "ctrl+j"},
	"pageUp": {"pgup"}, "pageDown": {"pgdown"},
	"previewDown": {"shift+down"}, "previewUp": {"shift+up"},
	"toggleDown": {"tab"}, "toggleUp": {"shift+tab"}, "toggleAll": {"ctrl+a"},
	"clearQuery": {"ctrl+u"}, "deleteWord": {"ctrl+w"},
	// the table picker
	"nameOnly": {"ctrl+t"}, "relations": {"ctrl+r"}, "back": {"ctrl+o"},
}

// PickerKeyScopes: the picker's actions (one scope).
var PickerKeyScopes = [][]string{{"accept", "cancel", "up", "down", "pageUp", "pageDown", "previewDown", "previewUp",
	"toggleDown", "toggleUp", "toggleAll", "clearQuery", "deleteWord", "nameOnly", "relations", "back"}}

// merge: the defaults with the configured actions replaced.
func merge(def Keymap, set map[string]KeyList) Keymap {
	out := Keymap{}
	for a, k := range def {
		out[a] = k
	}
	for a, k := range set {
		out[a] = normalizeKeys(k)
	}
	return out
}

func normalizeKeys(ks []string) []string {
	var out []string
	for _, k := range ks {
		switch strings.ToLower(k) {
		case "space":
			k = " "
		case "escape":
			k = "esc"
		case "return":
			k = "enter"
		}
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

// checkKeys: unknown actions, and one key on two actions of a scope.
func checkKeys(what string, def Keymap, set map[string]KeyList, scopes [][]string) error {
	var unknown []string
	for a := range set {
		if _, ok := def[a]; !ok {
			unknown = append(unknown, a)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		var known []string
		for a := range def {
			known = append(known, a)
		}
		sort.Strings(known)
		return fmt.Errorf("%s: unknown action %s (actions: %s)", what, strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	km := merge(def, set)
	for _, scope := range scopes {
		owner := map[string]string{}
		for _, a := range scope {
			for _, k := range km[a] {
				if prev, ok := owner[k]; ok && prev != a {
					return fmt.Errorf("%s: key %q is on both %s and %s", what, k, prev, a)
				}
				owner[k] = a
			}
		}
	}
	return nil
}

// KeyMap is the panel's keymap: the defaults and panel.keys.
func (p PanelConfig) KeyMap() Keymap {
	set := map[string]KeyList{}
	for a, k := range p.Keys {
		set[a] = k
	}
	return merge(DefaultPanelKeys, set)
}

// KeyMap is the picker's keymap: the defaults and picker.keys.
func (p PickerConfig) KeyMap() Keymap { return merge(DefaultPickerKeys, p.Keys) }
