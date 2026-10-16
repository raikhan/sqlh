package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/raikhan/sqlh/internal/control"
)

// raw ANSI (the picker draws on /dev/tty, lipgloss's renderer watches stdout)
const (
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiReset = "\x1b[0m"
)

var ansiEnv = map[string]string{"dev": "\x1b[30;42m", "test": "\x1b[30;43m", "prod": "\x1b[1;97;41m"}

// ConnectionPickItems: the connections as the Connections tab's table (for
// the inline picker of `sqlh connect`): a header and a row per connection,
// ungrouped first, then by group; typing matches the alias, group and target.
func ConnectionPickItems(conns []control.Connection) (header string, items []PickItem) {
	conns = append([]control.Connection(nil), conns...)
	sort.SliceStable(conns, func(i, j int) bool {
		gi, gj := strings.ToLower(conns[i].Group), strings.ToLower(conns[j].Group)
		return gi < gj // "" (ungrouped) first; the config's order within
	})
	aliasW, dbW, schemaW := len("connection"), len("database"), 0
	label := func(c control.Connection) string {
		if c.Group != "" {
			return c.Group + "/" + c.Alias
		}
		return c.Alias
	}
	schemaOf := func(c control.Connection) string {
		if strings.EqualFold(c.Schema, c.Database) {
			return ""
		}
		return c.Schema
	}
	for _, c := range conns {
		aliasW = max(aliasW, lipgloss.Width(label(c)))
		dbW = max(dbW, lipgloss.Width(c.Database))
		if s := schemaOf(c); s != "" {
			schemaW = max(schemaW, lipgloss.Width(s), len("schema"))
		}
	}
	// the picker puts 3 cells before each row; then the icon, as the
	// Connections tab: "⛁ alias ● database  schema  env"
	const icon = "⛁ "
	header = strings.Repeat(" ", 3+lipgloss.Width(icon)) + fit("connection", aliasW) + "   " + fit("database", dbW) + "  "
	if schemaW > 0 {
		header += fit("schema", schemaW) + "  "
	}
	header = ansiDim + header + " env" + ansiReset
	for _, c := range conns {
		name := c.Alias
		if c.Current {
			name = ansiBold + c.Alias + ansiReset
		}
		if c.Group != "" {
			name = ansiDim + c.Group + "/" + ansiReset + name
		}
		mark := " "
		if c.Current {
			mark = "●"
		}
		row := "\x1b[36m" + icon + ansiReset + name + strings.Repeat(" ", aliasW-lipgloss.Width(label(c))) + " " + mark + " " +
			ansiDim + fit(c.Database, dbW) + "  "
		if schemaW > 0 {
			row += fit(schemaOf(c), schemaW) + "  "
		}
		row += ansiReset
		tail := ""
		if c.Environment != "" {
			tail = ansiEnv[c.Environment] + " " + fit(c.Environment, 4) + " " + ansiReset
		}
		items = append(items, PickItem{Value: c.Alias, Display: row, Tail: tail,
			Match: strings.Join([]string{c.Alias, c.Group, c.Database, schemaOf(c), c.Environment}, " ")})
	}
	return header, items
}
