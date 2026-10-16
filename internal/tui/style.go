package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// icons per panel.icons setting.
type icons struct {
	table, view, matview, sequence, typ, index, constraint, trigger string
	column, schema, connection, connections, statement, history     string
	group, file                                                     string
	open, closed, leaf                                              string
}

func iconSet(name string) icons {
	switch name {
	case "ascii":
		return icons{table: "T", view: "V", matview: "M", sequence: "S", typ: "Y", index: "I", constraint: "C", trigger: "G",
			column: "-", schema: "#", connection: "@", connections: "@", statement: "*", history: "~", group: "+", file: "=", open: "v", closed: ">", leaf: " "}
	case "nerd":
		return icons{table: "\U000f04eb", view: "\U000f0208", matview: "\U000f0209", sequence: "\U000f0b84", typ: "\U000f0b09",
			index: "\U000f0306", constraint: "\U000f033e", trigger: "\U000f0241", column: "\U000f0b1e", schema: "\U000f01bc",
			connection: "\U000f01bc", connections: "\U000f0b2e", statement: "\U000f0fc9", history: "\U000f02da", group: "\U000f024b", file: "\U000f0219", open: "\uf47c", closed: "\uf460", leaf: " "}
	}
	return icons{table: "▦", view: "◫", matview: "◩", sequence: "#", typ: "τ", index: "⌗", constraint: "⊂", trigger: "ϟ",
		column: "·", schema: "◈", connection: "⛁", connections: "⛁", statement: "✎", history: "↺", group: "▣", file: "✎", open: "▾", closed: "▸", leaf: " "}
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
	colAccent, colDim, colMatch, colSelBg, colErr, colAddBg, colDelBg color.Color

	stDim, stAccent, stBold, stMatch, stSel, stErr, stTitle, stTabOn, stMainTabOn, stMainTabOff,
	stTabOff, stMarked, stBorder, stEnvDev, stEnvTest, stEnvProd, stKey, stMenuSel, stMenuBox,
	stRelTable, stAdd, stDel, stPopup, stLabel, stFocus lipgloss.Style

	// darkStyles: the styles are for a dark background
	darkStyles = true
)

func init() { setStyles(true) }

// setStyles builds the styles for a dark or light background (Bubble Tea
// reports the terminal's: tea.BackgroundColorMsg).
func setStyles(dark bool) {
	darkStyles = dark
	ld := lipgloss.LightDark(dark)
	c := func(light, dark string) color.Color { return ld(lipgloss.Color(light), lipgloss.Color(dark)) }
	colAccent = c("#005f87", "#5fafd7")
	colDim = c("#8a8a8a", "#6c6c6c")
	colMatch = c("#af5f00", "#ffaf5f")
	colSelBg = c("#d0d0d0", "#303a46")
	colErr = c("#af0000", "#ff5f5f")
	colAddBg = c("#d7f5dd", "#09301f") // diffs (modus bg-added-faint-ish)
	colDelBg = c("#fbe1e1", "#380a19")

	stDim = lipgloss.NewStyle().Foreground(colDim)
	stAccent = lipgloss.NewStyle().Foreground(colAccent)
	stBold = lipgloss.NewStyle().Bold(true)
	stMatch = lipgloss.NewStyle().Foreground(colMatch).Bold(true)
	stSel = lipgloss.NewStyle().Background(colSelBg)
	stErr = lipgloss.NewStyle().Foreground(colErr)
	stTitle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	stTabOn = lipgloss.NewStyle().Bold(true).Underline(true)
	stMainTabOn = lipgloss.NewStyle().Bold(true).Foreground(c("#ffffff", "#000000")).Background(colAccent)
	stMainTabOff = lipgloss.NewStyle().Foreground(colDim)
	stTabOff = lipgloss.NewStyle().Foreground(colDim)
	stMarked = lipgloss.NewStyle().Foreground(colMatch)
	stBorder = lipgloss.NewStyle().Foreground(colDim)
	stEnvDev = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("2")).Padding(0, 1)
	stEnvTest = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")).Padding(0, 1)
	stEnvProd = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("1")).Bold(true).Padding(0, 1)
	stKey = lipgloss.NewStyle().Foreground(colAccent)
	stMenuSel = lipgloss.NewStyle().Background(colSelBg).Bold(true)
	stMenuBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 1)
	stRelTable = lipgloss.NewStyle().Foreground(colAccent)
	stAdd = lipgloss.NewStyle().Foreground(c("#005f00", "#87d787"))
	stDel = lipgloss.NewStyle().Foreground(colErr)
	stPopup = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colMatch).Padding(0, 1)
	stLabel = lipgloss.NewStyle().Foreground(colDim)
	stFocus = lipgloss.NewStyle().Foreground(colMatch).Bold(true)
}

func envStyle(env string) lipgloss.Style {
	switch env {
	case "test":
		return stEnvTest
	case "prod":
		return stEnvProd
	}
	return stEnvDev
}
