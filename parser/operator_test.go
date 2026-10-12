package parser

import (
	"testing"

	"github.com/raikhan/sqlh/ast"
)

// PostgreSQL operators and casts must form Operator nodes, so that the select
// list is parsed as a list (completion relies on it).
func TestPostgresOperatorNodes(t *testing.T) {
	cases := map[string]string{
		"select sds.raw_data->>'language', sds.id from t sds": "sds.raw_data->>'language'",
		"select d->'a'->>'b', x from t":                       "d->'a'->>'b'",
		"select x::text, y from t":                            "x::text",
		"select a.name || ' ' || a.id from t a":               "a.name || ' ' || a.id",
		"select tags @> '{}', y from t":                       "tags @> '{}'",
	}
	for src, want := range cases {
		parsed, err := Parse(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		found := false
		var walk func(n ast.Node)
		walk = func(n ast.Node) {
			if op, ok := n.(*ast.Operator); ok && op.String() == want {
				found = true
			}
			if tl, ok := n.(ast.TokenList); ok {
				for _, c := range tl.GetTokens() {
					walk(c)
				}
			}
		}
		walk(parsed)
		if !found {
			t.Errorf("%q: no Operator node %q", src, want)
		}
	}
}
