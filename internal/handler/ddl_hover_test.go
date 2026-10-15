package handler

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

func TestDDLHover(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	tx.addWorkspaceConfig(t, &config.Config{Connections: []*database.DBConfig{{Driver: "mock"}}})
	tx.waitSecondary(t)
	cases := []struct {
		in   string // | before the hovered character
		want string // substring of the hover; "" = none
	}{
		{"DROP TABLE |city", "`city` table"},
		{"ALTER TABLE ci|ty ADD x int", "`city` table"},
		{"ALTER TABLE city DROP COLUMN Na|me", "`city`.`Name` column"},
		{"TRUNCATE TABLE |city", "`city` table"},
		{"CREATE INDEX i ON city (Na|me)", "`city`.`Name` column"},
		{"COMMENT ON TABLE ci|ty IS 'x'", "`city` table"},
		{"COMMENT ON COLUMN city.Dis|trict IS 'x'", "`city`.`District` column"},
		{"INSERT INTO city (Name) VALUES ('x') RETURNING I|D", "`city`.`ID` column"},
		{"DROP INDEX idx_city|_name", "`idx_city_name` index on `city` (Name)"},
		{"ALTER TABLE city DROP CONSTRAINT fk_city|_country", "foreign key constraint on `city` (CountryCode) → `country`"},
		{"DROP SEQUENCE city|_id_seq", "`city_id_seq` sequence"},
		{"CREATE TABLE t (|id int)", ""},
		{"SELECT Na|me FROM city", "`city`.`Name` column"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			i := strings.Index(c.in, "|")
			tx.textDocumentDidOpen(t, testFileURI, c.in[:i]+c.in[i+1:])
			var got lsp.Hover
			params := lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
				Position:     lsp.Position{Line: 0, Character: i},
			}}
			if err := tx.conn.Call(tx.ctx, "textDocument/hover", params, &got); err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if got.Contents.Value != "" {
					t.Errorf("unexpected hover %q", got.Contents.Value)
				}
				return
			}
			if !strings.Contains(got.Contents.Value, c.want) {
				t.Errorf("hover %q, want it to contain %q", got.Contents.Value, c.want)
			}
		})
	}
}
