package handler

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

func sigParams(t *testing.T, s string) lsp.SignatureHelpParams {
	t.Helper()
	i := strings.Index(s, "|")
	before := s[:i]
	line := strings.Count(before, "\n")
	col := len(before) - (strings.LastIndex(before, "\n") + 1)
	return lsp.SignatureHelpParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Line: line, Character: col},
	}}
}

func TestFunctionSignatureHelp(t *testing.T) {
	in := "select regexp_replace(name, 'x', |"
	text := strings.Replace(in, "|", "", 1)
	h := FunctionSignatureHelp(text, sigParams(t, in), nil, dialect.DatabaseDriverPostgreSQL, true, false)
	if h == nil || len(h.Signatures) != 1 {
		t.Fatalf("got %+v", h)
	}
	sig := h.Signatures[0]
	if h.ActiveParameter != 2 {
		t.Errorf("active parameter %v", h.ActiveParameter)
	}
	off := sig.Parameters[2].Label.([]int)
	if got := sig.Label[off[0]:off[1]]; got != "replacement text" || sig.Label != "regexp_replace(string text, pattern text, replacement text, [flags text]) → text" {
		t.Errorf("parameter 2 label %q in %q", got, sig.Label)
	}

	// variadic: the last parameter stays active
	in = "select coalesce(a, b, c, |"
	h = FunctionSignatureHelp(strings.Replace(in, "|", "", 1), sigParams(t, in), nil, dialect.DatabaseDriverPostgreSQL, true, false)
	if h == nil || h.ActiveParameter != 1 {
		t.Fatalf("variadic: %+v", h)
	}

	// overloads: the first one that takes the argument is active
	cache := &database.DBCache{Functions: database.NewFunctionCatalog(dialect.DatabaseDriverPostgreSQL, []*dialect.Function{
		{Name: "my_trunc", Args: "text, timestamp with time zone", Result: "timestamp with time zone"},
		{Name: "my_trunc", Args: "text, timestamp with time zone, text", Result: "timestamp with time zone"},
	})}
	in = "select my_trunc('day', x, |"
	h = FunctionSignatureHelp(strings.Replace(in, "|", "", 1), sigParams(t, in), cache, dialect.DatabaseDriverPostgreSQL, true, false)
	if h == nil || h.ActiveSignature != 1 || h.ActiveParameter != 2 {
		t.Fatalf("overloads: %+v", h)
	}

	// not a function call
	in = "select * from t where id in (|"
	if h := FunctionSignatureHelp(strings.Replace(in, "|", "", 1), sigParams(t, in), nil, dialect.DatabaseDriverPostgreSQL, true, false); h != nil {
		t.Errorf("IN list: %+v", h)
	}
}
