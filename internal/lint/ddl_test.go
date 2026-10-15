package lint

import (
	"fmt"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

func ddlCatalog() *Catalog {
	return NewCatalog(sqlsplit.PostgreSQL, "app").
		Schema("pg_catalog").
		Table("app", "customer", []string{"id", "name", "email"}).
		Table("app", "orders", []string{"id", "customer_id", "total"}).
		Table("app", "v_orders", []string{"id", "total"}).
		Table("app", "mv_totals", []string{"customer_id", "total"}).
		Kind("app", "v_orders", "view").
		Kind("app", "mv_totals", "materialized view").
		KindsKnown("app").
		Object("index", "app", "orders_customer_idx", "orders").
		Object("constraint", "app", "orders_pkey", "orders").
		Object("constraint", "app", "orders_customer_fk", "orders").
		Object("sequence", "app", "orders_id_seq", "").
		Object("trigger", "app", "orders_audit", "orders").
		ObjectsKnown(true).
		Complete(true).Build()
}

func TestDDLFlags(t *testing.T) {
	cases := []struct {
		sql  string
		want string
	}{
		{"DROP TABLE custmer", "unknown table custmer (did you mean customer?)"},
		{"DROP TABLE customer, nope", "unknown table nope"},
		{"DROP VIEW nope_view", "unknown view nope_view"},
		{"ALTER TABLE ordrs ADD x int", "unknown table ordrs (did you mean orders?)"},
		{"TRUNCATE TABLE nope", "unknown table nope"},
		{"CREATE INDEX i ON nope (a)", "unknown table nope"},
		{"COMMENT ON TABLE nope IS 'x'", "unknown table or view nope"},
		{"CREATE TABLE t (id int, cid int REFERENCES custmer (id))", "unknown table custmer (did you mean customer?)"},
		{"CREATE TABLE t (id int, cid int REFERENCES customer (idd))", "customer has no column idd (did you mean id?)"},
		{"ALTER TABLE orders DROP COLUMN totl", "orders has no column totl (did you mean total?)"},
		{"ALTER TABLE orders RENAME COLUMN totl TO amount", "orders has no column totl"},
		{"ALTER TABLE orders RENAME COLUMN total TO id", "orders already has a column id"},
		{"ALTER TABLE orders ALTER COLUMN totl TYPE numeric", "orders has no column totl"},
		{"ALTER TABLE orders ADD COLUMN total numeric", "orders already has a column total"},
		{"ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (cust_id) REFERENCES customer (id)", "orders has no column cust_id"},
		{"CREATE INDEX i ON orders (custmer_id)", "orders has no column custmer_id (did you mean customer_id?)"},
		{"COMMENT ON COLUMN orders.totl IS 'x'", "orders has no column totl"},
		{"CREATE TABLE t (id int, PRIMARY KEY (idd))", "column idd is not defined in this table"},
		{"CREATE TABLE t (id int, cid int, FOREIGN KEY (cid, x) REFERENCES customer (id))", "column x is not defined in this table"},
		{"DROP VIEW orders", "orders is a table, not a view: use DROP TABLE"},
		{"DROP TABLE v_orders", "v_orders is a view, not a table: use DROP VIEW"},
		{"DROP VIEW mv_totals", "mv_totals is a materialized view, not a view: use DROP MATERIALIZED VIEW"},
		{"DROP INDEX orders_customer_idz", "unknown index orders_customer_idz (did you mean orders_customer_idx?)"},
		{"ALTER TABLE orders DROP CONSTRAINT orders_pkee", "unknown constraint orders_pkee on orders (did you mean orders_pkey?)"},
		{"ALTER TABLE customer DROP CONSTRAINT orders_pkey", "unknown constraint orders_pkey on customer"},
		{"ALTER TABLE customer DROP CONSTRAINT orders_pkey ", "unknown constraint orders_pkey on customer"},
		{"DROP SEQUENCE nope_seq", "unknown sequence nope_seq"},
		{"DROP TRIGGER nope ON orders", "unknown trigger nope on orders"},
		{"GRANT SELECT ON TABLE nope TO someone", "unknown table or view nope"},
		{"INSERT INTO orders (total) VALUES (1) RETURNING idd", "orders has no column idd (did you mean id?)"},
		{"INSERT INTO orders (id) VALUES (1) ON CONFLICT (idd) DO NOTHING", "orders has no column idd"},
		{"INSERT INTO orders (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET totl = EXCLUDED.total", "orders has no column totl"},
		{"MERGE INTO orders o USING customer c ON (o.customer_id = c.id) WHEN MATCHED THEN UPDATE SET totl = 0", "orders has no column totl"},
		{"CREATE VIEW v AS SELECT c.nme FROM customer c", "customer has no column nme (did you mean name?)"},
		{"CREATE TABLE t AS SELECT * FROM custmer", "unknown table custmer"},
	}
	for _, c := range cases {
		got := messages(t, ddlCatalog(), c.sql)
		if len(got) != 1 || !strings.HasPrefix(got[0], c.want) {
			t.Errorf("%s\n  got  %q\n  want %q", c.sql, got, c.want)
		}
	}
}

func TestDDLQuiet(t *testing.T) {
	for _, sql := range []string{
		"DROP TABLE IF EXISTS nope",
		"DROP VIEW IF EXISTS nope",
		"ALTER TABLE IF EXISTS nope ADD x int",
		"ALTER TABLE orders DROP COLUMN IF EXISTS nope",
		"ALTER TABLE orders ADD COLUMN IF NOT EXISTS total numeric",
		"DROP INDEX IF EXISTS nope",
		"ALTER TABLE orders DROP CONSTRAINT IF EXISTS nope",
		"CREATE TABLE t (id int REFERENCES t (id))", // self reference
		"CREATE TABLE t (LIKE customer, PRIMARY KEY (email))",
		"CREATE TABLE nope.t (id int REFERENCES nope.u (x))", // schema not cached
		"DROP TABLE other.whatever",
		"DROP TABLE customer",
		"DROP VIEW v_orders",
		"DROP MATERIALIZED VIEW mv_totals",
		"ALTER TABLE orders RENAME COLUMN total TO amount",
		"CREATE INDEX i ON orders (lower(customer_id::text), total DESC)",
		"CREATE INDEX i ON orders (total) WHERE whatever > 0", // expressions aren't checked
		"INSERT INTO orders (id) VALUES (1) RETURNING id, upper(total::text), CURRENT_TIMESTAMP",
		"INSERT INTO orders (id) VALUES (1) ON CONFLICT (lower(id::text)) DO NOTHING",
		"MERGE INTO orders o USING customer c ON (o.customer_id = c.id) WHEN MATCHED THEN UPDATE SET o.total = 0",
		"DROP FUNCTION nope(int)",
		"DROP TYPE nope",
		"GRANT SELECT ON customer TO someone",
		"GRANT SELECT ON some_seq TO someone",
		"GRANT EXECUTE ON some_pkg TO someone",
		"DROP TABLE pg_whatever",
		// earlier statements of the document
		"CREATE TABLE t (id int, name text); ALTER TABLE t ADD COLUMN x int; ALTER TABLE t DROP COLUMN name; CREATE INDEX i ON t (x)",
		"ALTER TABLE orders ADD COLUMN amount numeric; CREATE INDEX i ON orders (amount); ALTER TABLE orders DROP COLUMN amount",
		"CREATE INDEX nidx ON orders (total); DROP INDEX nidx",
		"ALTER TABLE orders RENAME TO purchase; ALTER TABLE purchase DROP COLUMN total",
		"DROP TABLE customer; DROP TABLE customer",
		"CREATE TABLE t (id int); DROP TABLE t",
		"CREATE SEQUENCE s; DROP SEQUENCE s",
	} {
		if got := messages(t, ddlCatalog(), sql); len(got) != 0 {
			t.Errorf("%s\n  got %q", sql, got)
		}
	}
}

func TestDDLDocumentOrder(t *testing.T) {
	cases := []struct{ sql, want string }{
		// only earlier statements count
		{"ALTER TABLE t ADD x int; CREATE TABLE t (id int)", "unknown table t"},
		{"CREATE TABLE t (id int); ALTER TABLE t DROP COLUMN nope", "t has no column nope"},
		{"ALTER TABLE orders DROP COLUMN total; CREATE INDEX i ON orders (total)", "orders has no column total"},
		{"DROP TABLE customer; ALTER TABLE customer ADD x int", "table customer is dropped by an earlier statement"},
	}
	for _, c := range cases {
		got := messages(t, ddlCatalog(), c.sql)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s\n  got  %q\n  want %q", c.sql, got, c.want)
		}
	}
}

func TestDDLSkip(t *testing.T) {
	sql := "DROP TABLE nope;\nALTER TABLE orders ADD COLUMN total int"
	all := CheckWith(sql, ddlCatalog(), Options{})
	if len(all) != 2 {
		t.Fatalf("got %v", all)
	}
	got := CheckWith(sql, ddlCatalog(), Options{SkipDDL: func(s string) bool { return strings.HasPrefix(s, "DROP") }})
	if len(got) != 1 || got[0].Message != "orders already has a column total" {
		t.Errorf("got %v", got)
	}
}

func TestDDLPositions(t *testing.T) {
	sql := "SELECT 1;\n  ALTER TABLE orders\n    DROP COLUMN totl"
	d := Check(sql, ddlCatalog())
	if len(d) != 1 {
		t.Fatalf("got %v", d)
	}
	if got := fmt.Sprint(d[0].Start, d[0].End); got != "{2 16} {2 20}" {
		t.Errorf("range %s", got)
	}
	sql = "CREATE VIEW v AS\n  SELECT c.nme FROM customer c"
	d = Check(sql, ddlCatalog())
	if len(d) != 1 || fmt.Sprint(d[0].Start, d[0].End) != "{1 11} {1 14}" {
		t.Errorf("view body: %v", d)
	}
	sql = "  CREATE VIEW v AS SELECT c.nme FROM customer c"
	d = Check(sql, ddlCatalog())
	if len(d) != 1 || fmt.Sprint(d[0].Start, d[0].End) != "{0 28} {0 31}" {
		t.Errorf("view body, one line: %v", d)
	}
}

func TestDDLUnknownState(t *testing.T) {
	// views/objects not loaded: no kind or object checks
	cat := NewCatalog(sqlsplit.PostgreSQL, "app").
		Table("app", "v_orders", []string{"id"}).Complete(true).Build()
	for _, sql := range []string{"DROP TABLE v_orders", "DROP INDEX nope", "ALTER TABLE v_orders DROP CONSTRAINT nope"} {
		if got := messages(t, cat, sql); len(got) != 0 {
			t.Errorf("%s: %q", sql, got)
		}
	}
	// Oracle: system-named constraints may be NOT NULL checks (not loaded)
	ora := NewCatalog(sqlsplit.Oracle, "CADE").Table("CADE", "T", []string{"A"}).ObjectsKnown(true).Complete(true).Build()
	if got := messages(t, ora, "ALTER TABLE t DROP CONSTRAINT SYS_C0012345"); len(got) != 0 {
		t.Errorf("SYS_C: %q", got)
	}
	if got := messages(t, ora, "ALTER TABLE t DROP CONSTRAINT fk_nope"); len(got) != 1 {
		t.Errorf("fk_nope: %q", got)
	}
}

func TestDDLPartialObjects(t *testing.T) {
	cat := NewCatalog(sqlsplit.Oracle, "CADE").Table("CADE", "T", []string{"A"}).
		Object("index", "CADE", "T_IX", "T").
		ObjectsPartial("trigger", "CADE").ObjectsKnown(true).Complete(true).Build()
	if got := messages(t, cat, "ALTER TRIGGER t_trg DISABLE"); len(got) != 0 {
		t.Errorf("partial triggers: %q", got)
	}
	if got := messages(t, cat, "DROP INDEX t_ixx"); len(got) != 1 {
		t.Errorf("indexes: %q", got)
	}
	// logged in as another user (current schema not cached): unqualified
	// names can't be told
	ro := NewCatalog(sqlsplit.Oracle, "CADE_RO").Table("CADE", "T", []string{"A"}).
		Object("index", "CADE", "T_IX", "T").ObjectsKnown(true).Build()
	if got := messages(t, ro, "DROP INDEX t_ixx"); len(got) != 0 {
		t.Errorf("other user: %q", got)
	}
	if got := messages(t, ro, "DROP INDEX cade.t_ixx"); len(got) != 1 {
		t.Errorf("qualified: %q", got)
	}
}

func TestDDLDynamic(t *testing.T) {
	cat2 := NewCatalog(sqlsplit.Oracle, "APP").Table("APP", "ORDERS", []string{"ID", "TOTAL"}).Complete(true).Build()
	sql := "BEGIN EXECUTE IMMEDIATE q'[CREATE TABLE hist (id NUMBER)]'; END;\n/\n" +
		"COMMENT ON TABLE hist IS 'x';\n" +
		"BEGIN EXECUTE IMMEDIATE 'ALTER TABLE orders ADD note VARCHAR2(10)'; EXCEPTION WHEN OTHERS THEN NULL; END;\n/\n" +
		"COMMENT ON COLUMN orders.note IS 'it''s new';\n" +
		"COMMENT ON COLUMN orders.nope IS 'x';\n"
	got := messages(t, cat2, sql)
	if len(got) != 1 || got[0] != "orders has no column nope (did you mean note?)" {
		t.Errorf("got %q", got)
	}
	if s, ok := stringLiteral("'a''b' rest"); !ok || s != "a'b" {
		t.Errorf("literal %q", s)
	}
	if s, ok := stringLiteral("q'{x]y}' rest"); !ok || s != "x]y" {
		t.Errorf("q literal %q", s)
	}
}
