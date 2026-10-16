package cmd

import (
	"reflect"
	"testing"

	"github.com/urfave/cli/v2"
)

func TestReorderArgs(t *testing.T) {
	app := &cli.App{
		Flags:    []cli.Flag{&cli.StringFlag{Name: "config", Aliases: []string{"c"}}},
		Commands: Commands(),
	}
	cases := []struct{ in, want []string }{
		{[]string{"sqlh", "columns", "t", "-p", "--prefix", "p."}, []string{"sqlh", "columns", "-p", "--prefix", "p.", "t"}},
		{[]string{"sqlh", "-c", "x.yml", "columns", "t", "--long"}, []string{"sqlh", "-c", "x.yml", "columns", "--long", "t"}},
		{[]string{"sqlh", "connect", "pg_local", "-C", "/tmp"}, []string{"sqlh", "connect", "-C", "/tmp", "pg_local"}},
		{[]string{"sqlh", "run", "--sql=select 1", "--limit", "5"}, []string{"sqlh", "run", "--sql=select 1", "--limit", "5"}},
		{[]string{"sqlh", "columns", "--", "-weird-name"}, []string{"sqlh", "columns", "--", "-weird-name"}},
		{[]string{"sqlh"}, []string{"sqlh"}},
		{[]string{"sqlh", "panel", "run", "-C", "/w", "--editor-pane", "w1:p1"}, []string{"sqlh", "panel", "run", "-C", "/w", "--editor-pane", "w1:p1"}},
		{[]string{"sqlh", "panel", "take", "--editor", "-f", "q.sql"}, []string{"sqlh", "panel", "take", "--editor", "-f", "q.sql"}},
	}
	for _, c := range cases {
		if got := ReorderArgs(app, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ReorderArgs(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
