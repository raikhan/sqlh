package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pkey(p *picker, keys ...string) tea.Cmd {
	var last tea.Cmd
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "ctrl+r":
			msg = tea.KeyMsg{Type: tea.KeyCtrlR}
		case "ctrl+t":
			msg = tea.KeyMsg{Type: tea.KeyCtrlT}
		case "ctrl+a":
			msg = tea.KeyMsg{Type: tea.KeyCtrlA}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, last = p.Update(msg)
	}
	return last
}

func TestPickerFilterAndAccept(t *testing.T) {
	p := newPicker(items("customer", "orders", "order_line", "region"), PickOptions{Prompt: "table"})
	p.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	pkey(p, "o", "l")
	if v := plain(p.View()); !strings.Contains(v, "table> ol") || !strings.Contains(v, "order_line") || strings.Contains(v, "customer") {
		t.Errorf("view:\n%s", v)
	}
	pkey(p, "enter")
	if !p.accepted || strings.Join(p.result.Values, ",") != "order_line" || p.View() != "" {
		t.Errorf("result %+v accepted %v", p.result, p.accepted)
	}
}

func items(vals ...string) []PickItem {
	out := make([]PickItem, len(vals))
	for i, v := range vals {
		out[i] = PickItem{Value: v}
	}
	return out
}

func TestPickerCancelMultiExpect(t *testing.T) {
	p := newPicker(items("a", "b", "c"), PickOptions{})
	pkey(p, "esc")
	if p.accepted || !p.done {
		t.Error("esc should cancel")
	}
	p = newPicker(items("a", "b", "c"), PickOptions{Multi: true})
	pkey(p, "tab", "down", "tab", "enter") // marks a, then c (tab moved to b, down to c)
	if strings.Join(p.result.Values, ",") != "a,c" {
		t.Errorf("multi %v", p.result.Values)
	}
	p = newPicker(items("a", "b"), PickOptions{Multi: true})
	pkey(p, "ctrl+a", "enter")
	if strings.Join(p.result.Values, ",") != "a,b" {
		t.Errorf("all %v", p.result.Values)
	}
	p = newPicker(items("a", "b"), PickOptions{Expect: []string{"ctrl+t"}})
	pkey(p, "down", "ctrl+t")
	if p.result.Key != "ctrl+t" || p.result.Values[0] != "b" {
		t.Errorf("expect %+v", p.result)
	}
}

func TestPickerBindReload(t *testing.T) {
	var got string
	p := newPicker(items("orders", "customer"), PickOptions{Bind: map[string]func(*PickItem) (*PickReload, error){
		"ctrl+r": func(it *PickItem) (*PickReload, error) {
			got = it.Value
			return &PickReload{Items: items("region"), Prompt: "orders › customer"}, nil
		},
	}})
	pkey(p, "down", "ctrl+r")
	if got != "customer" || len(p.items) != 1 || p.prompt != "orders › customer" {
		t.Errorf("bind: got %q items %v prompt %q", got, p.items, p.prompt)
	}
	pkey(p, "enter")
	if p.result.Values[0] != "region" {
		t.Errorf("after reload %+v", p.result)
	}
}

func TestPickerAcceptQueryAndNoSort(t *testing.T) {
	num := func(q string) bool { return q != "" && strings.Trim(q, "0123456789") == "" }
	p := newPicker(items("100", "1000", "all"), PickOptions{AcceptQuery: num, NoSort: true})
	pkey(p, "1", "0", "enter")
	if p.result.Query != "10" {
		t.Errorf("typed number %+v", p.result)
	}
	p = newPicker(items("100", "1000", "all"), PickOptions{AcceptQuery: num, NoSort: true})
	pkey(p, "a", "enter")
	if p.result.Values[0] != "all" {
		t.Errorf("preset %+v", p.result)
	}
	// NoSort: input order among matches
	p = newPicker(items("1000", "100"), PickOptions{NoSort: true})
	pkey(p, "1", "0", "0")
	if p.items[p.results[0].index].Value != "1000" {
		t.Errorf("no sort: %v", p.results)
	}
}

func TestPickScripted(t *testing.T) {
	t.Setenv(PickEnv, "ol")
	res, ok, err := Pick(items("orders", "order_line"), PickOptions{})
	if err != nil || !ok || strings.Join(res.Values, ",") != "order_line" {
		t.Errorf("scripted %+v %v %v", res, ok, err)
	}
	t.Setenv(PickEnv, "or")
	res, _, _ = Pick(items("orders", "order_line", "region"), PickOptions{Multi: true})
	if len(res.Values) != 2 {
		t.Errorf("scripted multi %+v", res)
	}
	t.Setenv(PickEnv, "zz")
	if _, ok, _ := Pick(items("orders"), PickOptions{}); ok {
		t.Error("no match should cancel")
	}
}
