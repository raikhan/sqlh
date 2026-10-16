package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/config"
)

func TestKeySections(t *testing.T) {
	cfg, err := config.Parse([]byte("panel:\n  keys:\n    insert: i\n    history:\n      quit: Q\npicker:\n  keys:\n    nameOnly: ctrl+n\n    down: ctrl+j\n"))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printKeySections(&b, keySections(cfg))
	out := b.String()
	for _, want := range []string{
		"panel.keys — every tab",
		"panel.keys.objects — the Objects tab (2)",
		"panel.keys.history — the History tab (5)",
		"picker.keys — the inline picker",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	line := func(section, action string) string {
		s := out[strings.Index(out, section+" —"):]
		if i := strings.Index(s, "\n\n"); i > 0 {
			s = s[:i]
		}
		for _, l := range strings.Split(s, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == action {
				return l
			}
		}
		return ""
	}
	if l := line("panel.keys.objects", "insert"); !strings.Contains(l, " i ") || !strings.Contains(l, "[default: ctrl+y]") {
		t.Errorf("insert: %q", l)
	}
	if l := line("panel.keys.history", "quit"); !strings.Contains(l, " Q ") || !strings.Contains(l, "(set for this tab)") {
		t.Errorf("quit in history: %q", l)
	}
	if l := line("panel.keys", "quit"); !strings.Contains(l, " q ") {
		t.Errorf("quit: %q", l)
	}
	if l := line("picker.keys", "nameOnly"); !strings.Contains(l, "ctrl+n") || !strings.Contains(l, "[default: ctrl+t]") {
		t.Errorf("picker nameOnly: %q", l)
	}
}
