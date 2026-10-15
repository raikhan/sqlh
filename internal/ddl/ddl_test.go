package ddl

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// describe renders a slot compactly: kinds, table, flags; keywords separately.
func describe(s *Slot) string {
	if s == nil {
		return "<nil>"
	}
	var parts []string
	if len(s.Kinds) > 0 {
		var ks []string
		for _, k := range s.Kinds {
			ks = append(ks, k.String())
		}
		parts = append(parts, strings.Join(ks, ","))
	}
	if s.Qualifier != "" {
		parts = append(parts, "qual="+s.Qualifier)
	}
	if s.Table != nil {
		parts = append(parts, "of="+s.Table.String())
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{s.Defined, "defined"}, {s.Types, "types"}, {s.Functions, "functions"}, {s.Excluded, "excluded"}, {s.PreferKeys, "keys"}} {
		if f.on {
			parts = append(parts, f.name)
		}
	}
	if s.Guess != "" {
		parts = append(parts, "guess="+s.Guess)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

func at(t *testing.T, d sqlsplit.Dialect, in string) *Analysis {
	t.Helper()
	i := strings.Index(in, "|")
	text := in[:i] + in[i+1:]
	lines := strings.Split(in[:i], "\n")
	a, ok := AtCursor(text, len(lines)-1, len([]rune(lines[len(lines)-1])), d)
	if !ok {
		t.Fatalf("%q: no analysis", in)
	}
	return a
}

type slotCase struct {
	in   string
	want string   // describe(slot); "handoff" for Handoff
	kw   []string // keywords that must be offered
	nokw []string // keywords that must not be offered
}

func runSlotCases(t *testing.T, d sqlsplit.Dialect, cases []slotCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			a := at(t, d, c.in)
			got := describe(a.Slot)
			if a.Handoff {
				got = "handoff"
			}
			if got != c.want {
				t.Errorf("slot = %q, want %q (keywords %v)", got, c.want, keywords(a))
			}
			have := map[string]bool{}
			for _, k := range keywords(a) {
				have[k] = true
			}
			for _, k := range c.kw {
				if !have[k] {
					t.Errorf("keyword %q missing from %v", k, keywords(a))
				}
			}
			for _, k := range c.nokw {
				if have[k] {
					t.Errorf("keyword %q offered: %v", k, keywords(a))
				}
			}
		})
	}
}

func keywords(a *Analysis) []string {
	if a.Slot == nil {
		return nil
	}
	k := append([]string(nil), a.Slot.Keywords...)
	sort.Strings(k)
	return k
}

func TestCreateTable(t *testing.T) {
	runSlotCases(t, sqlsplit.PostgreSQL, []slotCase{
		{in: "CREATE |", want: "-", kw: []string{"TABLE", "VIEW", "OR REPLACE", "MATERIALIZED VIEW", "UNIQUE INDEX"}},
		{in: "CREATE OR REPLACE |", want: "-", kw: []string{"VIEW"}, nokw: []string{"TABLE"}},
		{in: "CREATE TEMPORARY |", want: "-", kw: []string{"TABLE", "VIEW"}},
		{in: "CREATE TABLE |", want: "-", kw: []string{"IF NOT EXISTS"}},
		{in: "CREATE TABLE IF |", want: "-", kw: []string{"NOT EXISTS"}},
		{in: "CREATE TABLE IF NOT EXISTS |", want: "-", nokw: []string{"IF NOT EXISTS"}},
		{in: "CREATE TABLE t |", want: "-", kw: []string{"AS", "PARTITION OF"}},
		{in: "CREATE TABLE t (|", want: "-", kw: []string{"CONSTRAINT", "PRIMARY KEY", "FOREIGN KEY", "UNIQUE", "CHECK", "LIKE"}},
		{in: "CREATE TABLE t (id int, |", want: "-", kw: []string{"PRIMARY KEY"}},
		{in: "CREATE TABLE t (id |", want: "types"},
		{in: "CREATE TABLE t (id va|", want: "types"},
		{in: "CREATE TABLE t (id int |", want: "-", kw: []string{"NOT NULL", "DEFAULT", "PRIMARY KEY", "REFERENCES", "GENERATED ALWAYS AS IDENTITY"}},
		{in: "CREATE TABLE t (id timestamp |", want: "-", kw: []string{"WITH TIME ZONE", "WITHOUT TIME ZONE", "NOT NULL"}},
		{in: "CREATE TABLE t (id timestamp with |", want: "-", kw: []string{"TIME ZONE"}},
		{in: "CREATE TABLE t (id timestamp with time zone |", want: "-", kw: []string{"NOT NULL"}, nokw: []string{"WITH TIME ZONE"}},
		{in: "CREATE TABLE t (id double |", want: "-", kw: []string{"PRECISION", "NOT NULL"}},
		{in: "CREATE TABLE t (id timestamp(6) with |", want: "-", kw: []string{"TIME ZONE"}},
		{in: "CREATE TABLE t (id interval day(2) to |", want: "-", kw: []string{"SECOND"}},
		{in: "CREATE TABLE t (id interval day(2) to second(3) |", want: "-", kw: []string{"NOT NULL"}},
		{in: "CREATE TABLE t (id numeric(|", want: "-"},
		{in: "CREATE TABLE t (id numeric(12, 2) |", want: "-", kw: []string{"NOT NULL"}},
		{in: "CREATE TABLE t (id int NOT |", want: "-", kw: []string{"NULL"}},
		{in: "CREATE TABLE t (id int NOT NULL |", want: "-", kw: []string{"DEFAULT"}},
		{in: "CREATE TABLE t (id int DEFAULT |", want: "functions", kw: []string{"CURRENT_TIMESTAMP", "NULL"}},
		{in: "CREATE TABLE t (id int DEFAULT 0 |", want: "-", kw: []string{"NOT NULL", "PRIMARY KEY"}},
		{in: "CREATE TABLE t (id int, cc char(3) REFERENCES |", want: "table guess=cc"},
		{in: "CREATE TABLE t (customer_id int REFERENCES |", want: "table guess=customer_id"},
		{in: "CREATE TABLE t (cc char(3) REFERENCES country (|", want: "column of=country keys"},
		{in: "CREATE TABLE t (cc char(3) REFERENCES country(|", want: "column of=country keys"},
		{in: "CREATE TABLE t (cc char(3) REFERENCES world.country (|", want: "column of=world.country keys"},
		{in: "CREATE TABLE t (cc char(3) REFERENCES country (Code) |", want: "-", kw: []string{"ON DELETE", "NOT NULL"}},
		{in: "CREATE TABLE t (cc char(3) REFERENCES country (Code) ON DELETE |", want: "-", kw: []string{"CASCADE", "SET NULL"}},
		{in: "CREATE TABLE t (cc char(3) REFERENCES country ON DELETE CASCADE, |", want: "-", kw: []string{"PRIMARY KEY"}},
		{in: "CREATE TABLE t (cc char(3) REFERENCES world.|", want: "table qual=world"},
		{in: "CREATE TABLE t (id int, cc char(3), FOREIGN KEY (|", want: "defined"},
		{in: "CREATE TABLE t (id int, cc char(3), FOREIGN KEY (cc) REFERENCES |", want: "table guess=cc"},
		{in: "CREATE TABLE t (id int, cc char(3), FOREIGN KEY (cc) |", want: "-", kw: []string{"REFERENCES"}},
		{in: "CREATE TABLE t (id int, cc char(3), PRIMARY KEY (|", want: "defined"},
		{in: "CREATE TABLE t (id int, cc char(3), PRIMARY KEY (id, |", want: "defined"},
		{in: "CREATE TABLE t (id int, CONSTRAINT pk |", want: "-", kw: []string{"PRIMARY KEY", "FOREIGN KEY"}},
		{in: "CREATE TABLE t (id int CHECK (|", want: "defined functions", kw: []string{"NULL"}},
		{in: "CREATE TABLE t (id int, CHECK (id > 0 AND |", want: "defined functions"},
		{in: "CREATE TABLE t (LIKE |", want: "table,view,materialized view"},
		{in: "CREATE TABLE t (id int) |", want: "-", kw: []string{"INHERITS", "PARTITION BY"}},
		{in: "CREATE TABLE t (id int) INHERITS (|", want: "table"},
		{in: "CREATE TABLE t (id int GENERATED |", want: "-", kw: []string{"ALWAYS AS IDENTITY", "BY DEFAULT AS IDENTITY"}},
		{in: "CREATE TABLE t (id int GENERATED ALWAYS AS IDENTITY |", want: "-", kw: []string{"PRIMARY KEY"}},
		{in: "CREATE TABLE t AS SELECT | FROM city", want: "handoff"},
		{in: "CREATE TABLE t AS |", want: "-", kw: []string{"SELECT", "WITH"}},
		{in: "CREATE TABLE t (id int[] |", want: "-", kw: []string{"NOT NULL"}},
	})
}

func TestCreateTableDialects(t *testing.T) {
	runSlotCases(t, sqlsplit.Oracle, []slotCase{
		{in: "CREATE |", want: "-", kw: []string{"GLOBAL TEMPORARY TABLE", "SYNONYM", "BITMAP INDEX"}, nokw: []string{"UNLOGGED TABLE"}},
		{in: "CREATE TABLE t (n varchar2(10 |", want: "-", kw: []string{"CHAR", "BYTE"}},
		{in: "CREATE TABLE t (ts timestamp |", want: "-", kw: []string{"WITH LOCAL TIME ZONE", "WITH TIME ZONE"}, nokw: []string{"WITHOUT TIME ZONE"}},
		{in: "CREATE TABLE t (|", want: "-", nokw: []string{"LIKE"}},
	})
	runSlotCases(t, sqlsplit.SQLite, []slotCase{
		{in: "CREATE TABLE t (id INTEGER PRIMARY KEY |", want: "-", kw: []string{"AUTOINCREMENT", "NOT NULL"}},
		{in: "CREATE TABLE t (a, |", want: "-", kw: []string{"PRIMARY KEY"}},
		{in: "CREATE TABLE t (a) |", want: "-", kw: []string{"WITHOUT ROWID", "STRICT"}},
	})
}

func TestAlterTable(t *testing.T) {
	runSlotCases(t, sqlsplit.PostgreSQL, []slotCase{
		{in: "ALTER |", want: "-", kw: []string{"TABLE", "VIEW", "INDEX"}},
		{in: "ALTER TABLE |", want: "table", kw: []string{"IF EXISTS", "ONLY"}},
		{in: "ALTER TABLE world.|", want: "table qual=world"},
		{in: "ALTER TABLE city |", want: "-", kw: []string{"ADD", "DROP COLUMN", "ALTER COLUMN", "RENAME TO", "OWNER TO"}, nokw: []string{"MODIFY"}},
		{in: "ALTER TABLE city DROP COLUMN |", want: "column of=city", kw: []string{"IF EXISTS"}},
		{in: "ALTER TABLE city DROP COLUMN IF EXISTS |", want: "column of=city"},
		{in: "ALTER TABLE city DROP |", want: "column of=city", kw: []string{"COLUMN", "CONSTRAINT"}},
		{in: "ALTER TABLE city DROP COLUMN Name |", want: "-", kw: []string{"CASCADE"}},
		{in: "ALTER TABLE city DROP CONSTRAINT |", want: "constraint of=city", kw: []string{"IF EXISTS"}},
		{in: "ALTER TABLE city RENAME COLUMN |", want: "column of=city"},
		{in: "ALTER TABLE city RENAME |", want: "column of=city", kw: []string{"COLUMN", "TO", "CONSTRAINT"}},
		{in: "ALTER TABLE city RENAME COLUMN Name |", want: "-", kw: []string{"TO"}},
		{in: "ALTER TABLE city RENAME COLUMN Name TO |", want: "-"},
		{in: "ALTER TABLE city RENAME TO |", want: "-"},
		{in: "ALTER TABLE city ALTER COLUMN |", want: "column of=city"},
		{in: "ALTER TABLE city ALTER |", want: "column of=city", kw: []string{"COLUMN"}},
		{in: "ALTER TABLE city ALTER COLUMN Name |", want: "-", kw: []string{"TYPE", "SET DEFAULT", "DROP NOT NULL"}},
		{in: "ALTER TABLE city ALTER COLUMN Name TYPE |", want: "types"},
		{in: "ALTER TABLE city ALTER COLUMN Name SET |", want: "-", kw: []string{"DATA TYPE", "DEFAULT", "NOT NULL"}},
		{in: "ALTER TABLE city ALTER COLUMN Name SET DATA TYPE |", want: "types"},
		{in: "ALTER TABLE city ALTER COLUMN Name SET DEFAULT |", want: "functions", kw: []string{"CURRENT_TIMESTAMP"}},
		{in: "ALTER TABLE city ADD |", want: "-", kw: []string{"COLUMN", "CONSTRAINT", "PRIMARY KEY", "FOREIGN KEY"}},
		{in: "ALTER TABLE city ADD COLUMN |", want: "-", kw: []string{"IF NOT EXISTS"}},
		{in: "ALTER TABLE city ADD COLUMN x |", want: "types"},
		{in: "ALTER TABLE city ADD x |", want: "types"},
		{in: "ALTER TABLE city ADD x int |", want: "-", kw: []string{"NOT NULL", "REFERENCES"}},
		{in: "ALTER TABLE city ADD COLUMN country_id int REFERENCES |", want: "table guess=country_id"},
		{in: "ALTER TABLE city ADD CONSTRAINT fk FOREIGN KEY (|", want: "column of=city"},
		{in: "ALTER TABLE city ADD CONSTRAINT fk FOREIGN KEY (CountryCode) REFERENCES |", want: "table guess=CountryCode"},
		{in: "ALTER TABLE city ADD CONSTRAINT fk FOREIGN KEY (CountryCode) REFERENCES country (|", want: "column of=country keys"},
		{in: "ALTER TABLE city ADD PRIMARY KEY (|", want: "column of=city"},
		{in: "ALTER TABLE city ADD CONSTRAINT ck CHECK (|", want: "column of=city functions"},
		{in: "ALTER TABLE city ADD CONSTRAINT ck CHECK (Population > 0 AND |", want: "column of=city functions"},
		{in: "ALTER TABLE city ADD CONSTRAINT |", want: "-"},
		{in: "ALTER TABLE city ADD CONSTRAINT ck |", want: "-", kw: []string{"CHECK", "UNIQUE"}},
		{in: "ALTER TABLE city DROP COLUMN a, |", want: "-", kw: []string{"ADD", "DROP COLUMN"}},
		{in: "ALTER TABLE city VALIDATE CONSTRAINT |", want: "constraint of=city"},
		{in: "ALTER TABLE city ENABLE TRIGGER |", want: "trigger of=city", kw: []string{"ALL"}},
		{in: "ALTER VIEW |", want: "view", kw: []string{"IF EXISTS"}},
		{in: "ALTER VIEW v |", want: "-", kw: []string{"RENAME TO", "OWNER TO"}},
		{in: "ALTER VIEW v RENAME COLUMN |", want: "column of=v"},
		{in: "ALTER INDEX |", want: "index", kw: []string{"IF EXISTS"}},
		{in: "ALTER SEQUENCE s |", want: "-", kw: []string{"RESTART", "INCREMENT BY"}},
		{in: "ALTER TRIGGER trg ON |", want: "table,view"},
	})
	runSlotCases(t, sqlsplit.Oracle, []slotCase{
		{in: "ALTER TABLE city |", want: "-", kw: []string{"ADD", "MODIFY", "DROP PRIMARY KEY"}, nokw: []string{"ALTER COLUMN", "OWNER TO"}},
		{in: "ALTER TABLE city ADD (x |", want: "types"},
		{in: "ALTER TABLE city ADD (x NUMBER, y |", want: "types"},
		{in: "ALTER TABLE city MODIFY (|", want: "column of=city"},
		{in: "ALTER TABLE city MODIFY (Name |", want: "types", kw: []string{"NOT NULL"}},
		{in: "ALTER TABLE city MODIFY Name |", want: "types", kw: []string{"NOT NULL"}},
		{in: "ALTER TABLE city DROP (|", want: "column of=city"},
		{in: "ALTER TABLE city DROP COLUMN Name |", want: "-", kw: []string{"CASCADE CONSTRAINTS"}},
	})
	runSlotCases(t, sqlsplit.SQLite, []slotCase{
		{in: "ALTER |", want: "-", kw: []string{"TABLE"}, nokw: []string{"INDEX"}},
		{in: "ALTER TABLE city |", want: "-", kw: []string{"ADD COLUMN", "RENAME TO", "DROP COLUMN"}, nokw: []string{"ADD CONSTRAINT"}},
	})
}

func TestDropTruncateCommentGrant(t *testing.T) {
	runSlotCases(t, sqlsplit.PostgreSQL, []slotCase{
		{in: "DROP |", want: "-", kw: []string{"TABLE", "VIEW", "MATERIALIZED VIEW", "INDEX", "SEQUENCE"}},
		{in: "DROP TABLE |", want: "table", kw: []string{"IF EXISTS"}},
		{in: "DROP TABLE IF EXISTS |", want: "table"},
		{in: "DROP TABLE city, |", want: "table"},
		{in: "DROP TABLE world.|", want: "table qual=world"},
		{in: "DROP TABLE city |", want: "-", kw: []string{"CASCADE", "RESTRICT"}},
		{in: "DROP VIEW |", want: "view", kw: []string{"IF EXISTS"}},
		{in: "DROP MATERIALIZED VIEW |", want: "materialized view"},
		{in: "DROP INDEX |", want: "index", kw: []string{"CONCURRENTLY", "IF EXISTS"}},
		{in: "DROP SEQUENCE |", want: "sequence"},
		{in: "DROP TRIGGER |", want: "trigger"},
		{in: "DROP TRIGGER trg |", want: "-", kw: []string{"ON"}},
		{in: "DROP TRIGGER trg ON |", want: "table,view"},
		{in: "DROP TYPE |", want: "type"},
		{in: "DROP FUNCTION |", want: "function"},
		{in: "DROP TABLE |; SELECT 1", want: "table"},
		{in: "TRUNCATE |", want: "table", kw: []string{"TABLE", "ONLY"}},
		{in: "TRUNCATE TABLE |", want: "table", kw: []string{"ONLY"}},
		{in: "TRUNCATE TABLE city |", want: "-", kw: []string{"RESTART IDENTITY", "CASCADE"}},
		{in: "COMMENT |", want: "-", kw: []string{"ON"}},
		{in: "COMMENT ON |", want: "-", kw: []string{"TABLE", "COLUMN"}},
		{in: "COMMENT ON TABLE |", want: "table,view,materialized view"},
		{in: "COMMENT ON COLUMN |", want: "table,view,materialized view"},
		{in: "COMMENT ON COLUMN city.|", want: "column,table,view,materialized view qual=city of=city"},
		{in: "COMMENT ON COLUMN world.city.|", want: "column of=world.city"},
		{in: "COMMENT ON COLUMN city.Name |", want: "-", kw: []string{"IS"}},
		{in: "COMMENT ON TABLE city |", want: "-", kw: []string{"IS"}},
		{in: "GRANT |", want: "-", kw: []string{"SELECT", "ALL PRIVILEGES"}},
		{in: "GRANT SELECT |", want: "-", kw: []string{"ON"}},
		{in: "GRANT SELECT, |", want: "-", kw: []string{"INSERT"}},
		{in: "GRANT SELECT ON |", want: "table,view,materialized view", kw: []string{"TABLE", "ALL TABLES IN SCHEMA"}},
		{in: "GRANT SELECT ON city |", want: "-", kw: []string{"TO"}},
		{in: "REVOKE SELECT ON city |", want: "-", kw: []string{"FROM"}},
		{in: "GRANT SELECT ON city TO |", want: "-", kw: []string{"PUBLIC"}},
	})
	runSlotCases(t, sqlsplit.Oracle, []slotCase{
		{in: "DROP TABLE city |", want: "-", kw: []string{"CASCADE CONSTRAINTS", "PURGE"}, nokw: []string{"RESTRICT"}},
		{in: "RENAME |", want: "table,view,materialized view"},
		{in: "RENAME city |", want: "-", kw: []string{"TO"}},
	})
}

func TestCreateIndexViewTrigger(t *testing.T) {
	runSlotCases(t, sqlsplit.PostgreSQL, []slotCase{
		{in: "CREATE INDEX |", want: "-", kw: []string{"ON", "IF NOT EXISTS", "CONCURRENTLY"}},
		{in: "CREATE INDEX i |", want: "-", kw: []string{"ON"}},
		{in: "CREATE INDEX i ON |", want: "table,materialized view", kw: []string{"ONLY"}},
		{in: "CREATE INDEX ON |", want: "table,materialized view"},
		{in: "CREATE INDEX i ON city |", want: "-", kw: []string{"USING"}},
		{in: "CREATE INDEX i ON city USING |", want: "-", kw: []string{"btree", "gin"}},
		{in: "CREATE INDEX i ON city (|", want: "column of=city functions"},
		{in: "CREATE INDEX i ON city(|", want: "column of=city functions"},
		{in: "CREATE INDEX i ON city USING btree (|", want: "column of=city functions"},
		{in: "CREATE INDEX i ON city (Name, |", want: "column of=city functions"},
		{in: "CREATE INDEX i ON city (lower(|", want: "column of=city functions"},
		{in: "CREATE INDEX i ON city (Name |", want: "-", kw: []string{"DESC", "NULLS LAST"}},
		{in: "CREATE INDEX i ON city (Name) |", want: "-", kw: []string{"WHERE", "INCLUDE"}},
		{in: "CREATE UNIQUE INDEX i ON city (Name) WHERE |", want: "column of=city functions"},
		{in: "CREATE UNIQUE INDEX i ON city (Name) WHERE Population > 0 |", want: "-", kw: []string{"AND"}},
		{in: "CREATE INDEX i ON city (Name) INCLUDE (|", want: "column of=city"},
		{in: "CREATE VIEW v AS SELECT | FROM city", want: "handoff"},
		{in: "CREATE OR REPLACE VIEW v AS SELECT c.| FROM city c", want: "handoff"},
		{in: "CREATE VIEW v |", want: "-", kw: []string{"AS"}},
		{in: "CREATE VIEW v (|", want: "-"},
		{in: "CREATE MATERIALIZED VIEW v AS SELECT | FROM city", want: "handoff"},
		{in: "CREATE TRIGGER trg |", want: "-", kw: []string{"BEFORE", "AFTER", "INSTEAD OF"}},
		{in: "CREATE TRIGGER trg AFTER |", want: "-", kw: []string{"INSERT", "UPDATE"}},
		{in: "CREATE TRIGGER trg AFTER INSERT |", want: "-", kw: []string{"ON", "OR"}},
		{in: "CREATE TRIGGER trg AFTER INSERT ON |", want: "table,view"},
		{in: "CREATE TRIGGER trg AFTER INSERT OR UPDATE ON |", want: "table,view"},
		{in: "CREATE TRIGGER trg AFTER INSERT ON city |", want: "-", kw: []string{"FOR EACH ROW", "EXECUTE FUNCTION"}},
		{in: "CREATE SEQUENCE s |", want: "-", kw: []string{"START WITH", "INCREMENT BY"}},
		{in: "CREATE SEQUENCE s START WITH 1 |", want: "-", kw: []string{"INCREMENT BY"}},
		{in: "CREATE SEQUENCE s START WITH |", want: "-"},
	})
	runSlotCases(t, sqlsplit.Oracle, []slotCase{
		{in: "CREATE SYNONYM s FOR |", want: "table,view,materialized view"},
	})
}

func TestDMLTails(t *testing.T) {
	runSlotCases(t, sqlsplit.PostgreSQL, []slotCase{
		{in: "INSERT INTO |", want: "handoff"},
		{in: "INSERT INTO city (|", want: "handoff"},
		{in: "INSERT INTO city (Name) VALUES (|", want: "handoff"},
		{in: "INSERT INTO city (Name) VALUES ('x') |", want: "-", kw: []string{"RETURNING", "ON CONFLICT"}},
		{in: "INSERT INTO city (Name) VALUES ('x'), ('y') |", want: "-", kw: []string{"RETURNING"}},
		{in: "INSERT INTO city (Name) VALUES ('x') RETURNING |", want: "column of=city functions"},
		{in: "INSERT INTO city (Name) VALUES ('x') RETURNING ID, |", want: "column of=city functions"},
		{in: "INSERT INTO city (Name) VALUES ('x') RETURNING ID |", want: "-", kw: []string{"AS"}},
		{in: "INSERT INTO world.city (Name) VALUES ('x') RETURNING |", want: "column of=world.city functions"},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT |", want: "-", kw: []string{"DO NOTHING", "DO UPDATE SET", "ON CONSTRAINT"}},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (|", want: "column of=city"},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) |", want: "-", kw: []string{"DO NOTHING", "DO UPDATE SET"}},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT ON CONSTRAINT |", want: "constraint of=city"},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO |", want: "-", kw: []string{"NOTHING", "UPDATE SET"}},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET |", want: "column of=city"},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET Name = |", want: "column of=city functions excluded"},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET Name = EXCLUDED.|", want: "column of=city"},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET Name = EXCLUDED.Name |", want: "-", kw: []string{"WHERE", "RETURNING"}},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET Name = EXCLUDED.Name, |", want: "column of=city"},
		{in: "INSERT INTO city (Name) VALUES ('x') ON CONFLICT DO NOTHING |", want: "-", kw: []string{"RETURNING"}},
		{in: "INSERT INTO city (Name) SELECT | FROM country", want: "handoff"},
		{in: "UPDATE city SET Name = 'x' RETURNING |", want: "column of=city functions"},
		{in: "UPDATE city SET Name = 'x' WHERE |", want: "handoff"},
		{in: "UPDATE city SET |", want: "handoff"},
		{in: "DELETE FROM city WHERE ID = 1 RETURNING |", want: "column of=city functions"},
		{in: "DELETE FROM city WHERE |", want: "handoff"},
		{in: "WITH x AS (SELECT 1) INSERT INTO city (Name) VALUES ('x') RETURNING |", want: "column of=city functions"},
		{in: "WITH x AS (SELECT | FROM city) INSERT INTO city (Name) VALUES ('x')", want: "handoff"},
		{in: "MERGE |", want: "-", kw: []string{"INTO"}},
		{in: "MERGE INTO |", want: "table,view,materialized view"},
		{in: "MERGE INTO city c |", want: "-", kw: []string{"USING"}},
		{in: "MERGE INTO city c USING |", want: "table,view,materialized view"},
		{in: "MERGE INTO city c USING country s |", want: "-", kw: []string{"ON"}},
		{in: "MERGE INTO city c USING country s ON (c.|", want: "handoff"},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) |", want: "-", kw: []string{"WHEN MATCHED THEN", "WHEN NOT MATCHED THEN"}},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN MATCHED |", want: "-", kw: []string{"THEN", "AND"}},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN MATCHED THEN |", want: "-", kw: []string{"UPDATE SET", "DELETE"}},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN MATCHED THEN UPDATE SET |", want: "column of=city"},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN MATCHED THEN UPDATE SET Name = |", want: "handoff"},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN MATCHED THEN UPDATE SET Name = s.Name, |", want: "column of=city"},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN NOT MATCHED THEN INSERT (|", want: "column of=city"},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN NOT MATCHED THEN INSERT (Name) VALUES (|", want: "handoff"},
		{in: "MERGE INTO city c USING country s ON (c.ID = s.Capital) WHEN MATCHED THEN DELETE |", want: "handoff"},
	})
	runSlotCases(t, sqlsplit.Oracle, []slotCase{
		{in: "INSERT INTO city (Name) VALUES ('x') |", want: "-", kw: []string{"RETURNING"}, nokw: []string{"ON CONFLICT"}},
		{in: "INSERT INTO city (Name) VALUES ('x') RETURNING ID |", want: "-", kw: []string{"INTO"}},
	})
}

func TestNotHandled(t *testing.T) {
	for _, in := range []string{"SELECT | FROM city", "|", "SELECT 1; |", "BEGIN |"} {
		i := strings.Index(in, "|")
		a, ok := AtCursor(in[:i]+in[i+1:], 0, i, sqlsplit.PostgreSQL)
		if ok && (a.Kind != "" || a.Slot != nil) {
			t.Errorf("%q: handled as %q (%s)", in, a.Kind, describe(a.Slot))
		}
	}
}

func TestRefs(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"CREATE TABLE t (id int PRIMARY KEY, cc char(3) REFERENCES country (Code), PRIMARY KEY (id, nope))",
			[]string{"create table t", "use table country", "use column country.Code", "defined column t.id", "defined column t.nope"}},
		{"ALTER TABLE world.city DROP COLUMN IF EXISTS Name, ADD COLUMN x int, RENAME COLUMN a TO b",
			[]string{"use table world.city", "drop column world.city.Name ifexists", "create column world.city.x", "use column world.city.a", "create column world.city.b renamed=a"}},
		{"DROP VIEW IF EXISTS a, b CASCADE", []string{"drop view a ifexists strict", "drop view b ifexists strict"}},
		{"DROP INDEX world.i", []string{"drop index world.i"}},
		{"CREATE INDEX i ON city (Name, lower(District))", []string{"create index i", "use table city", "use column city.Name"}},
		{"COMMENT ON COLUMN city.Name IS 'x'", []string{"use table city", "use column city.Name"}},
		{"INSERT INTO city (Name) VALUES ('x') ON CONFLICT (Name) DO UPDATE SET Population = EXCLUDED.Pop RETURNING ID, upper(Name)",
			[]string{"use table city", "use column city.Name", "use column city.Population", "use column city.Pop", "use column city.ID"}},
		{"ALTER TABLE city DROP CONSTRAINT fk_x", []string{"use table city", "drop constraint city.fk_x"}},
		{"CREATE VIEW v AS SELECT 1", []string{"create view v"}},
	}
	for _, c := range cases {
		a, _, ok := InStatement(c.sql, sqlsplit.PostgreSQL)
		if !ok {
			t.Fatalf("%q: tokenize", c.sql)
		}
		var got []string
		for _, r := range a.Refs {
			s := map[Role]string{RoleUse: "use", RoleCreate: "create", RoleDrop: "drop", RoleDefined: "defined"}[r.Role] + " " + r.Kind.String() + " "
			if r.Table != nil {
				s += r.Table.String() + "."
			}
			s += r.Name.String()
			if r.IfExists {
				s += " ifexists"
			}
			if r.StrictKind {
				s += " strict"
			}
			if r.Renamed != nil {
				s += " renamed=" + r.Renamed.Name
			}
			got = append(got, s)
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%q:\n got %q\nwant %q", c.sql, got, c.want)
		}
	}
}

func TestBody(t *testing.T) {
	a, toks, _ := InStatement("CREATE OR REPLACE VIEW v AS SELECT a FROM t", sqlsplit.PostgreSQL)
	if a.Body < 0 || !toks[a.Body].Is("SELECT") {
		t.Fatalf("body = %d", a.Body)
	}
	if a.Unsure {
		t.Error("unsure")
	}
}

func TestOracleNames(t *testing.T) {
	toks, _ := Tokenize("CREATE INDEX V500$ACT_i1 ON V500$ACT_GE(ID_, A#B)")
	var words []string
	for _, x := range toks {
		if x.IsWord() {
			words = append(words, x.Text)
		}
	}
	if got := strings.Join(words, " "); got != "CREATE INDEX V500$ACT_i1 ON V500$ACT_GE ID_ A#B" {
		t.Errorf("words %q", got)
	}
	a, _, _ := InStatement("CREATE TABLE V500$ACT (ID_ NVARCHAR2(64) NOT NULL, REV_ INTEGER)", sqlsplit.Oracle)
	if a.Unsure || len(a.Defined) != 2 {
		t.Errorf("unsure %v, defined %v", a.Unsure, a.Defined)
	}
}
