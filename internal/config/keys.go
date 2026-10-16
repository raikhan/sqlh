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

// PanelKeys is panel.keys: `action: keys` for every tab, and sections by
// scope (`connections: {action: keys}`, …, `search:`, `form:`) for one.
type PanelKeys struct {
	All    map[string]KeyList
	Scopes map[string]map[string]KeyList
}

// UnmarshalYAML takes a map whose values are keys (an action) or maps (a
// scope's section).
func (k *PanelKeys) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw map[string]interface{}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	k.All, k.Scopes = map[string]KeyList{}, map[string]map[string]KeyList{}
	for name, v := range raw {
		if m, ok := v.(map[interface{}]interface{}); ok {
			sec := map[string]KeyList{}
			for a, kv := range m {
				keys, err := keyListOf(kv)
				if err != nil {
					return fmt.Errorf("panel.keys.%s.%v: %w", name, a, err)
				}
				sec[fmt.Sprint(a)] = keys
			}
			k.Scopes[name] = sec
			continue
		}
		keys, err := keyListOf(v)
		if err != nil {
			return fmt.Errorf("panel.keys.%s: %w", name, err)
		}
		k.All[name] = keys
	}
	return nil
}

func keyListOf(v interface{}) (KeyList, error) {
	switch x := v.(type) {
	case string:
		return KeyList{x}, nil
	case []interface{}:
		var out KeyList
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%v is not a key", e)
			}
			out = append(out, s)
		}
		return out, nil
	case int:
		return KeyList{fmt.Sprint(x)}, nil // 1: (unquoted digits)
	}
	return nil, fmt.Errorf("%v is not a key or a list of keys", v)
}

// DefaultPanelKeys are the panel's (and the app's) keys, by action.
var DefaultPanelKeys = Keymap{
	"up": {"k", "up", "ctrl+p"}, "down": {"j", "down", "ctrl+n"},
	"top": {"g", "home"}, "bottom": {"G", "end"},
	"pageDown": {"ctrl+d", "pgdown"}, "pageUp": {"ctrl+u", "pgup"},
	"scrollTop": {"z"}, // the selected row to the top of the view
	"expand":    {"l", "right"}, "collapse": {"h", "left"},
	"collapseAll": {"H"}, "expandAll": {"L"},
	"open":   {"enter"}, // navigate: open/close, connect, open the file
	"mark":   {"space"}, // mark columns for one insert
	"insert": {"ctrl+y"}, "copy": {"y"}, "actions": {".", "a"},
	"search":      {"/"},
	"previewNext": {"tab"}, "previewPrev": {"shift+tab"},
	"previewDown": {"J"}, "previewUp": {"K"},
	"sample": {"s"}, "nextRow": {"]"}, "prevRow": {"["},
	"newQuery": {"c"}, "openResult": {"o"}, "editFile": {"e"},
	"nextChain": {"n"}, "prevChain": {"N"}, "clearChains": {"esc"},
	"renameQuery": {"R"}, "deleteQuery": {"X"}, "historyAllConns": {"c"},
	"newConnection": {"C"}, "editConnection": {"E"}, "duplicateConnection": {"D"}, "deleteConnection": {"X"},
	"refresh": {"r"}, "help": {"?"}, "quit": {"q"},
	// out of the way (ctrl+R is ctrl+shift+r, ctrl+: is ctrl+shift+; on a US
	// layout: the panel matches either name)
	"editConfig": {"ctrl+:"}, "reloadConfig": {"ctrl+R"},
	"backToEditor": {"backspace"},
	"tab1":         {"1"}, "tab2": {"2"}, "tab3": {"3"}, "tab4": {"4"}, "tab5": {"5"},
	// while searching (other keys type into the query)
	"searchUp": {"up", "ctrl+p", "ctrl+k"}, "searchDown": {"down", "ctrl+n", "ctrl+j"},
	"searchOpen": {"enter"}, "searchInsert": {"ctrl+y"}, "searchCancel": {"esc"},
	"searchNextKind": {"tab"}, "searchPrevKind": {"shift+tab"},
	// the connection form
	"formSave": {"ctrl+s"}, "formTest": {"ctrl+t"}, "formCancel": {"esc"},
	"formNext": {"tab", "down"}, "formPrev": {"shift+tab", "up"},
}

// PanelScopes are the panel's key scopes: its tabs, its search, its form.
var PanelScopes = []string{"connections", "objects", "statement", "queries", "history", "search", "form"}

// PanelCommonActions are in every tab.
var PanelCommonActions = []string{"up", "down", "top", "bottom", "pageDown", "pageUp", "scrollTop",
	"expand", "collapse", "collapseAll", "open", "copy", "actions", "search",
	"previewNext", "previewPrev", "previewDown", "previewUp",
	"help", "quit", "backToEditor", "tab1", "tab2", "tab3", "tab4", "tab5", "editConfig", "reloadConfig"}

// panelScopeOwn are each scope's own actions (tabs: besides the common ones).
var panelScopeOwn = map[string][]string{
	"connections": {"newConnection", "editConnection", "duplicateConnection", "deleteConnection"},
	"objects":     {"insert", "mark", "sample", "nextRow", "prevRow", "newQuery", "refresh"},
	"statement":   {"insert", "mark", "sample", "nextRow", "prevRow", "newQuery", "refresh", "expandAll", "nextChain", "prevChain", "clearChains"},
	"queries":     {"newQuery", "renameQuery", "deleteQuery"},
	"history":     {"insert", "openResult", "editFile", "historyAllConns"},
	"search":      {"searchUp", "searchDown", "searchOpen", "searchInsert", "searchCancel", "searchNextKind", "searchPrevKind"},
	"form":        {"formSave", "formTest", "formCancel", "formNext", "formPrev"},
}

// PanelScopeActions: the actions available in a scope.
func PanelScopeActions(scope string) []string {
	own := panelScopeOwn[scope]
	if scope == "search" || scope == "form" {
		return own
	}
	return append(append([]string(nil), PanelCommonActions...), own...)
}

// PanelOwnActions: a tab's actions beyond the common ones.
func PanelOwnActions(scope string) []string { return panelScopeOwn[scope] }

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

func normalizeKeys(ks []string) []string {
	var out []string
	for _, k := range ks {
		switch strings.ToLower(k) {
		case " ":
			k = "space"
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

// unknownActions: the configured actions def doesn't have.
func unknownActions(what string, set map[string]KeyList, known func(string) bool, list []string) error {
	var unknown []string
	for a := range set {
		if !known(a) {
			unknown = append(unknown, a)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	list = append([]string(nil), list...)
	sort.Strings(list)
	return fmt.Errorf("%s: unknown action %s (actions: %s)", what, strings.Join(unknown, ", "), strings.Join(list, ", "))
}

// clashes: a key on two actions of one keymap.
func clashes(what string, km Keymap, actions []string) error {
	owner := map[string]string{}
	for _, a := range actions {
		for _, k := range km[a] {
			if prev, ok := owner[k]; ok && prev != a {
				return fmt.Errorf("%s: key %q is on both %s and %s", what, k, prev, a)
			}
			owner[k] = a
		}
	}
	return nil
}

func defaultActions(def Keymap) []string {
	var out []string
	for a := range def {
		out = append(out, a)
	}
	return out
}

// checkPanelKeys: unknown actions or scopes, and one key on two actions of
// a scope.
func checkPanelKeys(k PanelKeys) error {
	if err := unknownActions("panel.keys", k.All, func(a string) bool { _, ok := DefaultPanelKeys[a]; return ok },
		defaultActions(DefaultPanelKeys)); err != nil {
		return err
	}
	for scope, sec := range k.Scopes {
		acts := PanelScopeActions(scope)
		if !contains(PanelScopes, scope) {
			return fmt.Errorf("panel.keys.%s: unknown section (sections: %s; or an action)", scope, strings.Join(PanelScopes, ", "))
		}
		if err := unknownActions("panel.keys."+scope, sec, func(a string) bool { return contains(acts, a) }, acts); err != nil {
			return err
		}
	}
	maps := k.Keymaps()
	for _, scope := range PanelScopes {
		if err := clashes("panel.keys ("+scope+")", maps[scope], PanelScopeActions(scope)); err != nil {
			return err
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Keymaps: each scope's keymap (its actions only): the scope's section,
// else the keys for every tab, else the defaults.
func (k PanelKeys) Keymaps() map[string]Keymap {
	out := map[string]Keymap{}
	for _, scope := range PanelScopes {
		km := Keymap{}
		for _, a := range PanelScopeActions(scope) {
			keys := DefaultPanelKeys[a]
			if v, ok := k.All[a]; ok {
				keys = normalizeKeys(v)
			}
			if v, ok := k.Scopes[scope][a]; ok {
				keys = normalizeKeys(v)
			}
			km[a] = keys
		}
		out[scope] = km
	}
	return out
}

// checkPickerKeys: unknown actions and clashes of picker.keys.
func checkPickerKeys(set map[string]KeyList) error {
	if err := unknownActions("picker.keys", set, func(a string) bool { _, ok := DefaultPickerKeys[a]; return ok },
		defaultActions(DefaultPickerKeys)); err != nil {
		return err
	}
	return clashes("picker.keys", PickerConfig{Keys: set}.KeyMap(), PickerKeyScopes[0])
}

// KeyMaps is the panel's keymap per scope: the defaults and panel.keys.
func (p PanelConfig) KeyMaps() map[string]Keymap { return p.Keys.Keymaps() }

// KeyMap is the picker's keymap: the defaults and picker.keys.
func (p PickerConfig) KeyMap() Keymap {
	out := Keymap{}
	for a, k := range DefaultPickerKeys {
		out[a] = k
	}
	for a, k := range p.Keys {
		out[a] = normalizeKeys(k)
	}
	return out
}
