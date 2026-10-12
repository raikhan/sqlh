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
	preview string // fzf --preview command; {1} is the value field
}

// pick runs fzf over lines of "value\tdisplay" and returns the chosen values
// (nil when cancelled). Like ~/.config/helix/fzf-in-helix.nu it uses height
// mode and switches the kitty keyboard protocol off around fzf, so it works
// from Helix's :insert-output as well as from a terminal. SQLH_FZF_ARGS adds
// fzf arguments (e.g. --filter=x for scripted use).
func pick(lines []string, o pickOpts) ([]string, error) {
	fzf, err := exec.LookPath("fzf")
	if err != nil {
		return nil, errors.New("fzf not found: install it, or use the list output (without -p)")
	}
	if len(lines) == 0 {
		return nil, errors.New("nothing to pick from")
	}
	args := []string{"--height=80%", "--delimiter=\t", "--with-nth=2..", "--no-sort", "--reverse"}
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
		args = append(args, "--preview="+o.preview, "--preview-window=right,50%,wrap")
	}
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
			return nil, nil // no match / cancelled: insert nothing
		}
		return nil, err
	}
	var vals []string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l != "" {
			vals = append(vals, strings.SplitN(l, "\t", 2)[0])
		}
	}
	return vals, nil
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
