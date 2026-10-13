package completer

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

// cursor splits "…|…" into text and LSP position.
func cursor(t *testing.T, s string) (string, int, int) {
	t.Helper()
	i := strings.Index(s, "|")
	if i < 0 {
		t.Fatalf("no cursor in %q", s)
	}
	before := s[:i]
	line := strings.Count(before, "\n")
	col := len(before) - (strings.LastIndex(before, "\n") + 1)
	return s[:i] + s[i+1:], line, col
}

func TestFunctionCallAt(t *testing.T) {
	tests := []struct {
		in   string
		want string // qualifier.name#arg, "" = none
	}{
		{"select coalesce(a, |", "coalesce#1"},
		{"select coalesce(|)", "coalesce#0"},
		{"select coalesce (a, b, |) from t", "coalesce#2"},
		{"select pkg.fn(1, 'a,(b', |", "pkg.fn#2"},
		{"select f(g(1), |", "f#1"},
		{"select f(g(|", "g#0"},
		{"select f(1) + |", ""},
		// names are looked up in the function catalog, which rejects these
		{"select (|", "select#0"},
		{"select x from t where id in (|", "in#0"},
		{"select 1 + (|", ""},
		{"-- f(\nselect |", ""},
		{"/* f( */ select |", ""},
		{"select \"MyFn\"(1, |", "MyFn#1"},
		{"select f(1);\nselect |", ""},
		{"select to_char(\n  created,\n  |", "to_char#1"},
	}
	for _, tt := range tests {
		text, line, col := cursor(t, tt.in)
		call, ok := FunctionCallAt(text, line, col)
		got := ""
		if ok {
			got = call.Name
			if call.Qualifier != "" {
				got = call.Qualifier + "." + got
			}
			got += "#" + string(rune('0'+call.ArgIndex))
		}
		if got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.in, got, tt.want)
		}
	}
}

func complete(t *testing.T, c *Completer, input string, lower bool) []lsp.CompletionItem {
	t.Helper()
	text, line, col := cursor(t, input)
	items, err := c.Complete(text, lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			Position: lsp.Position{Line: line, Character: col},
		},
	}, lower)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func findItem(items []lsp.CompletionItem, label string) *lsp.CompletionItem {
	for i := range items {
		if items[i].Label == label {
			return &items[i]
		}
	}
	return nil
}

func TestFunctionCompletion(t *testing.T) {
	c := &Completer{Driver: dialect.DatabaseDriverPostgreSQL}

	items := complete(t, c, "select coal|", true)
	it := findItem(items, "coalesce")
	if it == nil {
		t.Fatalf("coalesce not offered: %+v", items)
	}
	if it.InsertText != "coalesce($0)" || it.InsertTextFormat != lsp.SnippetTextFormat {
		t.Errorf("insert: %q (%v)", it.InsertText, it.InsertTextFormat)
	}
	if !strings.Contains(it.Detail, "coalesce(value, ...)") {
		t.Errorf("detail: %q", it.Detail)
	}

	if it := findItem(complete(t, c, "select COAL|", false), "COALESCE"); it == nil {
		t.Error("upper-case keywords: COALESCE not offered")
	}
	if it := findItem(complete(t, c, "select current_d|", true), "current_date"); it == nil || it.InsertText != "current_date" {
		t.Errorf("current_date: %+v", it)
	}
	if it := findItem(complete(t, c, "select now|", true), "now"); it == nil || it.InsertText != "now()" {
		t.Errorf("now: %+v", it)
	}
	if it := findItem(complete(t, c, "select coal|(x) from t", true), "coalesce"); it == nil || it.InsertText != "coalesce" {
		t.Errorf("before a parenthesis: %+v", it)
	}
	if it := findItem(complete(t, c, "select * from t where lo|", true), "lower"); it == nil {
		t.Error("where: lower not offered")
	}
	if n := len(complete(t, c, "select |", true)); n > 50 {
		t.Errorf("no prefix: %d items, functions should wait for a prefix", n)
	}
}

func TestQualifiedFunctionCompletion(t *testing.T) {
	cache := &database.DBCache{
		Functions: database.NewFunctionCatalog(dialect.DatabaseDriverOracle, []*dialect.Function{
			{Qualifier: "CADE_PKG", Name: "GET_NAME", Args: "p_id NUMBER", Result: "VARCHAR2"},
			{Qualifier: "CADE_PKG", Name: "GET_NAME", Args: "p_code VARCHAR2", Result: "VARCHAR2"},
			{Name: "MY_FN", Result: "NUMBER"},
		}),
	}
	c := &Completer{Driver: dialect.DatabaseDriverOracle, DBCache: cache, LowercaseIdentifiers: true}
	items := complete(t, c, "select cade_pkg.|", true)
	it := findItem(items, "get_name")
	if it == nil {
		t.Fatalf("package member not offered: %+v", items)
	}
	if !strings.Contains(it.Detail, "(+1)") {
		t.Errorf("overloads in detail: %q", it.Detail)
	}
	if it := findItem(complete(t, c, "select my|", true), "my_fn"); it == nil || it.InsertText != "my_fn()" {
		t.Errorf("standalone function: %+v", it)
	}
	if it := findItem(complete(t, c, "select nv|", true), "nvl"); it == nil {
		t.Error("oracle built-in nvl not offered")
	}
}
