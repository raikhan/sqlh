package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/raikhan/sqlh/internal/control"
)

func (d *dumpServer) waitCache(cond func() bool) bool {
	for i := 0; i < 300; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (d *dumpServer) diagMessages() []string {
	d.editor.mu.Lock()
	defer d.editor.mu.Unlock()
	var out []string
	for _, x := range d.editor.diags[d.uri] {
		out = append(out, x.Message)
	}
	return out
}

func TestRefreshAfterDDL(t *testing.T) {
	d := newDumpServer(t, "create table u (id integer);\nalter table t add column extra text;\nalter table t add column name text;\n")
	hasTable := func(name string) bool {
		c := d.s.worker.Cache()
		if c == nil {
			return false
		}
		for _, x := range c.SortedTables() {
			if x == name {
				return true
			}
		}
		return false
	}
	if !d.waitCache(func() bool { return hasTable("t") }) {
		t.Fatal("no cache")
	}

	res := d.run(control.RunParams{File: d.file, Line: 1})
	if !strings.Contains(res.Summary, "schema refreshed") {
		t.Errorf("summary %q", res.Summary)
	}
	if !d.waitCache(func() bool { return hasTable("u") }) {
		t.Error("u not in the cache after the run")
	}

	// the run stamped the connection header: find the statement's line
	text := d.waitEdit(0)
	line := 1
	for i, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "extra") {
			line = i + 1
		}
	}
	res = d.run(control.RunParams{File: d.file, Line: line})
	if !strings.Contains(res.Summary, "schema refreshed") {
		t.Errorf("summary %q", res.Summary)
	}
	if !d.waitCache(func() bool { _, ok := d.s.worker.Cache().Column("t", "extra"); return ok }) {
		t.Error("extra not in the cache")
	}
	// the statement just run isn't linted any more (extra exists now); the
	// same mistake not run yet is
	ok := d.waitCache(func() bool {
		msgs := strings.Join(d.diagMessages(), "|")
		return strings.Contains(msgs, "t already has a column name") && !strings.Contains(msgs, "extra")
	})
	if !ok {
		t.Errorf("diagnostics %q", d.diagMessages())
	}

	// a query: no refresh
	res = d.run(control.RunParams{SQL: "select 1"})
	if strings.Contains(res.Summary, "schema") {
		t.Errorf("summary %q", res.Summary)
	}
}

func TestRefreshAfterDDLOff(t *testing.T) {
	d := newDumpServer(t, "create table u (id integer);\n")
	off := false
	d.cfg.RefreshAfterDDL = &off
	res := d.run(control.RunParams{File: d.file, Line: 1})
	if strings.Contains(res.Summary, "schema") {
		t.Errorf("summary %q", res.Summary)
	}
}
