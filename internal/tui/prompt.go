package tui

// A one-line prompt over the panel (renaming a query file).

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type prompt struct {
	title, hint string
	input       textinput.Model
	run         func(value string) tea.Cmd
}

func newPrompt(title, hint, value string, run func(string) tea.Cmd) *prompt {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 512
	staticCursor(&in)
	in.SetValue(value)
	in.Focus()
	in.CursorEnd()
	return &prompt{title: title, hint: hint, input: in, run: run}
}

func (p *Panel) promptKey(k tea.KeyPressMsg) tea.Cmd {
	pr := p.prompt
	switch k.String() {
	case "esc":
		p.prompt = nil
		return nil
	case "enter":
		v := strings.TrimSpace(pr.input.Value())
		p.prompt = nil
		if v == "" {
			return nil
		}
		return pr.run(v)
	}
	var cmd tea.Cmd
	pr.input, cmd = pr.input.Update(k)
	return cmd
}

func (p *Panel) viewPrompt(w int) []string {
	pr := p.prompt
	bw := min(max(w-8, 30), 70)
	pr.input.SetWidth(bw - 4)
	body := []string{stTitle.Render(pr.title), "", pr.input.View(), "", stDim.Render(pr.hint)}
	return strings.Split(stMenuBox.Width(bw+2).Render(lipgloss.JoinVertical(lipgloss.Left, body...)), "\n")
}
