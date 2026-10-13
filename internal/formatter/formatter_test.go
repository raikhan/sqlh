package formatter

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/parser"
)

func TestEval(t *testing.T) {
	testcases := []struct {
		name     string
		input    string
		params   lsp.DocumentFormattingParams
		config   *config.Config
		expected string
	}{
		{
			name:     "InsertIntoFormat",
			input:    "INSERT INTO users (NAME, email) VALUES ('john doe', 'example@host.com')",
			expected: "INSERT INTO users(\n\tNAME,\n\temail\n)\nVALUES(\n\t'john doe',\n\t'example@host.com'\n)",
			params:   lsp.DocumentFormattingParams{},
			config: &config.Config{
				LowercaseKeywords: false,
			},
		},
	}

	for _, tt := range testcases {
		t.Run(tt.name, func(t *testing.T) {
			actual, _ := Format(tt.input, tt.params, tt.config)
			if actual[0].NewText != tt.expected {
				t.Errorf("expected: %s, got %s", tt.expected, actual[0].NewText)
			}
		})
	}
}

func TestRenderIdentifier(t *testing.T) {
	testcases := []struct {
		name     string
		input    string
		opts     *ast.RenderOptions
		expected []string
	}{
		{
			name:  "snake case",
			input: "SELECT * FROM snake_case_table_name",
			opts: &ast.RenderOptions{
				LowerCase:        false,
				IdentifierQuoted: false,
			},
			expected: []string{
				"*",
				"snake_case_table_name",
			},
		},
		{
			name:  "pascal case",
			input: "SELECT p.PascalCaseColumnName FROM \"PascalCaseTableName\" p",
			opts: &ast.RenderOptions{
				LowerCase:        false,
				IdentifierQuoted: false,
			},
			expected: []string{
				"p.PascalCaseColumnName",
				"\"PascalCaseTableName\"",
			},
		},
		{
			name:  "quoted pascal case",
			input: "SELECT p.\"PascalCaseColumnName\" FROM \"PascalCaseTableName\" p",
			opts: &ast.RenderOptions{
				LowerCase:        false,
				IdentifierQuoted: false,
			},
			expected: []string{
				"p.\"PascalCaseColumnName\"",
				"\"PascalCaseTableName\"",
			},
		},
	}

	for _, tt := range testcases {
		t.Run(tt.name, func(t *testing.T) {
			stmts := parseInit(t, tt.input)
			list := stmts[0].GetTokens()
			j := 0
			for _, n := range list {
				if i, ok := n.(*ast.Identifier); ok {
					if actual := i.Render(tt.opts); actual != tt.expected[j] {
						t.Errorf("expected: %s, got %s", tt.expected[j], actual)
					}
					j++
				}
			}
		})
	}
}

func parseInit(t *testing.T, input string) []*ast.Statement {
	t.Helper()
	parsed, err := parser.Parse(input)
	if err != nil {
		t.Fatalf("error %+v\n", err)
	}

	var stmts []*ast.Statement
	for _, node := range parsed.GetTokens() {
		stmt, ok := node.(*ast.Statement)
		if !ok {
			t.Fatalf("invalid type want Statement parsed %T", stmt)
		}
		stmts = append(stmts, stmt)
	}
	return stmts
}

func TestFormat(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)

	files, err := filepath.Glob(filepath.Join(dir, "testdata", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)

	opts := &ast.RenderOptions{
		LowerCase:        false,
		IdentifierQuoted: false,
	}
	for _, fname := range files {
		b, err := os.ReadFile(fname)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.Parse(string(b))
		if err != nil {
			t.Fatal(err)
		}
		env := &formatEnvironment{}
		formatted := Eval(parsed, env)
		got := strings.TrimRight(formatted.Render(opts), "\n") + "\n"

		b, err = os.ReadFile(fname[:len(fname)-4] + ".golden")
		if err != nil {
			t.Fatal(err)
		}
		want := string(b)
		if got != want {
			if _, err := os.Stat(fname[:len(fname)-4] + ".ignore"); err == nil {
				t.Logf("%s:\n"+
					"    want: %q\n"+
					"     got: %q\n",
					fname, want, got)
			} else {
				t.Errorf("%s:\n"+
					"    want: %q\n"+
					"     got: %q\n",
					fname, want, got)
			}
		}
	}
}

// The WHERE clause of a formatted query, on one line (indent stripped).
func whereClause(t *testing.T, query string) string {
	t.Helper()
	edits, err := Format(query, lsp.DocumentFormattingParams{
		Options: lsp.FormattingOptions{TabSize: 4, InsertSpaces: true},
	}, &config.Config{LowercaseKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	out := edits[0].NewText
	i := strings.Index(out, "where\n")
	if i < 0 {
		t.Fatalf("no where clause in %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out[i+len("where\n"):]), "\n")
	for j := range lines {
		lines[j] = strings.TrimSpace(lines[j])
	}
	return strings.Join(lines, " | ")
}

func TestPredicateSpacing(t *testing.T) {
	tests := []struct{ in, want string }{
		// a call after a JSON operator expression was glued to it
		{"select * from t where tmp.raw_data->>'supplierName' like('%Merck%')", "tmp.raw_data->>'supplierName' like ('%Merck%')"},
		{"select * from t where j->>'k' like '%M%'", "j->>'k' like '%M%'"},
		{"select * from t where x::text || y like 'a'", "x::text || y like 'a'"},
		// predicates keep (or get) a space before their parenthesis
		{"select * from t where x in ('a', 'b')", "x in ( | 'a', | 'b' | )"}, // lists are expanded (existing layout)
		{"select * from t where x in('a')", "x in ('a')"},
		{"select * from t where x not in (1)", "x not in (1)"},
		// BETWEEN … AND is one predicate
		{"select * from t where a between 1 and 2 and b = 1", "a between 1 and 2 | and b = 1"},
		// calls are unchanged
		{"select * from t where lower(x) = 'a'", "lower(x) = 'a'"},
	}
	for _, tt := range tests {
		if got := whereClause(t, tt.in); got != tt.want {
			t.Errorf("%s\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}
