package formatter

import (
	"errors"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/lsp"
)

func verified(t *testing.T, src string) (string, error) {
	t.Helper()
	cfg := &config.Config{LowercaseKeywords: true}
	p := lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 2, InsertSpaces: true}}
	return FormatVerified(src, p, cfg)
}

func TestFormatVerifiedFormats(t *testing.T) {
	cases := map[string]string{
		"plain join":         "select p.prod_id, co.co_name from cade.caproduct p join cade.cacompany co on co.co_id = p.co_id where p.status = 'A';\n",
		"escaped quote":      "select * from t where name = '4,4''-mdi';\n",
		"function calls":     "select nvl(a, 0), decode(b, 1, 'one', 'other') from t;\n",
		"cte + window":       "with x as (select id, row_number() over (partition by g order by d desc) rn from t) select count(*) filter (where rn = 1) from x;\n",
		"upsert":             "insert into t (id, n) values ($1, 'a') on conflict (id) do update set n = excluded.n returning id;\n",
		"e-string":           "select e'line\\nbreak' from t;\n",
		"sections":           "-- 1. first\nselect 1 from t;\n\n-- 2. second\nselect 2 from t; -- trailing note\n\n\n/* block\n   comment */\nselect 3 from t;\n",
		"transaction":        "begin;\nupdate t set a = 1 where id = 2;\ncommit;\n",
		"already formatted":  "select\n  a\nfrom\n  t;\n",
		"pg cast":            "select a::int from t;\n", // becomes "a ::int": odd but valid
		"typed literal":      "select p.prod_id from cade.caproduct p where p.edited_date > DATE '2026-04-01' and p.status = 'A';\n",
		"midedit member":     "select b. from batch b;\n",
		"midedit schema":     "select * from cadetst.\n",
		"oracle outer join":  "select a.x, b.y from a, b where a.id = b.id(+);\n",
		"dblink + bind":      "select * from caproduct@remote_link where prod_id = :id;\n",
		"json operators":     "select sds.raw_data->'language' as language, d->'a'->>'b' from t sds where d #>> '{a,b}' is not null;\n",
		"pg operators":       "select a from t where tags @> '{\"x\":1}' and j ? 'k' and j ?| array['a'] and a || b = 'ab' and n !~* '^x';\n",
		"sqlite placeholder": "insert into t (a, b) values (?, ?);\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := verified(t, src)
			if err != nil {
				t.Fatalf("unexpected skip: %v", err)
			}
			if _, ok := sameTokens(src, out); !ok {
				t.Fatalf("content changed:\n%s", out)
			}
			again, err := verified(t, out)
			if err != nil || again != out {
				t.Fatalf("not idempotent (err=%v):\n%s\n---\n%s", err, out, again)
			}
		})
	}
}

func TestFormatVerifiedKeepsLayoutBetweenStatements(t *testing.T) {
	src := "-- 1. first\nselect 1 from t;\n\n-- 2. second\nselect 2 from t; -- trailing note\n\n\nselect 3 from t;\n"
	out, err := verified(t, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"t;\n\n-- 2. second\n", "t; -- trailing note\n\n\nselect"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "-- 1. first\nselect") {
		t.Errorf("header changed:\n%s", out)
	}
}

func TestFormatVerifiedCTEKeepsComma(t *testing.T) {
	// regression: "as(" made the formatter parse a function call and drop "id,"'s comma
	out, err := verified(t, "with x as (select id, row_number() over (order by d) rn from t) select * from x;")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id,") {
		t.Errorf("comma lost:\n%s", out)
	}
}

func TestFormatVerifiedSkips(t *testing.T) {
	cases := map[string]string{
		"oracle slash":        "select 1 from dual\n/\n",
		"oracle q-quote":      "select q'[a;b]' from dual;",
		"plsql":               "begin\n  null;\nend;\n/\n",
		"pg dollar":           "do $$ begin perform 1; end $$;",
		"dropped comment":     "select a, -- trailing\n  b from t;",
		"quoted ident space":  `select "with space" from t;`,
		"trailing comma":      "select a, from t;",
		"open paren":          "select * from t where (a = 1;",
		"unterminated string": "select 'abc from t;\n",
		// the raw formatter corrupts these; verification must catch it
		"merges values":      "select -1, .5 from t;",                           // -> "-1.5"
		"glues limit offset": "select a from t limit 10 offset 5;",              // -> "10offset"
		"glues prior":        "select a from emp connect by prior empno = mgr;", // -> "priorempno"
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := verified(t, src)
			var se *SkipError
			if !errors.As(err, &se) {
				t.Fatalf("expected a skip, got err=%v out=%q", err, out)
			}
		})
	}
}

func TestSameTokens(t *testing.T) {
	ok := [][2]string{
		{"select a,b from t", "SELECT\n  a,\n  b\nFROM t"},
		{"a=1", "a = 1"},
		{"x -- c  \n", "x -- c\n"},
	}
	for _, c := range ok {
		if _, same := sameTokens(c[0], c[1]); !same {
			t.Errorf("expected same: %q vs %q", c[0], c[1])
		}
	}
	bad := [][2]string{
		{"select 'Abc'", "select 'abc'"},      // string case
		{`select "Col"`, `select "col"`},      // quoted identifier case
		{"select j->>'k'", "select j-> >'k'"}, // split operator
		{"select a, b", "select a b"},         // dropped comma
		{"select a -- note\n", "select a\n"},  // dropped comment
		{"select 'it''s'", "select 'it's'"},   // escaped quote
		{"select t.id", "select t . id"},      // split qualified name
	}
	for _, c := range bad {
		if _, same := sameTokens(c[0], c[1]); same {
			t.Errorf("expected different: %q vs %q", c[0], c[1])
		}
	}
}

func TestFullRangeAndOffsets(t *testing.T) {
	text := "ab\n😀c"
	r := FullRange(text)
	if r.End.Line != 1 || r.End.Character != 3 { // emoji = 2 UTF-16 units
		t.Errorf("FullRange = %+v", r)
	}
	if off, ok := offsetOf(text, lsp.Position{Line: 1, Character: 2}); !ok || text[off:] != "c" {
		t.Errorf("offsetOf = %d %v", off, ok)
	}
}

func TestFormatVerifiedRecoversFromPanic(t *testing.T) {
	orig := formatFn
	defer func() { formatFn = orig }()
	formatFn = func(string, lsp.DocumentFormattingParams, *config.Config) ([]lsp.TextEdit, error) {
		panic("boom")
	}
	_, err := verified(t, "select 1;")
	var se *SkipError
	if !errors.As(err, &se) || !strings.Contains(se.Reason, "boom") {
		t.Fatalf("expected a skip mentioning the panic, got %v", err)
	}
}

func TestFormatNilOperands(t *testing.T) {
	// regression: the parser leaves Comparison.Right / MemberIdentifier.Child
	// nil for typed literals and half-typed SQL; Format used to panic
	cfg := &config.Config{}
	p := lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 2, InsertSpaces: true}}
	for _, src := range []string{
		"select a from t where d > DATE '2026-04-01';",
		"select a from t where d > TIMESTAMP '2026-04-01 10:00:00';",
		"select a from t where x = ",
		"select b. from t b;",
		"select * from s.",
	} {
		if _, err := Format(src, p, cfg); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
}

func TestFormatOperatorSpacing(t *testing.T) {
	for src, want := range map[string]string{
		"select sds.raw_data -> 'language' from t;": "sds.raw_data->'language'",
		"select d->'a' ->> 'b' from t;":             "d->'a'->>'b'",
		"select a from t where d#>>'{a,b}' = 'x';":  "d #>> '{a,b}' = 'x'",
		"select a from t where tags@>'{}' ;":        "tags @> '{}'",
		"select a||b from t;":                       "a || b",
		"select a from t where j?'k';":              "j ? 'k'",
		"select a from t where n!~'x';":             "n !~ 'x'",
		"select * from caproduct@remote_link;":      "caproduct@remote_link",
		"select a :: int, b::text from t;":          "a::int,",
		"select d->>'k' || '!' from t;":             "d->>'k' || '!'",
	} {
		out, err := verified(t, src)
		if err != nil {
			t.Errorf("%q: %v", src, err)
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("%q: want %q in\n%s", src, want, out)
		}
	}
}
