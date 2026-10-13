package handler

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

var renameTestCases = []struct {
	name    string
	input   string
	newName string
	output  lsp.WorkspaceEdit
	pos     lsp.Position
}{
	{
		name:    "subquery",
		input:   "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		newName: "ct",
		output: lsp.WorkspaceEdit{
			DocumentChanges: []lsp.TextDocumentEdit{
				{
					TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
						Version: 0,
						TextDocumentIdentifier: lsp.TextDocumentIdentifier{
							URI: "file:///Users/octref/Code/css-test/test.sql",
						},
					},
					Edits: []lsp.TextEdit{
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 7,
								},
								End: lsp.Position{
									Line:      0,
									Character: 9,
								},
							},
							NewText: "ct",
						},
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 14,
								},
								End: lsp.Position{
									Line:      0,
									Character: 16,
								},
							},
							NewText: "ct",
						},
						{
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
							NewText: "ct",
						},
					},
				},
			},
		},
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
	},
	{
		name:    "ok",
		input:   "SELECT ci.ID, ci.Name FROM city as ci",
		newName: "ct",
		output: lsp.WorkspaceEdit{
			DocumentChanges: []lsp.TextDocumentEdit{
				{
					TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
						Version: 0,
						TextDocumentIdentifier: lsp.TextDocumentIdentifier{
							URI: "file:///Users/octref/Code/css-test/test.sql",
						},
					},
					Edits: []lsp.TextEdit{
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 7,
								},
								End: lsp.Position{
									Line:      0,
									Character: 9,
								},
							},
							NewText: "ct",
						},
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 14,
								},
								End: lsp.Position{
									Line:      0,
									Character: 16,
								},
							},
							NewText: "ct",
						},
						{
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
							NewText: "ct",
						},
					},
				},
			},
		},
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
	},
}

func TestRenameMain(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range renameTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.RenameParams{
				TextDocument: lsp.TextDocumentIdentifier{
					URI: testFileURI,
				},
				Position: tt.pos,
				NewName:  tt.newName,
			}
			var got lsp.WorkspaceEdit
			err := tx.conn.Call(tx.ctx, "textDocument/rename", params, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/rename: %+v", err)
				return
			}

			if diff := cmp.Diff(tt.output, got); diff != "" {
				t.Errorf("unmatch rename edits (- want, + got):\n%s", diff)
			}
		})
	}
}

// The cursor on the first character of a one-letter alias (e.g. "p" in
// "p.id") still renames the alias, not the whitespace before it.
func TestRenameAliasFirstChar(t *testing.T) {
	input := "select\n" +
		"    p.id,\n" +
		"    am.model_tag\n" +
		"from\n" +
		"    ai_extraction_process p\n" +
		"join ai_model am\n" +
		"    on am.id = p.model_id"
	params := lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 1, Character: 4},
		NewName:      "proc",
	}
	got, err := rename(input, 0, params)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.DocumentChanges) != 1 {
		t.Fatalf("no edits: %+v", got)
	}
	want := []lsp.Position{{Line: 1, Character: 4}, {Line: 4, Character: 26}, {Line: 6, Character: 15}}
	edits := got.DocumentChanges[0].Edits
	if len(edits) != len(want) {
		t.Fatalf("got %d edits, want %d: %+v", len(edits), len(want), edits)
	}
	for i, e := range edits {
		if e.Range.Start != want[i] || e.Range.End.Character != want[i].Character+1 || e.NewText != "proc" {
			t.Errorf("edit %d: got %+v, want start %+v", i, e, want[i])
		}
	}
}

// A rename after the document changed is made against its new version
// (clients reject edits for an outdated version, e.g. Helix's "outdated
// workspace edit").
func TestRenameTracksDocumentVersion(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.addWorkspaceConfig(t, &config.Config{Connections: []*database.DBConfig{{Driver: "mock"}}})

	renameAt := func(newName string) int32 {
		t.Helper()
		var got lsp.WorkspaceEdit
		params := lsp.RenameParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 7},
			NewName:      newName,
		}
		if err := tx.conn.Call(tx.ctx, "textDocument/rename", params, &got); err != nil {
			t.Fatal("conn.Call textDocument/rename:", err)
		}
		if len(got.DocumentChanges) != 1 {
			t.Fatalf("no edits: %+v", got)
		}
		return got.DocumentChanges[0].TextDocument.Version
	}

	tx.textDocumentDidOpen(t, testFileURI, "SELECT c.ID FROM city c")
	if v := renameAt("ct"); v != 0 {
		t.Errorf("first rename: version %d, want 0", v)
	}

	change := lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: testFileURI, Version: 3},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "SELECT ct.ID FROM city ct"}},
	}
	if err := tx.conn.Call(tx.ctx, "textDocument/didChange", change, nil); err != nil {
		t.Fatal("conn.Call textDocument/didChange:", err)
	}
	if v := renameAt("x"); v != 3 {
		t.Errorf("rename after change: version %d, want 3", v)
	}
}
