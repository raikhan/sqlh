package handler

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

type definitionTestCase struct {
	name  string
	input string
	pos   lsp.Position
	want  lsp.Definition
}

var definitionTestCases = []definitionTestCase{
	{
		name:  "subquery",
		input: "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
		want: []lsp.Location{
			{
				URI: testFileURI,
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      0,
						Character: 114,
					},
					End: lsp.Position{
						Line:      0,
						Character: 116,
					},
				},
			},
		},
	},
	{
		name:  "inner subquery",
		input: "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		pos: lsp.Position{
			Line:      0,
			Character: 36,
		},
		want: []lsp.Location{
			{
				URI: testFileURI,
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      0,
						Character: 107,
					},
					End: lsp.Position{
						Line:      0,
						Character: 109,
					},
				},
			},
		},
	},
	{
		name:  "alias",
		input: "SELECT ci.ID, ci.Name FROM city as ci",
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
		want: []lsp.Location{
			{
				URI: testFileURI,
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      0,
						Character: 35,
					},
					End: lsp.Position{
						Line:      0,
						Character: 37,
					},
				},
			},
		},
	},
}

func cteLocation(line, from, to int) []lsp.Location {
	return []lsp.Location{{
		URI: testFileURI,
		Range: lsp.Range{
			Start: lsp.Position{Line: line, Character: from},
			End:   lsp.Position{Line: line, Character: to},
		},
	}}
}

func init() {
	definitionTestCases = append(definitionTestCases,
		definitionTestCase{
			name:  "CTE reference in FROM",
			input: "with recent as (select ID from city) select r.ID from recent r",
			pos:   lsp.Position{Line: 0, Character: 59},
			want:  lsp.Definition(cteLocation(0, 5, 11)),
		},
		definitionTestCase{
			name:  "CTE with column list referenced in a later CTE",
			input: "with a(x) as (select 1), b as (select x from a) select * from b",
			pos:   lsp.Position{Line: 0, Character: 45},
			want:  lsp.Definition(cteLocation(0, 5, 6)),
		},
	)
}

func TestDefinition(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range definitionTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.DefinitionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: testFileURI,
					},
					Position: tt.pos,
				},
			}
			var got lsp.Definition
			err := tx.conn.Call(tx.ctx, "textDocument/definition", params, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/definition: %+v", err)
				return
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unmatch hover contents (- want, + got):\n%s", diff)
			}
		})
	}
}

func TestTypeDefinition(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range definitionTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.DefinitionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: testFileURI,
					},
					Position: tt.pos,
				},
			}
			var got lsp.Definition
			err := tx.conn.Call(tx.ctx, "textDocument/typeDefinition", params, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/definition: %+v", err)
				return
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unmatch hover contents (- want, + got):\n%s", diff)
			}
		})
	}
}
