package lint

import (
	"strings"
	"testing"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

func pgCatalog() *Catalog {
	return NewCatalog(sqlsplit.PostgreSQL, "cadetst").
		Schema("pg_catalog").Schema("information_schema").
		Table("cadetst", "field_definition", []string{"id", "name", "type"}).
		Table("cadetst", "field_mapping", []string{"id", "field_id", "target"}).
		Table("cadetst", "batch", []string{"id", "name", "created_by"}).
		Table("cadetst", "sds_data_raw", []string{"id", "raw_data"}).
		Table("public", "users", []string{"id", "email"}).
		Table("public", "mv_stats", nil). // materialized view: columns unknown
		Complete(true).Build()
}

func oracleCatalog(synonymsLoaded bool) *Catalog {
	b := NewCatalog(sqlsplit.Oracle, "CADE").
		Table("CADE", "CAPRODUCT", []string{"PROD_ID", "NAME"}).
		Table("CADE", "CAREGLISTENTRY", []string{"ID", "CONTENT_JSON"})
	if synonymsLoaded {
		b.Unqualified("USER_TABLES").Unqualified("ALL_OBJECTS").Complete(true)
	}
	return b.Build()
}

func sqliteCatalog() *Catalog {
	return NewCatalog(sqlsplit.SQLite, "").Table("", "t1", []string{"a", "b"}).Complete(true).Build()
}

func messages(t *testing.T, cat *Catalog, sql string) []string {
	t.Helper()
	var out []string
	for _, d := range Check(sql, cat) {
		out = append(out, d.Message)
	}
	return out
}

func TestFlags(t *testing.T) {
	cases := []struct {
		cat  *Catalog
		sql  string
		want string
	}{
		{pgCatalog(), "select fm.* from field_definition fd", "fm is not a table or alias in this statement (in scope: fd)"},
		{pgCatalog(), "select fd.nme from field_definition fd", "field_definition has no column nme (did you mean name?)"},
		{pgCatalog(), "select * from field_defintion", "unknown table field_defintion (did you mean field_definition?)"},
		{pgCatalog(), "select * from cadetst.batchh", "unknown table cadetst.batchh (did you mean batch?)"},
		{pgCatalog(), "insert into batch (id, nmae) values (1, 'x')", "batch has no column nmae (did you mean name?)"},
		{pgCatalog(), "update batch set nmae = 'x' where id = 1", "batch has no column nmae"},
		{pgCatalog(), "select b.id from batch b join field_mapping fm on fm.field_idd = b.id", "field_mapping has no column field_idd"},
		{pgCatalog(), "select x.id from (select id from batch) b", "x is not a table or alias"},
		{pgCatalog(), "select (select c.id from batch c where c.nope = 1) from field_definition fd", "batch has no column nope"},
		{pgCatalog(), "select b.id from batch b where b.id in (select q.field_id from field_mapping fm)", "q is not a table or alias"},
		{pgCatalog(), "select * from unnest((select array_agg(z.id) from batch b)) u", "z is not a table or alias"},
		{oracleCatalog(true), "select x.* from caproduct p", "x is not a table or alias"},
		{oracleCatalog(true), "select x.prod_id from caproduct p", "x is not a table or alias"},
		{oracleCatalog(true), "select p.nme from caproduct p", "caproduct has no column nme (did you mean name?)"},
		{oracleCatalog(true), "select * from caprodcut", "unknown table caprodcut (did you mean caproduct?)"},
		{sqliteCatalog(), "select x.a from t1", "x is not a table or alias"},
		{sqliteCatalog(), "select * from nope", "unknown table nope"},
	}
	for _, c := range cases {
		got := messages(t, c.cat, c.sql)
		if len(got) != 1 || !strings.HasPrefix(got[0], c.want) {
			t.Errorf("%s\n  got  %q\n  want %q", c.sql, got, c.want)
		}
	}
}

func TestQuiet(t *testing.T) {
	pg := []string{
		"select b.* from (select * from batch) b",
		"with x as (select id from batch) select x.id, x.anything from x",
		"with recursive r(n) as (select 1 union all select n + 1 from r) select r.n from r",
		"select u.email from users u",
		"select m.whatever from mv_stats m",
		"select t.x from generate_series(1, 3) as t(x)",
		"select u.n from unnest(array[1, 2]) with ordinality as u(v, n)",
		"select extract(year from b.created_by) from batch b",
		"insert into batch (id, name) values (1, 'a') on conflict (id) do update set name = excluded.name",
		"select b.ctid from batch b",
		"select * from pg_class",
		"select * from information_schema.tables",
		"create temp table tmp1 as select 1 as a; select * from tmp1",
		"select b.id from batch b where b.id in (select fm.field_id from field_mapping fm)",
		"select sds.raw_data->>'language', sds.id from sds_data_raw sds",
		"select a.id from batch a, lateral (select fm.id from field_mapping fm where fm.field_id = a.id) l",
		"select x.id from batch tablesample system (10) x",
		"select b.id from batch b where b.name is distinct from 'x'",
		"select count(*) filter (where b.id > 1) from batch b",
		"select b.id::text from batch b",
		"delete from batch b using field_mapping fm where fm.id = b.id",
		"update batch b set name = fm.target from field_mapping fm where fm.id = b.id",
		"select * from batch b join field_mapping fm using (id)",
		"select myschema.func(b.id) from batch b",
		"select * from unknown_schema.tbl",
		"do $$ begin perform x.y; end $$",
		"select $1, b.id from batch b",
		"select batch.id from cadetst.batch",
		"select pg_catalog.now()",
		"select b.id from batch as b order by b.name",
		"select b.id from batch b for update of b",
		"select * from (values (1, 2)) as v(a, b) where v.a = 1",
		"select fd.name, fm.target from field_definition fd left outer join field_mapping fm on fm.field_id = fd.id",
		"select * from batch where name in ('a.b', 'fm.*')",
		"select 1 -- fm.x in a comment\n",
		"explain select x.y from batch",
		"set search_path = cadetst",
		"select x.y from dblink('db', 'select 1') as x(y int)",
		"select n.id from nodes n where n.pid = {old_root}",
		"select t.id from {{ ref('batch') }} t",
		"select b.id from batch b where exists (select 1 from field_mapping fm where fm.field_id = b.id)",
		"select row_number() over (partition by b.name order by b.id) from batch b",
		"select case when b.id > 1 then fd.name end from batch b join field_definition fd on true",
		"select * from batch b, field_mapping fm, users u where fm.id = b.id and u.id = b.id",
		"select x.a, y.b from (select 1 a) x join (select 2 b) y on true",
		"select s.n from batch b inner join (select id, count(*) n from field_mapping group by id) s on s.id = b.id",
		"select t.* from cadetst.batch t",
		"insert into batch as b (id, name) values (1, 'x')",
		"insert into batch select * from batch",
		"update batch as b set name = 'x' where b.id = 1",
		"select \"B\".id from batch \"B\"",
		"select b.id from batch b group by rollup (b.id)",
		"select fd.name from field_definition fd where fd.name ~* 'x' and fd.name !~ 'y'",
		"select b.id from batch b where b.name similar to 'x%'",
		"select string_agg(fd.name, ', ' order by fd.id) from field_definition fd",
		"select fd.name from field_definition fd where fd.id = any(array[1, 2])",
		"select json_agg(fd) from field_definition fd",
		"select (fd).name from field_definition fd",
	}
	for _, sql := range pg {
		if got := messages(t, pgCatalog(), sql); len(got) != 0 {
			t.Errorf("pg %q: %q", sql, got)
		}
	}
	ora := []string{
		"select dbms_random.value from dual",
		"select pkg.some_const from caproduct p",
		"select e.content_json.cas, e.id from careglistentry e",
		"select seq.nextval from dual",
		"select * from user_tables",
		"select * from sys.obj$",
		"select * from other.tbl",
		"begin select x.* into v from caproduct p; end;",
		"select p.prod_id from caproduct partition (p1) p",
		"select p.rowid, p.prod_id from caproduct p",
		"select a.prod_id from caproduct a, table(sys.odcivarchar2list('a')) t where t.column_value = 'x'",
		"select p.prod_id from caproduct p, careglistentry e where p.prod_id = e.id(+)",
		"select * from caproduct as of timestamp systimestamp - 1 where prod_id = 1",
		"merge into caproduct p using (select 1 id from dual) s on (p.prod_id = s.id) when matched then update set p.name = 'x'",
		"select q'[fm.* it's]' from dual",
		"select * from caproduct@remote p where p.anything = 1",
		"delete caproduct where prod_id = 1",
		"insert all into caproduct (prod_id) values (1) into careglistentry (id) values (2) select * from dual",
		"begin execute immediate 'create table tmp_x (a number)'; end;\n/\ninsert into tmp_x (a) values (1);\nselect t.a from tmp_x t",
		"alter table caproduct add (new_col number);\nselect p.new_col from caproduct p",
		"delete from caproduct where exists (select 1 from ${baseSchema}.tmp_delete t where t.id = caproduct.prod_id)",
		"select t.x from &schema..tbl t",
	}
	for _, sql := range ora {
		if got := messages(t, oracleCatalog(true), sql); len(got) != 0 {
			t.Errorf("oracle %q: %q", sql, got)
		}
	}
	// synonyms not loaded yet: an unknown unqualified table may be a synonym
	if got := messages(t, oracleCatalog(false), "select * from whatever_view"); len(got) != 0 {
		t.Errorf("oracle without synonyms: %q", got)
	}
	for _, sql := range []string{"select * from sqlite_master", "select t1.rowid from t1", "select * from pragma_table_info('t1')"} {
		if got := messages(t, sqliteCatalog(), sql); len(got) != 0 {
			t.Errorf("sqlite %q: %q", sql, got)
		}
	}
}

func TestPositions(t *testing.T) {
	doc := "select 1;\n\n-- the query\nselect\n    fm.*\nfrom\n    field_definition fd;\n"
	ds := Check(doc, pgCatalog())
	if len(ds) != 1 {
		t.Fatalf("got %v", ds)
	}
	d := ds[0]
	if d.Start != (sqlsplit.Position{Line: 4, Char: 4}) || d.End != (sqlsplit.Position{Line: 4, Char: 6}) {
		t.Errorf("range %v-%v, want 4:4-4:6", d.Start, d.End)
	}
	// on the first line of a statement that doesn't start the line
	ds = Check("select 1; select x.* from batch b;", pgCatalog())
	if len(ds) != 1 || ds[0].Start != (sqlsplit.Position{Line: 0, Char: 17}) {
		t.Errorf("got %v, want start 0:17", ds)
	}
}

func TestViewWrites(t *testing.T) {
	cat := NewCatalog(sqlsplit.PostgreSQL, "cadetst").
		Table("cadetst", "batch", []string{"id", "name"}).
		Table("cadetst", "v_doc", []string{"id", "batch_name"}).
		Table("cadetst", "v_model", []string{"id", "tag"}).
		Table("cadetst", "v_noins", []string{"id"}).
		Table("cadetst", "mv_sum", []string{"batch_id", "documents"}).
		Writes("cadetst", "v_doc", false, false, false, false).
		Writes("cadetst", "v_model", false, true, true, true).
		Writes("cadetst", "v_noins", false, false, true, true).
		Writes("cadetst", "mv_sum", true, false, false, false).
		Complete(true).Build()
	flagged := []struct{ sql, want string }{
		{"insert into v_doc (id) values (1)", "v_doc is a read-only view: the database can't write through it"},
		{"update v_doc set batch_name = 'x'", "v_doc is a read-only view"},
		{"update only v_doc set batch_name = 'x'", "v_doc is a read-only view"},
		{"delete from cadetst.v_doc where id = 1", "v_doc is a read-only view"},
		{"with x as (select 1) insert into v_doc (id) select 1 from x", "v_doc is a read-only view"},
		{"merge into v_doc d using batch b on b.id = d.id when matched then update set batch_name = b.name", "v_doc is a read-only view"},
		{"insert into v_noins (id) values (1)", "view v_noins doesn't accept INSERT (only UPDATE, DELETE)"},
		{"refresh materialized view mv_sum; delete from mv_sum", "mv_sum is a materialized view: it can't be written (refresh it instead)"},
	}
	for _, c := range flagged {
		got := messages(t, cat, c.sql)
		if len(got) != 1 || got[0] != c.want && !strings.HasPrefix(got[0], c.want) {
			t.Errorf("%s\n  got  %q\n  want %q", c.sql, got, c.want)
		}
	}
	quiet := []string{
		"select * from v_doc",
		"insert into batch (id, name) select id, batch_name from v_doc",
		"update batch b set name = d.batch_name from v_doc d where d.id = b.id",
		"delete from batch b using v_doc d where d.id = b.id",
		"insert into v_model (id, tag) values (1, 'x')",
		"update v_model set tag = 'y'",
		"delete from v_noins where id = 1",
		"merge into v_noins n using batch b on b.id = n.id when matched then delete",
		"insert into batch (id, name) values (1, 'a') on conflict (id) do update set name = excluded.name",
		"create view v_doc2 as select * from v_doc; insert into v_doc2 (id) values (1)",
	}
	for _, sql := range quiet {
		if got := messages(t, cat, sql); len(got) > 0 {
			t.Errorf("%s: unexpected %q", sql, got)
		}
	}
	// the name is underlined
	ds := Check("delete from v_doc", cat)
	if len(ds) != 1 || ds[0].Start.Char != 12 || ds[0].End.Char != 17 {
		t.Errorf("range: %+v", ds)
	}
}
