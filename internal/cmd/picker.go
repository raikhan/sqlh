package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/raikhan/sqlh/internal/tui"
)

// pick lets the user choose among items with sqlh's inline picker (drawn on
// /dev/tty, so it works from Helix's :insert-output as well as from a
// terminal) and returns the chosen values; nil when cancelled.
// SQLH_PICK=<query> picks without a UI (scripts, tests).
func pick(items []tui.PickItem, o tui.PickOptions) ([]string, error) {
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
