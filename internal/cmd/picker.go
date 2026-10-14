package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type pickOpts struct {
	prompt  string
	header  string
	multi   bool
	preview string   // fzf --preview command; {1} is the value field
	expect  []string // keys that also accept; pickKey reports which one
	binds   []string // extra --bind specs
	args    []string // other fzf arguments
	// display: the field shown and matched (default 2.., after the value);
	// it may contain ANSI colours.
	display string
}

// pick runs fzf over lines of "value\tdisplay" and returns the chosen values
// (nil when cancelled). Like ~/.config/helix/fzf-in-helix.nu it uses height
// mode and switches the kitty keyboard protocol off around fzf, so it works
// from Helix's :insert-output as well as from a terminal. SQLH_FZF_ARGS adds
// fzf arguments (e.g. --filter=x for scripted use).
func pick(lines []string, o pickOpts) ([]string, error) {
	_, vals, err := pickKey(lines, o)
	return vals, err
}

// pickKey is pick, also returning the key that accepted ("" for Enter) when
// o.expect is set.
func pickKey(lines []string, o pickOpts) (string, []string, error) {
	fzf, err := exec.LookPath("fzf")
	if err != nil {
		return "", nil, errors.New("fzf not found: install it, or use the list output (without -p)")
	}
	if len(lines) == 0 {
		return "", nil, errors.New("nothing to pick from")
	}
	display := o.display
	if display == "" {
		display = "2.."
	}
	// Ranked by match quality once a query is typed; the input order (e.g.
	// related tables first) holds while it is empty.
	args := []string{"--height=80%", "--delimiter=\t", "--with-nth=" + display, "--ansi", "--reverse"}
	if o.prompt != "" {
		args = append(args, "--prompt="+o.prompt+"> ")
	}
	if o.header != "" {
		args = append(args, "--header="+o.header)
	}
	if o.multi {
		args = append(args, "--multi", "--bind=ctrl-a:select-all")
	}
	if o.preview != "" {
		// wrapped preview lines: at word boundaries, indented instead of fzf's ↳
		args = append(args, "--preview="+o.preview, "--preview-window=right,50%,wrap-word", "--preview-wrap-sign=    ")
	}
	if len(o.expect) > 0 {
		args = append(args, "--expect="+strings.Join(o.expect, ","))
	}
	for _, b := range o.binds {
		args = append(args, "--bind="+b)
	}
	args = append(args, o.args...)
	extra := strings.Fields(os.Getenv("SQLH_FZF_ARGS"))
	args = append(args, extra...)
	scripted := false
	for _, a := range extra {
		if strings.HasPrefix(a, "--filter") || a == "-f" {
			scripted = true
		}
	}

	cmd := exec.Command(fzf, args...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "SHELL=/bin/sh") // preview commands; the user's shell may be nu
	if !scripted {
		kitty("\x1b[>0u") // push "no kitty keyboard flags" so Ctrl/Esc reach fzf
		defer kitty("\x1b[<1u")
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && (ee.ExitCode() == 1 || ee.ExitCode() == 130) {
			return "", nil, nil // no match / cancelled: insert nothing
		}
		return "", nil, err
	}
	outLines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	key := ""
	if len(o.expect) > 0 && len(outLines) > 0 && !scripted { // --filter prints no key line
		key, outLines = outLines[0], outLines[1:]
	}
	var vals []string
	for _, l := range outLines {
		if l != "" {
			vals = append(vals, strings.SplitN(l, "\t", 2)[0])
		}
	}
	return key, vals, nil
}

func kitty(seq string) {
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		_, _ = tty.WriteString(seq)
		tty.Close()
	}
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

// self returns how to invoke this executable (for fzf previews).
func self() string {
	if exe, err := os.Executable(); err == nil {
		return shellQuote(exe)
	}
	return "sqlh"
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
