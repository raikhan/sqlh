package header

import (
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name, text   string
		conn, db     string
		line         int
		roundTripped string
	}{
		{"none", "select 1", "", "", -1, ""},
		{"empty file", "", "", "", -1, ""},
		{"first line", "-- sqlh: conn=pg_local\nselect 1", "pg_local", "", 0, "-- sqlh: conn=pg_local"},
		{"with db", "-- sqlh: conn=pg_local db=reports\n\nselect 1", "pg_local", "reports", 0, "-- sqlh: conn=pg_local db=reports"},
		{"among comments", "-- monthly report\n--   owner: x\n\n-- sqlh: db=r conn=dev\nselect 1", "dev", "r", 3, "-- sqlh: conn=dev db=r"},
		{"after block comment", "/* report\n   for x */\n-- sqlh: conn=dev\nselect 1", "dev", "", 2, "-- sqlh: conn=dev"},
		{"one-line block comment", "/* x */\n-- sqlh: conn=dev\nselect 1", "dev", "", 1, "-- sqlh: conn=dev"},
		{"after SQL: not a header", "select 1;\n-- sqlh: conn=dev", "", "", -1, ""},
		{"SQL after block comment", "/* x */ select 1;\n-- sqlh: conn=dev", "", "", -1, ""},
		{"CRLF", "-- sqlh: conn=dev\r\nselect 1\r\n", "dev", "", 0, "-- sqlh: conn=dev"},
		{"spacing and case", "--SQLH:conn=dev   db=x", "dev", "x", 0, "-- sqlh: conn=dev db=x"},
		{"quoted value, other keys", `-- sqlh: conn=dev db="my db" note=keep flag`, "dev", "my db", 0, `-- sqlh: conn=dev db="my db" note=keep flag`},
		{"indented", "  -- sqlh: conn=dev\nselect 1", "dev", "", 0, "-- sqlh: conn=dev"},
	}
	for _, c := range cases {
		h := Parse(c.text)
		if h.Conn != c.conn || h.DB != c.db || h.Line != c.line {
			t.Errorf("%s: got conn=%q db=%q line=%d", c.name, h.Conn, h.DB, h.Line)
		}
		if c.line >= 0 && h.String() != c.roundTripped {
			t.Errorf("%s: String() = %q, want %q", c.name, h.String(), c.roundTripped)
		}
	}
}

func TestStamp(t *testing.T) {
	cases := []struct {
		name, text, conn, db, want string
	}{
		{"insert before SQL", "select 1;\n", "pg_local", "", "-- sqlh: conn=pg_local\n\nselect 1;\n"},
		{"insert before comments", "-- report\nselect 1;\n", "pg_local", "", "-- sqlh: conn=pg_local\n-- report\nselect 1;\n"},
		{"insert with db", "\nselect 1", "pg_local", "rep", "-- sqlh: conn=pg_local db=rep\n\nselect 1"},
		{"insert into empty file", "", "dev", "", "-- sqlh: conn=dev\n"},
		{"insert CRLF", "select 1\r\n", "dev", "", "-- sqlh: conn=dev\r\n\r\nselect 1\r\n"},
		{"replace conn", "-- sqlh: conn=dev\n\nselect 1", "pg_local", "", "-- sqlh: conn=pg_local\n\nselect 1"},
		{"replace in place among comments", "-- report\n-- sqlh: conn=dev db=x\nselect 1", "pg_local", "", "-- report\n-- sqlh: conn=pg_local\nselect 1"},
		{"add db, keep other keys", "-- sqlh: note=keep conn=dev\nselect 1", "dev", "other", "-- sqlh: conn=dev db=other note=keep\nselect 1"},
		{"replace CRLF keeps line break", "-- sqlh: conn=dev\r\nselect 1", "x", "", "-- sqlh: conn=x\r\nselect 1"},
		{"replace keeps indentation", "  -- sqlh: conn=dev\nselect 1", "x", "", "  -- sqlh: conn=x\nselect 1"},
		{"non-ASCII before", "-- sqlh: conn=dév\nselect 'é'", "x", "", "-- sqlh: conn=x\nselect 'é'"},
		{"quoted db", "select 1", "dev", "my db", "-- sqlh: conn=dev db=\"my db\"\n\nselect 1"},
	}
	for _, c := range cases {
		e, ok := Stamp(c.text, c.conn, c.db)
		if !ok {
			t.Errorf("%s: no edit", c.name)
			continue
		}
		if got := Apply(c.text, e); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		// stamping again is a no-op
		if _, again := Stamp(Apply(c.text, e), c.conn, c.db); again {
			t.Errorf("%s: second stamp not a no-op", c.name)
		}
	}
	if _, ok := Stamp("-- sqlh: conn=dev db=x\nselect 1", "dev", "x"); ok {
		t.Error("up-to-date header edited")
	}
}

func TestEditUTF16(t *testing.T) {
	// LSP ranges count UTF-16 units: an astral character is 2
	e, _ := Stamp("-- sqlh: conn=😀\nselect 1", "x", "")
	if e.Line != 0 || e.OldLen != len("-- sqlh: conn=")+2 {
		t.Errorf("edit %+v", e)
	}
}
