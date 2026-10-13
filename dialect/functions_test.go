package dialect

import (
	"reflect"
	"testing"
)

func TestSplitParams(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"value, ...", []string{"value", "..."}},
		{"string text, pattern text [, flags text]", []string{"string text", "pattern text", "[flags text]"}},
		{"value [, offset int [, default]]", []string{"value", "[offset int]", "[default]"}},
		{"source_char, pattern [, position [, occurrence]]", []string{"source_char", "pattern", "[position]", "[occurrence]"}},
		{"[LEADING | TRAILING | BOTH] [characters] FROM string", []string{"[LEADING | TRAILING | BOTH] [characters] FROM string"}},
		{"a text[], b int", []string{"a text[]", "b int"}},
		{"VARIADIC \"any\"", []string{"VARIADIC \"any\""}},
		{"p numeric(10, 2), q text", []string{"p numeric(10, 2)", "q text"}},
		{"sys_context('a, b', x)", []string{"sys_context('a, b', x)"}},
		{"p_id NUMBER, [p_flag VARCHAR2]", []string{"p_id NUMBER", "[p_flag VARCHAR2]"}},
	}
	for _, tt := range tests {
		if got := SplitParams(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuiltinTables(t *testing.T) {
	for _, d := range []DatabaseDriver{DatabaseDriverPostgreSQL, DatabaseDriverOracle, DatabaseDriverSQLite3} {
		fns := BuiltinFunctions(d)
		if len(fns) < 50 {
			t.Errorf("%s: only %d built-ins", d, len(fns))
		}
		for _, f := range fns {
			if f.Name == "" || f.Doc == "" || f.Result == "" {
				t.Errorf("%s: incomplete entry %+v", d, f)
			}
		}
	}
	var pc *Function
	for _, f := range BuiltinFunctions(DatabaseDriverPostgreSQL) {
		if f.Name == "percentile_cont" {
			pc = f
		}
	}
	if pc == nil || pc.Args != "fraction" || pc.Suffix != "WITHIN GROUP (ORDER BY expression)" || pc.Kind != FunctionAggregate {
		t.Errorf("percentile_cont: %+v", pc)
	}
}
