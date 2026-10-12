package cmd

import (
	"strings"

	"github.com/urfave/cli/v2"
)

// ReorderArgs moves a subcommand's flags before its positional arguments.
// urfave/cli v2 stops parsing flags at the first positional, so
// "sqlh columns t -p --prefix x." would silently ignore -p and --prefix.
// Global flags before the subcommand are left alone; "--" ends flag parsing.
func ReorderArgs(app *cli.App, args []string) []string {
	if len(args) < 2 {
		return args
	}
	// find the subcommand
	i := 1
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if takesValue(app.Flags, args[i]) && !strings.Contains(args[i], "=") {
			i++
		}
		i++
	}
	if i >= len(args) {
		return args
	}
	cmd := app.Command(args[i])
	if cmd == nil {
		return args
	}
	var flags, pos []string
	rest := args[i+1:]
	for j := 0; j < len(rest); j++ {
		a := rest[j]
		switch {
		case a == "--":
			pos = append(pos, rest[j:]...)
			j = len(rest)
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			if takesValue(cmd.Flags, a) && !strings.Contains(a, "=") && j+1 < len(rest) {
				j++
				flags = append(flags, rest[j])
			}
		default:
			pos = append(pos, a)
		}
	}
	out := append([]string{}, args[:i+1]...)
	out = append(out, flags...)
	return append(out, pos...)
}

func takesValue(flags []cli.Flag, arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if k := strings.IndexByte(name, '='); k >= 0 {
		name = name[:k]
	}
	for _, f := range flags {
		for _, n := range f.Names() {
			if n == name {
				_, isBool := f.(*cli.BoolFlag)
				return !isBool
			}
		}
	}
	return false
}
