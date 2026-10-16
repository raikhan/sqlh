package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/raikhan/sqlh/internal/connedit"
)

// formField is a row of the connection form.
type formField struct {
	key, label, hint string
	choices          []string // cycled with ←/→ instead of typed
	input            textinput.Model
}

// connForm edits a connection of the config file.
type connForm struct {
	title  string
	old    string // the alias being edited ("" = a new connection)
	after  string // new: insert after this connection
	fields []*formField
	focus  int
	status string
	err    bool
	busy   bool
}

var drivers = []string{"postgresql", "oracle", "sqlite3", "mysql", "mssql", "clickhouse", "vertica", "h2"}

var formSpec = []struct {
	key, label, hint string
	choices          []string
}{
	{"alias", "Alias", "the connection's name (conn= in headers)", nil},
	{"group", "Group", "a folder in the connection list (empty: top level)", nil},
	{"environment", "Environment", "←/→: dev, test or prod", []string{"dev", "test", "prod"}},
	{"sample", "Sample rows", "←/→: default (auto on dev/test, on request on prod), auto, manual, off", []string{"", "auto", "manual", "off"}},
	{"driver", "Driver", "←/→", drivers},
	{"dataSourceName", "Conn. string", "a connection string instead of the fields below (SQLite: the file)", nil},
	{"host", "Host", "", nil},
	{"port", "Port", "", nil},
	{"dbName", "Database", "the database / Oracle service", nil},
	{"user", "User", "", nil},
	{"passwd", "Password", "stored in the config file as is; or use the password command", nil},
	{"passwdCmd", "Password cmd", "a command printing the password, e.g. security find-generic-password -s NAME -w", nil},
	{"schema", "Schema", "the schema it works in (default: the first of Schemas, else the login's)", nil},
	{"schemas", "Schemas", "metadata allow-list, comma-separated (Oracle: empty = the login schema)", nil},
	{"proto", "Protocol", "tcp, udp, unix, http (MySQL and others)", nil},
	{"path", "Socket path", "unix socket path", nil},
	{"params", "Params", "key=value, comma-separated (e.g. sslmode=disable)", nil},
}

func newConnForm(title, old, after string, f connedit.Fields) *connForm {
	cf := &connForm{title: title, old: old, after: after}
	for _, sp := range formSpec {
		in := textinput.New()
		in.Prompt = ""
		in.CharLimit = 1024
		staticCursor(&in) // no blink ticks
		in.SetValue(f[sp.key])
		if sp.key == "passwd" {
			in.EchoMode = textinput.EchoPassword
			in.EchoCharacter = '•'
		}
		ff := &formField{key: sp.key, label: sp.label, hint: sp.hint, choices: sp.choices, input: in}
		if ff.choices != nil && f[sp.key] != "" && !contains(ff.choices, f[sp.key]) {
			ff.choices = append(append([]string(nil), ff.choices...), f[sp.key])
		}
		if sp.key == "environment" && f[sp.key] == "" {
			ff.input.SetValue("dev")
		}
		if sp.key == "driver" && f[sp.key] == "" {
			ff.input.SetValue("postgresql")
		}
		cf.fields = append(cf.fields, ff)
	}
	cf.setFocus(0)
	return cf
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// visibleFields: SQLite needs only the file.
func (cf *connForm) visibleFields() []*formField {
	driver := cf.value("driver")
	var out []*formField
	for _, f := range cf.fields {
		if driver == "sqlite3" {
			switch f.key {
			case "host", "port", "dbName", "user", "passwd", "passwdCmd", "schema", "schemas", "proto", "path", "params":
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

func (cf *connForm) value(key string) string {
	for _, f := range cf.fields {
		if f.key == key {
			return strings.TrimSpace(f.input.Value())
		}
	}
	return ""
}

// values: the form's fields (hidden ones empty: they are removed).
func (cf *connForm) values() connedit.Fields {
	out := connedit.Fields{}
	shown := map[string]bool{}
	for _, f := range cf.visibleFields() {
		shown[f.key] = true
	}
	for _, f := range cf.fields {
		if shown[f.key] {
			out[f.key] = strings.TrimSpace(f.input.Value())
		} else {
			out[f.key] = ""
		}
	}
	return out
}

func (cf *connForm) setFocus(i int) {
	vis := cf.visibleFields()
	cf.focus = clamp(i, 0, len(vis)-1)
	for j, f := range vis {
		if j == cf.focus && f.choices == nil {
			f.input.Focus()
		} else {
			f.input.Blur()
		}
	}
}

func (cf *connForm) focused() *formField {
	vis := cf.visibleFields()
	if cf.focus < 0 || cf.focus >= len(vis) {
		return nil
	}
	return vis[cf.focus]
}

type formSavedMsg struct {
	alias string
	err   error
}
type formTestedMsg struct{ err error }

// formKey handles a key in the form.
func (p *Panel) formKey(k tea.KeyPressMsg) tea.Cmd {
	cf := p.form
	key := k.String()
	switch p.km.actionOf(scopeForm, k) {
	case "formCancel":
		p.form = nil
		return nil
	case "formNext":
		cf.setFocus(cf.focus + 1)
		return nil
	case "formPrev":
		cf.setFocus(cf.focus - 1)
		return nil
	case "formSave":
		if cf.busy {
			return nil
		}
		vals := cf.values()
		cf.busy, cf.status, cf.err = true, "saving…", false
		be, old, after := p.be, cf.old, cf.after
		return func() tea.Msg { return formSavedMsg{vals["alias"], be.SaveConnection(old, vals, after)} }
	case "formTest":
		if cf.busy {
			return nil
		}
		vals := cf.values()
		cf.busy, cf.status, cf.err = true, "connecting…", false
		be := p.be
		return func() tea.Msg { return formTestedMsg{be.TestConnection(vals)} }
	}
	f := cf.focused()
	if f == nil {
		return nil
	}
	if f.choices != nil {
		i := choiceIndex(f.choices, f.input.Value())
		switch key {
		case "right", "l", "space":
			f.input.SetValue(f.choices[(i+1)%len(f.choices)])
		case "left", "h":
			f.input.SetValue(f.choices[(i-1+len(f.choices))%len(f.choices)])
		case "enter":
			cf.setFocus(cf.focus + 1)
		}
		return nil
	}
	if key == "enter" {
		cf.setFocus(cf.focus + 1)
		return nil
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(k)
	return cmd
}

func choiceIndex(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return 0
}

// viewForm renders the form as a box.
func (p *Panel) viewForm(w, h int) []string {
	cf := p.form
	labelW := 13
	valueW := max(min(w-labelW-8, 70), 12)
	rows := []string{stTitle.Render(cf.title), ""}
	vis := cf.visibleFields()
	for i, f := range vis {
		label := stLabel.Render(fit(f.label, labelW))
		var val string
		switch {
		case f.choices != nil:
			v := f.input.Value()
			if v == "" {
				v = "default"
			}
			if i == cf.focus {
				val = stFocus.Render("‹ " + v + " ›")
			} else {
				val = v
			}
		case i == cf.focus:
			f.input.SetWidth(valueW)
			val = f.input.View()
		default:
			val = f.input.Value()
			if f.key == "passwd" && val != "" {
				val = strings.Repeat("•", min(len(val), 8))
			}
			val = fit(val, valueW)
		}
		marker := "  "
		if i == cf.focus {
			marker = stFocus.Render("▸ ")
		}
		rows = append(rows, marker+label+" "+val)
	}
	if f := cf.focused(); f != nil && f.hint != "" {
		rows = append(rows, "", stDim.Render(fit(f.hint, labelW+valueW+3)))
	} else {
		rows = append(rows, "", "")
	}
	switch {
	case cf.status != "" && cf.err:
		rows = append(rows, stErr.Render(fit(cf.status, labelW+valueW+3)))
	case cf.status != "":
		rows = append(rows, stAccent.Render(fit(cf.status, labelW+valueW+3)))
	default:
		rows = append(rows, "")
	}
	rows = append(rows, stKey.Render(p.k("formSave"))+stDim.Render(" save · ")+
		stKey.Render(p.k("formTest"))+stDim.Render(" test · ")+
		stKey.Render(p.k("formNext"))+stDim.Render(" next · ")+
		stKey.Render(p.k("formCancel"))+stDim.Render(" cancel"))
	// keep the focused row visible in a short pane
	if extra := len(rows) + 2 - h; extra > 0 {
		cut := min(extra, max(cf.focus, 0))
		rows = append(rows[:2], rows[2+cut:]...)
	}
	return strings.Split(stMenuBox.Render(strings.Join(rows, "\n")), "\n")
}

// staticCursor: a text input whose cursor doesn't blink (no tick messages).
func staticCursor(in *textinput.Model) {
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
}
