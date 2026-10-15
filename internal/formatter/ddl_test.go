package formatter

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/lsp"
)

func TestDDLLayout(t *testing.T) {
	params := lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 4, InsertSpaces: true}}
	cases := []struct{ in, want string }{
		{
			"create table orders (id bigint generated always as identity primary key, customer_id bigint not null references customer (id), status varchar(20) not null default 'new' check (status in ('new', 'paid')), total numeric(12,2) not null default 0);",
			`CREATE TABLE orders (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id BIGINT NOT NULL REFERENCES customer (id),
    status VARCHAR(20) NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'paid')),
    total NUMERIC(12, 2) NOT NULL DEFAULT 0
);`,
		},
		{
			"create table if not exists t(a int, b text, constraint t_pk primary key (a)) inherits (base);",
			`CREATE TABLE IF NOT EXISTS t(
    a INT,
    b TEXT,
    CONSTRAINT t_pk PRIMARY KEY (a)
) INHERITS (base);`,
		},
		{"alter table orders add column shipped_at timestamptz;", "ALTER TABLE orders ADD COLUMN shipped_at TIMESTAMPTZ;"},
		{
			"alter table orders add column a int, drop column b, alter column c type text;",
			`ALTER TABLE orders
    ADD COLUMN a INT,
    DROP COLUMN b,
    ALTER COLUMN c TYPE TEXT;`,
		},
		{"alter table orders add (a number, b varchar2(10 char));", "ALTER TABLE orders ADD (a NUMBER, b VARCHAR2(10 CHAR));"},
		{"create index orders_customer_id_idx on orders (customer_id);", "CREATE INDEX orders_customer_id_idx ON orders (customer_id);"},
		{"create unique index i on t (lower(name)) where active;", "CREATE UNIQUE INDEX i ON t (LOWER(name)) WHERE active;"},
		{"drop view if exists v cascade;", "DROP VIEW IF EXISTS v CASCADE;"},
		{"comment on column t.a is 'the a';", "COMMENT ON COLUMN t.a IS 'the a';"},
		{
			"create or replace view v as select o.id, c.name from orders o join customer c on c.id = o.customer_id;",
			`CREATE OR REPLACE VIEW v AS
SELECT
    o.id,
    c.name
FROM
    orders o
JOIN customer c
    ON c.id = o.customer_id;`,
		},
		{
			"create table t as select a, b from u;",
			`CREATE TABLE t AS
SELECT
    a,
    b
FROM
    u;`,
		},
	}
	for _, c := range cases {
		got, err := FormatVerified(c.in, params, &config.Config{})
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if strings.TrimSpace(got) != c.want {
			t.Errorf("%s\n got:\n%s\nwant:\n%s", c.in, got, c.want)
		}
	}
}

func TestDDLLayoutKeepsIdentifiers(t *testing.T) {
	params := lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 2, InsertSpaces: true}}
	// columns named like keywords stay as written; lower-case keywords
	got, err := FormatVerified("CREATE TABLE T (Type TEXT, Data INT, PRIMARY KEY (Type));", params, &config.Config{LowercaseKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "create table T (\n  Type text,\n  Data int,\n  primary key (Type)\n);"
	if strings.TrimSpace(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDDLLayoutComments(t *testing.T) {
	params := lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 4, InsertSpaces: true}}
	// comments inside a DDL statement: left to the regular formatter, still verified
	in := "create table t (\n  id int, -- the key\n  name text\n);"
	if _, err := FormatVerified(in, params, &config.Config{}); err != nil {
		if _, ok := err.(*SkipError); !ok {
			t.Fatal(err)
		}
	}
	// a document of DDL and queries: each keeps its own layout, blank lines kept
	in = "drop table a;\n\nselect x from y where z = 1;\n\ncreate index i on y (z);\n"
	got, err := FormatVerified(in, params, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	want := "DROP TABLE a;\n\nSELECT\n    x\nFROM\n    y\nWHERE\n    z = 1;\n\nCREATE INDEX i ON y (z);\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
