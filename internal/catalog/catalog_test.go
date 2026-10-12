package catalog

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
)

type fake struct {
	def    string
	tables map[string][]string
	cols   map[string][]*database.ColumnDesc
}

func (f fake) DefaultSchema() string { return f.def }
func (f fake) Schemas() []string {
	var s []string
	for k := range f.tables {
		s = append(s, k)
	}
	return s
}
func (f fake) SchemaTables() map[string][]string { return f.tables }
func (f fake) Columns(schema, table string) ([]*database.ColumnDesc, bool) {
	for k, c := range f.cols { // case-insensitive, like DBCache.ColumnDatabase
		if strings.EqualFold(k, schema+"."+table) {
			return c, true
		}
	}
	return nil, false
}

func col(n, t string) *database.ColumnDesc {
	return &database.ColumnDesc{ColumnBase: database.ColumnBase{Name: n}, Type: t}
}

func TestOracle(t *testing.T) {
	src := fake{def: "CADE",
		tables: map[string][]string{"CADE": {"CAPRODUCT", "Mixed Case"}, "CADE500": {"CAPRODUCT"}},
		cols:   map[string][]*database.ColumnDesc{"CADE.CAPRODUCT": {col("PROD_ID", "NUMBER"), col("Weird col", "VARCHAR2(10)")}, "CADE500.CAPRODUCT": {col("PROD_ID", "NUMBER")}},
	}
	opt := Options{Driver: dialect.DatabaseDriverOracle, Lower: true}
	var got []string
	for _, ti := range Tables(src, "", opt) {
		got = append(got, ti.Insert)
	}
	if want := []string{"caproduct", `"Mixed Case"`, "cade500.caproduct"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tables %q want %q", got, want)
	}
	for _, name := range []string{"caproduct", "CAPRODUCT", "cade.caproduct"} {
		cols, err := Columns(src, name, opt)
		if err != nil || len(cols) != 2 || cols[0].Insert != "prod_id" || cols[1].Insert != `"Weird col"` {
			t.Errorf("Columns(%q) = %+v, %v", name, cols, err)
		}
	}
	if _, err := Columns(src, "cade500.caproduct", opt); err != nil {
		t.Error(err)
	}
	if _, err := Columns(src, "nope", opt); err == nil {
		t.Error("expected unknown table error")
	}
	if s := Schemas(src, opt); len(s) != 2 || s[0].Name != "CADE" || !s[0].Default {
		t.Errorf("schemas %+v", s)
	}
}

func TestPostgresHidesSystemSchemas(t *testing.T) {
	src := fake{def: "cadetst", tables: map[string][]string{"cadetst": {"batch"}, "public": {"t"}, "pg_catalog": {"pg_class"}, "information_schema": {"tables"}}}
	opt := Options{Driver: dialect.DatabaseDriverPostgreSQL}
	var got []string
	for _, ti := range Tables(src, "", opt) {
		got = append(got, ti.Insert)
	}
	if want := []string{"batch", "public.t"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tables %q want %q", got, want)
	}
	opt.All = true
	if n := len(Tables(src, "", opt)); n != 4 {
		t.Errorf("--all: %d tables", n)
	}
	if n := len(Tables(src, "pg_catalog", Options{Driver: dialect.DatabaseDriverPostgreSQL})); n != 1 {
		t.Errorf("explicit system schema: %d", n)
	}
}

func TestSplitName(t *testing.T) {
	for in, want := range map[string][]string{
		"a":               {"a"},
		"s.t":             {"s", "t"},
		`"My Schema"."t"`: {"My Schema", "t"},
		`s."a.b"`:         {"s", "a.b"},
		`"say ""hi"""`:    {`say "hi"`},
	} {
		if got := splitName(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitName(%q) = %q", in, got)
		}
	}
}
