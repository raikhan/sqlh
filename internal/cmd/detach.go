package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/urfave/cli/v2"
)

// --bg: the command starts itself again in a detached process (own session,
// stdio on /dev/null) and returns at once, so an editor's :pipe-to doesn't
// wait for the query. The detached run reports through the server (the
// editor's status line) and opens the viewer when asked to.

// detach starts this invocation again without the flags in drop (and their
// values), with extra appended.
func detach(c *cli.Context, drop []string, extra ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := append(childArgs(c.App, os.Args, append(drop, "bg")), extra...)
	cmd := exec.Command(exe, args[1:]...) // stdio nil: /dev/null
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "started in the background (pid %d)\n", cmd.Process.Pid)
	return cmd.Process.Release()
}

// childArgs is args without the subcommand flags named in drop (any of their
// names), values included.
func childArgs(app *cli.App, args []string, drop []string) []string {
	i := commandIndex(app, args)
	if i < 0 {
		return args
	}
	cmd := app.Command(args[i])
	dropped := map[string]bool{}
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			for _, d := range drop {
				if n == d {
					for _, m := range f.Names() {
						dropped[m] = true
					}
				}
			}
		}
	}
	out := append([]string{}, args[:i+1]...)
	rest := args[i+1:]
	for j := 0; j < len(rest); j++ {
		a := rest[j]
		if a == "--" {
			out = append(out, rest[j:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			name := strings.TrimLeft(a, "-")
			if k := strings.IndexByte(name, '='); k >= 0 {
				name = name[:k]
			}
			if dropped[name] {
				if takesValue(cmd.Flags, a) && !strings.Contains(a, "=") {
					j++
				}
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// --- row limit -------------------------------------------------------------

// limitChoice is a parsed --limit: a row count, all rows, or ask.
type limitChoice struct {
	n   int
	all bool
	ask bool
}

func parseLimit(s string) (limitChoice, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "_", "")
	switch strings.ToLower(s) {
	case "":
		return limitChoice{}, nil
	case "all":
		return limitChoice{all: true}, nil
	case "ask":
		return limitChoice{ask: true}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return limitChoice{}, fmt.Errorf("--limit: %q is not a positive number, all or ask", s)
	}
	return limitChoice{n: n}, nil
}

func (l limitChoice) String() string {
	if l.all {
		return "all"
	}
	return strconv.Itoa(l.n)
}

// typedNumber: Enter on a typed number takes the number itself ("10" would
// otherwise pick "100").
const typedNumber = `case {q} in "" | *[!0-9_]*) echo accept ;; *) echo print-query ;; esac`

// askLimit picks a row limit with fzf: a preset, a typed number or all.
// ok is false when cancelled.
func askLimit() (limitChoice, bool, error) {
	presets := []string{"100", "1000", "10000", "100000", "all"}
	lines := make([]string, len(presets))
	for i, p := range presets {
		lines[i] = p + "\t" + p
	}
	vals, err := pick(lines, pickOpts{
		prompt: "rows", header: "Rows to fetch (type a number, or pick)",
		binds: []string{"enter:transform:" + typedNumber}, args: []string{"--no-sort"},
	})
	if err != nil || len(vals) == 0 {
		return limitChoice{}, false, err
	}
	l, err := parseLimit(vals[0])
	if err != nil || l.ask || (!l.all && l.n == 0) {
		return limitChoice{}, false, nil // not a number: cancelled
	}
	return l, true, nil
}
