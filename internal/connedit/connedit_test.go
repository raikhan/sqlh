package connedit

import (
	"strings"
	"testing"
)

const sample = `lowercaseKeywords: false
# insert Oracle's upper-case names as lower case
lowercaseIdentifiers: true
connections:
  # CADE
  - alias: cadeprod
    environment: prod
    driver: oracle
    host: oracleops02
    port: 1521
    dbName: CADEPROD
    user: sdsai
    passwd: "s3cret"
    schemas: [CADE]    # metadata allow-list
  - alias: cade2
    environment: dev
    driver: oracle
    host: oracle.dev
    port: 1521

  - alias: pg_local
    driver: postgresql
    dataSourceName: host=localhost port=5432 user=admin dbname=sdsai
  - alias: pg_sds_ai
    driver: postgresql
    host: srepg
    params: { sslmode: disable }
    sshConfig:
      host: bastion
      port: 22

  - alias: sqlite_scratch
    driver: sqlite3
    dataSourceName: file:/tmp/scratch.db

results:
  previewLimit: 100
`

func parse(t *testing.T, text string) *File {
	t.Helper()
	f, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGet(t *testing.T) {
	f := parse(t, sample)
	got, err := f.Get("cadeprod")
	if err != nil {
		t.Fatal(err)
	}
	if got["passwd"] != "s3cret" || got["schemas"] != "CADE" || got["port"] != "1521" || got["environment"] != "prod" {
		t.Errorf("fields %v", got)
	}
	got, _ = f.Get("pg_sds_ai")
	if got["params"] != "sslmode=disable" {
		t.Errorf("params %v", got)
	}
	if strings.Join(f.Aliases(), ",") != "cadeprod,cade2,pg_local,pg_sds_ai,sqlite_scratch" {
		t.Errorf("aliases %v", f.Aliases())
	}
}

func TestEditKeepsTheRest(t *testing.T) {
	f := parse(t, sample)
	fields, _ := f.Get("cade2")
	fields["host"] = "oracle2.dev"
	fields["group"] = "CADE"
	fields["port"] = ""
	if err := f.Set("cade2", fields, ""); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sample, `  - alias: cade2
    environment: dev
    driver: oracle
    host: oracle.dev
    port: 1521
`, `  - alias: cade2
    environment: dev
    driver: oracle
    host: oracle2.dev
    group: CADE
`, 1)
	if got := string(f.Bytes()); got != want {
		t.Errorf("got:\n%s", got)
	}
}

func TestEditKeepsCommentsAndQuotes(t *testing.T) {
	f := parse(t, sample)
	fields, _ := f.Get("cadeprod")
	fields["passwd"] = "n3w"
	if err := f.Set("cadeprod", fields, ""); err != nil {
		t.Fatal(err)
	}
	got := string(f.Bytes())
	for _, want := range []string{"  # CADE\n  - alias: cadeprod\n", `    passwd: "n3w"`, "    schemas: [CADE] # metadata allow-list\n  - alias: cade2"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	// nested maps it doesn't edit survive
	fields, _ = f.Get("pg_sds_ai")
	fields["host"] = "srepg2"
	if err := f.Set("pg_sds_ai", fields, ""); err != nil {
		t.Fatal(err)
	}
	got = string(f.Bytes())
	if !strings.Contains(got, "    host: srepg2\n    params: {sslmode: disable}\n    sshConfig:\n      host: bastion\n      port: 22\n\n  - alias: sqlite_scratch") {
		t.Errorf("pg_sds_ai:\n%s", got)
	}
}

func TestAddDuplicateDelete(t *testing.T) {
	f := parse(t, sample)
	fields, _ := f.Get("pg_local")
	fields["alias"] = "pg_local_copy"
	if err := f.Set("", fields, "pg_local"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.Aliases(), ",") != "" { // aliases come from the parse; re-parse
	}
	g := parse(t, string(f.Bytes()))
	if strings.Join(g.Aliases(), ",") != "cadeprod,cade2,pg_local,pg_local_copy,pg_sds_ai,sqlite_scratch" {
		t.Errorf("aliases %v\n%s", g.Aliases(), f.Bytes())
	}
	if err := g.Set("", Fields{"alias": "pg_local"}, ""); err == nil {
		t.Error("a second pg_local")
	}
	if err := g.Set("", Fields{"alias": "new", "driver": "sqlite3", "dataSourceName": "file:/x.db"}, ""); err != nil {
		t.Fatal(err)
	}
	h := parse(t, string(g.Bytes()))
	if a := h.Aliases(); a[len(a)-1] != "new" || !strings.Contains(string(g.Bytes()), "    dataSourceName: file:/x.db\n\nresults:") {
		t.Errorf("new at the end:\n%s", g.Bytes())
	}
	if err := h.Delete("cade2"); err != nil {
		t.Fatal(err)
	}
	got := string(h.Bytes())
	if strings.Contains(got, "cade2") || !strings.Contains(got, "    schemas: [CADE]    # metadata allow-list\n\n  - alias: pg_local\n") {
		t.Errorf("deleted:\n%s", got)
	}
}

func TestNoConnectionsYet(t *testing.T) {
	f := parse(t, "results:\n  previewLimit: 10\n")
	if err := f.Set("", Fields{"alias": "a", "driver": "sqlite3", "dataSourceName": "a.db"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Bytes()); got != "results:\n  previewLimit: 10\nconnections:\n  - alias: a\n    driver: sqlite3\n    dataSourceName: a.db\n" {
		t.Errorf("got %q", got)
	}
	f = parse(t, "connections:\nresults: {}\n")
	if err := f.Set("", Fields{"alias": "a", "driver": "sqlite3"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Bytes()); got != "connections:\n  - alias: a\n    driver: sqlite3\nresults: {}\n" {
		t.Errorf("got %q", got)
	}
}
