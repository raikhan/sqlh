package jsoncomplete

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// at returns text with the cursor marker removed, and the cursor position.
func at(s string) (string, int, int) {
	i := strings.Index(s, "|")
	before := s[:i]
	line := strings.Count(before, "\n")
	col := len([]rune(before[strings.LastIndex(before, "\n")+1:]))
	return before + s[i+1:], line, col
}

func TestDetect(t *testing.T) {
	pg, ora := sqlsplit.PostgreSQL, sqlsplit.Oracle
	cases := []struct {
		name string
		d    sqlsplit.Dialect
		in   string
		want *Context // nil: no context
	}{
		{"arrow", pg, "select x from t sds where sds.raw_data->>'lan|", &Context{Style: StyleArrow, Qualifier: "sds", Column: "raw_data", Prefix: "lan", Replace: Position{0, 42}}},
		{"arrow empty, auto-paired quote", pg, "where raw_data->'|'", &Context{Style: StyleArrow, Column: "raw_data", Replace: Position{0, 17}}},
		{"nested", pg, "where d.raw_data->'properties' -> 'phys'->>'|", &Context{Style: StyleArrow, Qualifier: "d", Column: "raw_data", Path: []string{"properties", "phys"}, Replace: Position{0, 44}}},
		{"array index", pg, "where raw_data->'ingredients'->0->>'na|", &Context{Style: StyleArrow, Column: "raw_data", Path: []string{"ingredients", "0"}, Prefix: "na", Replace: Position{0, 36}}},
		{"multiline", pg, "select\n  s.raw_data\n    ->'a'\n    ->>'b|", &Context{Style: StyleArrow, Qualifier: "s", Column: "raw_data", Path: []string{"a"}, Prefix: "b", Replace: Position{3, 8}}},
		{"key exists", pg, "where raw_data ? 'sy|", &Context{Style: StyleArrow, Column: "raw_data", Prefix: "sy", Replace: Position{0, 18}}},
		{"schema-qualified", pg, "where cadetst.sds_data_raw.raw_data->>'|", &Context{Style: StyleArrow, Qualifier: "cadetst.sds_data_raw", Column: "raw_data", Replace: Position{0, 39}}},
		{"quoted column", pg, `where "Raw Data"->>'|`, &Context{Style: StyleArrow, Column: "Raw Data", Replace: Position{0, 20}}},
		{"array path", pg, "where raw_data #>> '{properties, ph|", &Context{Style: StyleArray, Column: "raw_data", Path: []string{"properties"}, Prefix: "ph", Replace: Position{0, 33}}},
		{"array path first", pg, "where raw_data #> '{|", &Context{Style: StyleArray, Column: "raw_data", Replace: Position{0, 20}}},
		{"pg jsonpath", pg, "select jsonb_path_query(raw_data, '$.properties.|", &Context{Style: StyleJSONPath, Column: "raw_data", Path: []string{"properties"}, Replace: Position{0, 47}}},
		{"oracle json_value", ora, "select json_value(e.content_json, '$.CAS|", &Context{Style: StyleJSONPath, Qualifier: "e", Column: "content_json", Prefix: "CAS", Replace: Position{0, 36}}},
		{"oracle quoted key", ora, `select json_value(content_json, '$."CAS R|`, &Context{Style: StyleJSONPath, Column: "content_json", Prefix: "CAS R", Replace: Position{0, 34}}},
		{"oracle after quoted + array", ora, `select json_query(c, '$."a b".items[*].|`, &Context{Style: StyleJSONPath, Column: "c", Path: []string{"a b", "items"}, Replace: Position{0, 38}}},
		{"oracle unquoted key with space", ora, `select json_value(c, '$.CAS R|`, &Context{Style: StyleJSONPath, Column: "c", Prefix: "CAS R", Replace: Position{0, 23}}},
		{"oracle dollar only", ora, "select json_value(c, '$|", &Context{Style: StyleJSONPath, Column: "c", Replace: Position{0, 23}}},
		{"oracle # in name", ora, "select json_value(t.col#1, '$.|", &Context{Style: StyleJSONPath, Qualifier: "t", Column: "col#1", Replace: Position{0, 29}}},
		{"escaped quote in key", pg, "where raw_data->>'it''s|", &Context{Style: StyleArrow, Column: "raw_data", Prefix: "it's", Replace: Position{0, 18}}},

		{"plain string", pg, "where name = 'abc|", nil},
		{"closed string", pg, "where raw_data->>'abc'|", nil},
		{"string in comment", pg, "-- raw_data->>'x|", nil},
		{"not after op", pg, "select 'a|", nil},
		{"oracle arrow not json", ora, "where raw_data->>'x|", nil},
		{"non json function", pg, "select upper(c, '$.|", nil},
		{"jsonpath without $", ora, "select json_value(c, 'x|", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, line, col := at(c.in)
			got, ok := Detect(text, line, col, c.d)
			if c.want == nil {
				if ok {
					t.Fatalf("unexpected context %+v", got)
				}
				return
			}
			if !ok {
				t.Fatal("no context")
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestParseGuide(t *testing.T) {
	g := `[{"o:path":"$","type":"object"},{"o:path":"$.FLAG","type":"string"},{"o:path":"$.\"CAS RN\"","type":"string"},
	{"o:path":"$.contexts","type":"array"},{"o:path":"$.contexts[*]","type":"string"},
	{"o:path":"$.items","type":"array"},{"o:path":"$.items[*].name","type":"string"},{"o:path":"$.items[*].name","type":"number"},
	{"o:path":"$.b","type":"object"},{"o:path":"$.b.c","type":"string"}]`
	all, err := parseGuide(g)
	if err != nil {
		t.Fatal(err)
	}
	names := func(ks []Key) (out []string) {
		for _, k := range ks {
			out = append(out, k.Name+":"+strings.Join(k.Types, "|"))
		}
		return
	}
	if got := names(Children(all, nil)); !reflect.DeepEqual(got, []string{"CAS RN:string", "FLAG:string", "b:object", "contexts:array", "items:array"}) {
		t.Errorf("top level: %q", got)
	}
	if got := names(Children(all, []string{"items"})); !reflect.DeepEqual(got, []string{"name:string|number"}) {
		t.Errorf("array elements: %q", got)
	}
	if got := names(Children(all, []string{"b"})); !reflect.DeepEqual(got, []string{"c:string"}) {
		t.Errorf("nested: %q", got)
	}
}

func TestInsertText(t *testing.T) {
	cases := []struct {
		s         Style
		key, want string
	}{
		{StyleJSONPath, "CASRN", ".CASRN"},
		{StyleJSONPath, "CAS RN", `."CAS RN"`},
		{StyleJSONPath, `a"b`, `."a\"b"`},
		{StyleArray, "phys", "phys"},
		{StyleArray, "a,b", `"a,b"`},
		{StyleArrow, "it's", "it's"},
	}
	for _, c := range cases {
		if got := InsertText(c.s, c.key); got != c.want {
			t.Errorf("InsertText(%v, %q) = %q, want %q", c.s, c.key, got, c.want)
		}
	}
	if pgTextArray([]string{"a", `b"c`}) != `{"a","b\"c"}` {
		t.Error(pgTextArray([]string{"a", `b"c`}))
	}
}

func TestCacheTimeoutThenReady(t *testing.T) {
	c := &Cache{TTL: time.Minute}
	release := make(chan struct{})
	calls := 0
	fetch := func(context.Context) ([]Key, error) { calls++; <-release; return []Key{{Name: "a"}}, nil }
	if _, ready, _ := c.Get("x", 20*time.Millisecond, fetch); ready {
		t.Fatal("should not be ready")
	}
	close(release)
	keys, ready, err := c.Get("x", time.Second, fetch) // joins the in-flight fetch
	if !ready || err != nil || len(keys) != 1 || calls != 1 {
		t.Fatalf("ready=%v err=%v keys=%v calls=%d", ready, err, keys, calls)
	}
	c.Clear()
	c.Get("x", time.Second, func(context.Context) ([]Key, error) { calls++; return nil, nil })
	if calls != 2 {
		t.Errorf("Clear did not force a refetch")
	}
}

func TestArrayHint(t *testing.T) {
	jc := &Context{Style: StyleArrow, Qualifier: "sds", Column: "raw_data", Path: []string{"ghsItems"}}
	elemOnly := Key{Name: "code", Count: 15, InArray: 15}
	doc, ok := ArrayHint(jc, elemOnly)
	if !ok {
		t.Fatal("expected a hint")
	}
	for _, want := range []string{
		"`ghsItems` is an **array**",
		"`sds.raw_data->'ghsItems'->>'code'` is always NULL",
		"`sds.raw_data->'ghsItems'->0->>'code'`",
		"`jsonb_path_query_array(sds.raw_data, '$.ghsItems[*].code')`",
		"jsonb_array_elements(sds.raw_data->'ghsItems')",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("hint lacks %q:\n%s", want, doc)
		}
	}
	// nested path with an index and keys needing quotes
	jc2 := &Context{Style: StyleArrow, Column: "doc", Path: []string{"a b", "0", "items"}}
	doc, _ = ArrayHint(jc2, Key{Name: "it's", Count: 1, InArray: 1})
	for _, want := range []string{`doc->'a b'->0->'items'->0->>'it''s'`, `'$."a b"[0].items[*]."it''s"'`} {
		if !strings.Contains(doc, want) {
			t.Errorf("hint lacks %q:\n%s", want, doc)
		}
	}
	if _, ok := ArrayHint(jc, Key{Name: "x", Count: 3, InArray: 1}); ok {
		t.Error("key also in plain objects: no hint")
	}
	if _, ok := ArrayHint(&Context{Style: StyleJSONPath, Column: "c"}, elemOnly); ok {
		t.Error("jsonpath steps into arrays: no hint")
	}
	ks := []Key{{Name: "a", Count: 9, InArray: 9}, {Name: "b", Count: 1}}
	sortKeys(ks)
	if ks[0].Name != "b" {
		t.Errorf("usable keys must sort first: %v", ks)
	}
}
