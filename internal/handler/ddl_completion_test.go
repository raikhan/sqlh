package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

// waitSecondary waits for the background load (views, objects).
func (tx *TestContext) waitSecondary(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := tx.server.worker.Cache(); c != nil && c.SecondaryLoaded && c.Relations != nil && c.Relations.ViewsLoaded {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background load not done")
}

func (tx *TestContext) complete(t *testing.T, text string, line, col int) []lsp.CompletionItem {
	t.Helper()
	tx.textDocumentDidOpen(t, testFileURI, text)
	params := lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: line, Character: col},
	}}
	var got []lsp.CompletionItem
	if err := tx.conn.Call(tx.ctx, "textDocument/completion", params, &got); err != nil {
		t.Fatal("completion:", err)
	}
	return got
}

// The census of PLAN.md §9.1: one case per position (| = cursor). The mock
// driver gets the generic keywords (no PostgreSQL-only IF EXISTS, RETURNING …).
var ddlCompletionCases = []struct {
	in   string
	want []string
	bad  []string
}{
	// CREATE TABLE
	{"CREATE TABLE |", []string{"IF NOT EXISTS"}, []string{"city", "SELECT"}},
	{"CREATE TABLE t (|", []string{"PRIMARY KEY", "CONSTRAINT", "FOREIGN KEY"}, []string{"city", "ID"}},
	{"CREATE TABLE t (id int, |)", []string{"PRIMARY KEY"}, []string{"ID"}},
	{"CREATE TABLE t (id |)", []string{"INTEGER", "VARCHAR", "TIMESTAMP"}, []string{"city", "PRIMARY KEY"}},
	{"CREATE TABLE t (id VA|)", []string{"VARCHAR"}, []string{"INTEGER"}},
	{"CREATE TABLE t (id int |)", []string{"NOT NULL", "DEFAULT", "PRIMARY KEY", "REFERENCES"}, []string{"SELECT", "city"}},
	{"CREATE TABLE t (id int DEFAULT |)", []string{"CURRENT_TIMESTAMP", "NULL"}, []string{"city"}},
	{"CREATE TABLE t (id int, cc char(3) REFERENCES |)", []string{"city", "country"}, []string{"SELECT"}},
	{"CREATE TABLE t (cc char(3) REFERENCES country (|))", []string{"Code", "Name"}, []string{"ID", "District"}},
	{"CREATE TABLE t (cc char(3) REFERENCES country(|))", []string{"Code"}, []string{"ID"}},
	{"CREATE TABLE t (id int, cc char(3), FOREIGN KEY (|))", []string{"id", "cc"}, []string{"ID", "Code"}},
	{"CREATE TABLE t (id int, cc char(3), FOREIGN KEY (cc) REFERENCES |)", []string{"country"}, nil},
	{"CREATE TABLE t (id int, cc char(3), PRIMARY KEY (|))", []string{"id", "cc"}, nil},
	{"CREATE TABLE t (id int CHECK (|))", []string{"id"}, []string{"ID"}},
	{"CREATE TABLE t (LIKE |)", []string{"city"}, nil},
	{"CREATE TABLE t AS SELECT | FROM city", []string{"ID", "Name"}, nil},
	// ALTER TABLE
	{"ALTER TABLE |", []string{"city", "country"}, []string{"SELECT"}},
	{"ALTER TABLE ci|", []string{"city"}, []string{"country"}},
	{"ALTER TABLE world.|", []string{"city", "country"}, nil},
	{"ALTER TABLE city |", []string{"ADD", "DROP COLUMN", "RENAME TO", "ALTER COLUMN"}, []string{"SELECT", "ID"}},
	{"ALTER TABLE city DROP COLUMN |", []string{"ID", "Name", "District"}, []string{"Code", "SELECT"}},
	{"ALTER TABLE city DROP |", []string{"ID", "COLUMN", "CONSTRAINT"}, nil},
	{"ALTER TABLE city RENAME COLUMN |", []string{"Name"}, nil},
	{"ALTER TABLE city ALTER COLUMN |", []string{"Name"}, nil},
	{"ALTER TABLE city ALTER COLUMN Name TYPE |", []string{"VARCHAR"}, []string{"Name"}},
	{"ALTER TABLE city ADD COLUMN x |", []string{"INTEGER"}, nil},
	{"ALTER TABLE city ADD x |", []string{"INTEGER"}, nil},
	{"ALTER TABLE city ADD CONSTRAINT fk FOREIGN KEY (|)", []string{"CountryCode"}, []string{"Code"}},
	{"ALTER TABLE city ADD CONSTRAINT fk FOREIGN KEY (CountryCode) REFERENCES |", []string{"country"}, nil},
	{"ALTER TABLE city ADD CONSTRAINT fk FOREIGN KEY (CountryCode) REFERENCES country (|)", []string{"Code"}, []string{"District"}},
	{"ALTER TABLE city ADD PRIMARY KEY (|)", []string{"ID"}, nil},
	{"ALTER TABLE city ADD CONSTRAINT ck CHECK (|)", []string{"Population"}, nil},
	{"ALTER TABLE city DROP CONSTRAINT |", []string{"fk_city_country", "city_pkey"}, []string{"country_pkey", "ID"}},
	{"ALTER TABLE city RENAME |", []string{"COLUMN", "TO", "Name"}, nil},
	// DROP / TRUNCATE
	{"DROP TABLE |", []string{"city", "country", "IF EXISTS"}, []string{"SELECT"}},
	{"DROP TABLE IF EXISTS |", []string{"city"}, []string{"IF EXISTS"}},
	{"DROP TABLE city, |", []string{"country"}, nil},
	{"DROP TABLE world.|", []string{"city"}, nil},
	{"DROP TABLE city |", []string{"CASCADE"}, []string{"city"}},
	{"DROP VIEW |", []string{"IF EXISTS"}, []string{"city"}},
	{"DROP INDEX |", []string{"idx_city_name", "city_pkey"}, []string{"city"}},
	{"DROP SEQUENCE |", []string{"city_id_seq"}, nil},
	{"DROP TRIGGER |", []string{"trg_city_audit"}, nil},
	{"DROP TYPE |", []string{"continent_kind"}, nil},
	{"TRUNCATE |", []string{"city", "TABLE"}, nil},
	{"TRUNCATE TABLE |", []string{"city"}, []string{"TABLE"}},
	// CREATE INDEX / VIEW / TRIGGER
	{"CREATE INDEX i ON |", []string{"city", "country"}, nil},
	{"CREATE INDEX i ON city (|)", []string{"Name", "District"}, []string{"Code", "city"}},
	{"CREATE INDEX i ON city(|)", []string{"Name"}, []string{"city"}},
	{"CREATE UNIQUE INDEX i ON city USING btree (|)", []string{"Name"}, nil},
	{"CREATE UNIQUE INDEX i ON city (Name) WHERE |", []string{"Population"}, []string{"Code"}},
	{"CREATE VIEW v AS SELECT | FROM city", []string{"ID", "Name"}, nil},
	{"CREATE OR REPLACE VIEW v AS SELECT c.| FROM city c", []string{"ID", "Name"}, nil},
	{"CREATE VIEW v AS SELECT * FROM city c JOIN |", []string{"country"}, nil},
	{"CREATE VIEW v (|) AS SELECT ID FROM city", nil, []string{"ID", "city"}},
	{"CREATE TRIGGER trg AFTER INSERT ON |", []string{"city"}, nil},
	// COMMENT / GRANT
	{"COMMENT ON TABLE |", []string{"city"}, nil},
	{"COMMENT ON COLUMN |", []string{"city"}, nil},
	{"COMMENT ON COLUMN city.|", []string{"Name", "ID"}, []string{"Code"}},
	{"GRANT SELECT ON |", []string{"city"}, nil},
	// DML tails
	{"INSERT INTO city (Name) VALUES ('x') |", nil, []string{"SELECT", "ID"}},
	{"INSERT INTO city (Name) VALUES ('x') RETURNING |", []string{"ID", "Name"}, []string{"Code", "SELECT"}},
	{"INSERT INTO city (Name) VALUES ('x') RETURNING ID, |", []string{"Name"}, nil},
	{"INSERT INTO city (Name) VALUES ('x') ON CONFLICT (|)", []string{"Name"}, nil},
	{"INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO |", []string{"NOTHING", "UPDATE SET"}, []string{"SELECT"}},
	{"INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET |", []string{"Name", "Population"}, []string{"country", "Code"}},
	{"INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET Name = EXCLUDED.|", []string{"Name"}, []string{"Code"}},
	{"INSERT INTO city (|)", []string{"Name"}, nil},
	{"UPDATE city SET Name = 'x' RETURNING |", []string{"ID"}, []string{"Code"}},
	{"DELETE FROM city WHERE ID = 1 RETURNING |", []string{"ID"}, nil},
	{"UPDATE city SET Name = 'x' WHERE |", []string{"ID", "Name"}, nil},
	{"MERGE INTO |", []string{"city"}, nil},
	{"MERGE INTO city c USING |", []string{"country"}, nil},
	{"MERGE INTO city c USING country s ON (c.|)", []string{"ID"}, nil},
	{"MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN MATCHED THEN UPDATE SET |", []string{"Name"}, []string{"Code", "country"}},
}

func TestDDLCompletion(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	tx.addWorkspaceConfig(t, &config.Config{Connections: []*database.DBConfig{{Driver: "mock"}}})
	tx.waitSecondary(t)
	for _, c := range ddlCompletionCases {
		t.Run(c.in, func(t *testing.T) {
			i := strings.Index(c.in, "|")
			got := tx.complete(t, c.in[:i]+c.in[i+1:], 0, i)
			testCompletionItem(t, c.want, c.bad, got)
		})
	}
}

func TestDDLCompletionLowercase(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	tx.addWorkspaceConfig(t, &config.Config{LowercaseKeywords: true, Connections: []*database.DBConfig{{Driver: "mock"}}})
	got := tx.complete(t, "CREATE TABLE t (id int ", 0, 23)
	testCompletionItem(t, []string{"not null", "primary key"}, []string{"NOT NULL"}, got)
	got = tx.complete(t, "CREATE TABLE t (id ", 0, 19)
	testCompletionItem(t, []string{"integer"}, []string{"INTEGER"}, got)
}

func TestDDLCompletionRanking(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	tx.addWorkspaceConfig(t, &config.Config{Connections: []*database.DBConfig{{Driver: "mock"}}})
	tx.waitSecondary(t)
	sortOf := func(items []lsp.CompletionItem, label string) string {
		for _, it := range items {
			if it.Label == label {
				return it.SortText
			}
		}
		return ""
	}
	// the table guessed from the column name first
	got := tx.complete(t, "CREATE TABLE t (country_id int REFERENCES ", 0, 42)
	if a, b := sortOf(got, "country"), sortOf(got, "city"); a == "" || a >= b {
		t.Errorf("country %q should sort before city %q", a, b)
	}
	// key columns of the referenced table first
	got = tx.complete(t, "CREATE TABLE t (cc char(3) REFERENCES country (", 0, 47)
	if a, b := sortOf(got, "Code"), sortOf(got, "Name"); a == "" || a >= b {
		t.Errorf("Code %q should sort before Name %q", a, b)
	}
}
