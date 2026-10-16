package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/raikhan/sqlh/internal/control"
)

func pkey(p *picker, keys ...string) tea.Cmd {
	var last tea.Cmd
	for _, k := range keys {
		_, last = p.Update(press(k))
	}
	return last
}

func TestPickerFilterAndAccept(t *testing.T) {
	p := newPicker(items("customer", "orders", "order_line", "region"), PickOptions{Prompt: "table"})
	p.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	pkey(p, "o", "l")
	if v := plain(p.render()); !strings.Contains(v, "table> ol") || !strings.Contains(v, "order_line") || strings.Contains(v, "customer") {
		t.Errorf("view:\n%s", v)
	}
	pkey(p, "enter")
	if !p.accepted || strings.Join(p.result.Values, ",") != "order_line" || p.render() != "" {
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

func TestConnectionPickItems(t *testing.T) {
	header, its := ConnectionPickItems([]control.Connection{
		{Alias: "cadeprod", Group: "CADE", Database: "CADEPROD", Schema: "CADE", Environment: "prod"},
		{Alias: "pg_local", Database: "sdsai", Schema: "cadetst", Environment: "dev", Current: true},
		{Alias: "scratch", Database: "scratch.db", Environment: "dev"},
	})
	h := plain(header)
	if !strings.Contains(h, "connection") || !strings.Contains(h, "schema") {
		t.Fatalf("header %q", h)
	}
	var vals []string
	for _, it := range its {
		vals = append(vals, it.Value)
	}
	if strings.Join(vals, ",") != "pg_local,scratch,cadeprod" {
		t.Errorf("order %v", vals)
	}
	// drawn by the picker: every row's columns under the header's, the
	// highlighted one too, the chip kept on it
	pk := newPicker(its, PickOptions{Header: header})
	pk.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	view := strings.Split(pk.render(), "\n")
	h = plain(view[1])
	col := func(s, sub string) int { return len([]rune(s[:strings.Index(s, sub)])) }
	for i, it := range its {
		row := plain(view[2+i])
		db := map[string]string{"cadeprod": "CADEPROD", "pg_local": "sdsai", "scratch": "scratch.db"}[it.Value]
		name := it.Value
		if name == "cadeprod" {
			name = "CADE/cadeprod"
		}
		if col(row, db) != col(h, "database") || col(row, name) != col(h, "connection") {
			t.Errorf("%s not aligned:\n%s\n%s", it.Value, h, row)
		}
		env := map[string]string{"cadeprod": "prod", "pg_local": "dev", "scratch": "dev"}[it.Value]
		if len([]rune(row[:strings.LastIndex(row, env)])) != col(h, "env") {
			t.Errorf("%s env not aligned:\n%s\n%s", it.Value, h, row)
		}
	}
	if !strings.Contains(view[2], "\x1b[30;42m") && !strings.Contains(pk.render(), "dev ") {
		t.Errorf("chip lost on the highlighted row: %q", view[2])
	}
	if !strings.Contains(its[2].Match, "CADE") || !strings.HasPrefix(plain(its[0].Display), "⛁ pg_local") || !strings.Contains(its[0].Display, "●") {
		t.Errorf("items %+v", its)
	}
}
