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
	i := commandIndex(app, args)
	if i < 0 {
		return args
	}
	cmd := app.Command(args[i])
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

// commandIndex is the index of the subcommand in args (after global flags),
// -1 if there is none.
func commandIndex(app *cli.App, args []string) int {
	i := 1
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if takesValue(app.Flags, args[i]) && !strings.Contains(args[i], "=") {
			i++
		}
		i++
	}
	if i >= len(args) || app.Command(args[i]) == nil {
		return -1
	}
	return i
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
