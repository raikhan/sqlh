package handler

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/raikhan/sqlh/internal/lsp"
)

func TestReferences(t *testing.T) {
	scratch := "select\n" +
		"    p.id,\n" +
		"    p.schema_id,\n" +
		"    am.model_tag\n" +
		"from\n" +
		"    ai_extraction_process p\n" +
		"join ai_model am\n" +
		"    on am.id = p.model_id"

	type span struct{ line, from, to int }
	cases := []struct {
		name  string
		input string
		pos   lsp.Position
		decl  bool
		want  []span
	}{
		{
			name:  "alias use",
			input: scratch,
			pos:   lsp.Position{Line: 1, Character: 4},
			decl:  true,
			want:  []span{{1, 4, 5}, {2, 4, 5}, {5, 26, 27}, {7, 15, 16}},
		},
		{
			name:  "alias declaration",
			input: scratch,
			pos:   lsp.Position{Line: 6, Character: 14},
			decl:  true,
			want:  []span{{3, 4, 6}, {6, 14, 16}, {7, 7, 9}},
		},
		{
			name:  "alias without declaration",
			input: scratch,
			pos:   lsp.Position{Line: 3, Character: 5},
			decl:  false,
			want:  []span{{3, 4, 6}, {7, 7, 9}},
		},
		{
			name:  "table: its aliases' uses",
			input: scratch,
			pos:   lsp.Position{Line: 5, Character: 10},
			decl:  true,
			want:  []span{{1, 4, 5}, {2, 4, 5}, {5, 4, 25}, {5, 26, 27}, {7, 15, 16}},
		},
		{
			name: "table: across statements",
			input: "SELECT c.ID FROM city c;\n" +
				"SELECT ID FROM country;\n" +
				"SELECT city.Name FROM city",
			pos:  lsp.Position{Line: 2, Character: 23},
			decl: true,
			want: []span{{0, 7, 8}, {0, 17, 21}, {0, 22, 23}, {2, 7, 11}, {2, 22, 26}},
		},
		{
			name:  "column: within the statement",
			input: "SELECT ci.ID FROM city ci WHERE ci.ID > 1;\nSELECT ID FROM country",
			pos:   lsp.Position{Line: 0, Character: 10},
			decl:  true,
			want:  []span{{0, 10, 12}, {0, 35, 37}},
		},
		{
			name:  "not an identifier",
			input: scratch,
			pos:   lsp.Position{Line: 0, Character: 2},
			decl:  true,
			want:  nil,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			params := lsp.ReferenceParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     tt.pos,
				},
				Context: lsp.ReferenceContext{IncludeDeclaration: tt.decl},
			}
			got, err := references(testFileURI, tt.input, params)
			if err != nil {
				t.Fatal(err)
			}
			var want []lsp.Location
			for _, s := range tt.want {
				want = append(want, lsp.Location{
					URI: testFileURI,
					Range: lsp.Range{
						Start: lsp.Position{Line: s.line, Character: s.from},
						End:   lsp.Position{Line: s.line, Character: s.to},
					},
				})
			}
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("unmatched references (-want +got):\n%s", diff)
			}
		})
	}
}
