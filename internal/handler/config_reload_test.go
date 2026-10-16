package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/control"
)

func TestReloadConfig(t *testing.T) {
	d := newDumpServer(t, "-- sqlh: conn=lite\nselect name from t where id = 1;\n")
	var st control.Status
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite"}, &st); err != nil {
		t.Fatal(err)
	}
	// the config as a file: lite, then lite2 renamed and a group
	a := d.cfg.Connections[0].DataSourceName
	b := d.cfg.Connections[1].DataSourceName
	path := filepath.Join(t.TempDir(), "config.yml")
	write := func(text string) {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		// mtimes may be coarse
		later := time.Now().Add(time.Duration(len(text)) * time.Millisecond)
		_ = os.Chtimes(path, later, later)
	}
	write("connections:\n  - alias: lite\n    driver: sqlite3\n    dataSourceName: " + a + "\n")
	d.s.mu.Lock()
	d.s.ConfigPath = path
	d.s.mu.Unlock()

	write("connections:\n  - alias: other\n    driver: sqlite3\n    dataSourceName: " + b + "\n    group: G\n" +
		"  - alias: lite\n    driver: sqlite3\n    dataSourceName: " + a + "\n")
	var msg string
	if err := d.call(control.MethodReloadConfig, nil, &msg); err != nil {
		t.Fatal(err)
	}
	var conns []control.Connection
	if err := d.call(control.MethodConnections, nil, &conns); err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 || conns[0].Alias != "other" || conns[0].Group != "G" || conns[0].Database != "b.db" || !conns[1].Current {
		t.Fatalf("connections %+v", conns)
	}
	// the current connection follows its alias to its new place
	if err := d.call(control.MethodStatus, nil, &st); err != nil || st.Connection == nil || st.Connection.Alias != "lite" {
		t.Fatalf("status %+v %v", st.Connection, err)
	}
	// changed settings of the current connection: reconnected
	write("connections:\n  - alias: lite\n    driver: sqlite3\n    dataSourceName: " + b + "\n")
	if err := d.call(control.MethodReloadConfig, nil, &msg); err != nil || !strings.Contains(msg, "switched connection to") {
		t.Fatalf("reload: %q %v", msg, err)
	}
	res := d.run(control.RunParams{SQL: "select name from t where id = 1", Table: true})
	if !strings.Contains(res.Table, "b.db") {
		t.Errorf("still on a.db: %s", res.Table)
	}
	// a broken file: refused, the old config stays
	write("connections: [\n")
	if err := d.call(control.MethodReloadConfig, nil, &msg); err == nil {
		t.Error("a broken config was accepted")
	}
	if err := d.call(control.MethodConnections, nil, &conns); err != nil || len(conns) != 1 {
		t.Errorf("after a broken file: %+v %v", conns, err)
	}
}

func TestAllColumns(t *testing.T) {
	d := newDumpServer(t, "select 1;\n")
	var st control.Status
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite"}, &st); err != nil {
		t.Fatal(err)
	}
	var cols []control.ColumnRef
	for i := 0; i < 100; i++ {
		if err := d.call(control.MethodAllColumns, nil, &cols); err == nil && len(cols) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var names []string
	for _, c := range cols {
		names = append(names, c.TableInsert+"."+c.Name)
	}
	if strings.Join(names, ",") != "t.id,t.name" {
		t.Errorf("columns %v", names)
	}
}

func TestRunRefusesDBHeader(t *testing.T) {
	d := newDumpServer(t, "-- sqlh: conn=lite db=other\nselect 1;\n")
	var st control.Status
	if err := d.call(control.MethodConnect, control.ConnectParams{Alias: "lite"}, &st); err != nil {
		t.Fatal(err)
	}
	var res control.RunResult
	err := d.call(control.MethodRun, control.RunParams{File: d.file, Line: 2, Table: true}, &res)
	if err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Errorf("run: %v %+v", err, res)
	}
}
