package tui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

var testNamedKeys = map[string]rune{
	"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "space": tea.KeySpace,
	"backspace": tea.KeyBackspace, "up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft,
	"right": tea.KeyRight, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
}

// press builds the key press a name describes, as a kitty-protocol
// terminal sends it: "ctrl+y", "shift+tab", "G" (text), "ctrl+R" (ctrl+shift+r),
// "ctrl+:" (ctrl+shift+;).
func press(name string) tea.KeyPressMsg {
	var k tea.KeyPressMsg
	parts := strings.Split(name, "+")
	last := parts[len(parts)-1]
	if strings.HasSuffix(name, "++") || name == "+" {
		last = "+"
	}
	for _, m := range parts[:len(parts)-1] {
		switch m {
		case "ctrl":
			k.Mod |= tea.ModCtrl
		case "alt":
			k.Mod |= tea.ModAlt
		case "shift":
			k.Mod |= tea.ModShift
		}
	}
	if c, ok := testNamedKeys[last]; ok {
		k.Code = c
		if c == tea.KeySpace && k.Mod == 0 {
			k.Text = " "
		}
		return k
	}
	r := []rune(last)[0]
	if k.Mod == 0 {
		k.Code, k.Text = r, last
		if unicode.IsUpper(r) {
			k.Code, k.Mod = unicode.ToLower(r), tea.ModShift
		}
		return k
	}
	switch {
	case unicode.IsUpper(r):
		k.Code, k.Mod = unicode.ToLower(r), k.Mod|tea.ModShift
	case r == ':':
		k.Code, k.Mod = ';', k.Mod|tea.ModShift
	default:
		k.Code = r
	}
	return k
}
