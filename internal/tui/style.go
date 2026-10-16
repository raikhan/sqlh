package tui

import "github.com/charmbracelet/lipgloss"

// icons per panel.icons setting.
type icons struct {
	table, view, matview, sequence, typ, index, constraint, trigger string
	column, schema, connection, connections, statement, history     string
	open, closed, leaf                                              string
}

func iconSet(name string) icons {
	switch name {
	case "ascii":
		return icons{table: "T", view: "V", matview: "M", sequence: "S", typ: "Y", index: "I", constraint: "C", trigger: "G",
			column: "-", schema: "#", connection: "@", connections: "@", statement: "*", history: "~", open: "v", closed: ">", leaf: " "}
	case "nerd":
		return icons{table: "\U000f04eb", view: "\U000f0208", matview: "\U000f0209", sequence: "\U000f0b84", typ: "\U000f0b09",
			index: "\U000f0306", constraint: "\U000f033e", trigger: "\U000f0241", column: "\U000f0b1e", schema: "\U000f01bc",
			connection: "\U000f01bc", connections: "\U000f0b2e", statement: "\U000f0fc9", history: "\U000f02da", open: "\uf47c", closed: "\uf460", leaf: " "}
	}
	return icons{table: "▦", view: "◫", matview: "◩", sequence: "#", typ: "τ", index: "⌗", constraint: "⊂", trigger: "ϟ",
		column: "·", schema: "◈", connection: "⛁", connections: "⛁", statement: "✎", history: "↺", open: "▾", closed: "▸", leaf: " "}
}

func (ic icons) kind(k string) string {
	switch k {
	case "table":
		return ic.table
	case "view":
		return ic.view
	case "materialized view":
		return ic.matview
	case "sequence":
		return ic.sequence
	case "type":
		return ic.typ
	case "index":
		return ic.index
	case "constraint":
		return ic.constraint
	case "trigger":
		return ic.trigger
	}
	return ic.table
}

var (
	colAccent = lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafd7"}
	colDim    = lipgloss.AdaptiveColor{Light: "#8a8a8a", Dark: "#6c6c6c"}
	colMatch  = lipgloss.AdaptiveColor{Light: "#af5f00", Dark: "#ffaf5f"}
	colSelBg  = lipgloss.AdaptiveColor{Light: "#d0d0d0", Dark: "#303a46"}
	colErr    = lipgloss.AdaptiveColor{Light: "#af0000", Dark: "#ff5f5f"}

	stDim      = lipgloss.NewStyle().Foreground(colDim)
	stAccent   = lipgloss.NewStyle().Foreground(colAccent)
	stBold     = lipgloss.NewStyle().Bold(true)
	stMatch    = lipgloss.NewStyle().Foreground(colMatch).Bold(true)
	stSel      = lipgloss.NewStyle().Background(colSelBg)
	stErr      = lipgloss.NewStyle().Foreground(colErr)
	stTitle    = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	stTabOn    = lipgloss.NewStyle().Bold(true).Underline(true)
	stTabOff   = lipgloss.NewStyle().Foreground(colDim)
	stMarked   = lipgloss.NewStyle().Foreground(colMatch)
	stBorder   = lipgloss.NewStyle().Foreground(colDim)
	stEnvDev   = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("2")).Padding(0, 1)
	stEnvTest  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")).Padding(0, 1)
	stEnvProd  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("1")).Bold(true).Padding(0, 1)
	stKey      = lipgloss.NewStyle().Foreground(colAccent)
	stMenuSel  = lipgloss.NewStyle().Background(colSelBg).Bold(true)
	stMenuBox  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 1)
	stRelTable = lipgloss.NewStyle().Foreground(colAccent)
	stAdd      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#005f00", Dark: "#87d787"})
	stDel      = lipgloss.NewStyle().Foreground(colErr)
)

func envStyle(env string) lipgloss.Style {
	switch env {
	case "test":
		return stEnvTest
	case "prod":
		return stEnvProd
	}
	return stEnvDev
}
