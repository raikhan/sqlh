package cmd

import (
	"reflect"
	"testing"

	"github.com/urfave/cli/v2"
)

func TestChildArgs(t *testing.T) {
	app := &cli.App{
		Flags:    []cli.Flag{&cli.StringFlag{Name: "config", Aliases: []string{"c"}}},
		Commands: Commands(),
	}
	drop := []string{"limit", "sql", "bg"}
	cases := []struct{ in, want []string }{
		{
			[]string{"sqlh", "run", "--editor", "--bg", "--open", "-f", "a.sql", "-l", "3", "--limit", "ask"},
			[]string{"sqlh", "run", "--editor", "--open", "-f", "a.sql", "-l", "3"},
		},
		{ // aliases, --flag=value, global flags kept
			[]string{"sqlh", "-c", "x.yml", "run", "-n", "5", "--sql=select 1", "--bg=true", "--file", "a.sql"},
			[]string{"sqlh", "-c", "x.yml", "run", "--file", "a.sql"},
		},
		{ // export has no --limit: nothing else is dropped
			[]string{"sqlh", "export", "--bg", "-e", "select 1", "-o", "x.csv"},
			[]string{"sqlh", "export", "-o", "x.csv"},
		},
		{ // dump's positional FILE stays
			[]string{"sqlh", "dump", "--editor", "--bg", "q.sql", "-l", "4", "--fresh"},
			[]string{"sqlh", "dump", "--editor", "q.sql", "-l", "4", "--fresh"},
		},
	}
	for _, c := range cases {
		if got := childArgs(app, c.in, drop); !reflect.DeepEqual(got, c.want) {
			t.Errorf("childArgs(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		in   string
		want limitChoice
		err  bool
	}{
		{"", limitChoice{}, false},
		{"10", limitChoice{n: 10}, false},
		{"10_000", limitChoice{n: 10000}, false},
		{"ALL", limitChoice{all: true}, false},
		{"ask", limitChoice{ask: true}, false},
		{"0", limitChoice{}, true},
		{"-5", limitChoice{}, true},
		{"many", limitChoice{}, true},
	}
	for _, c := range cases {
		got, err := parseLimit(c.in)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("parseLimit(%q) = %+v, %v; want %+v, err %v", c.in, got, err, c.want, c.err)
		}
	}
}

func TestViewerArgv(t *testing.T) {
	files := []string{"/r/a.csv", "/r/b.csv"}
	cases := []struct{ viewer, want []string }{
		{[]string{"vd"}, []string{"vd", "/r/a.csv", "/r/b.csv"}},
		{[]string{"pane.nu", "0.8", "vd", "{}"}, []string{"pane.nu", "0.8", "vd", "/r/a.csv", "/r/b.csv"}},
		{[]string{"x", "{}", "--flag"}, []string{"x", "/r/a.csv", "/r/b.csv", "--flag"}},
	}
	for _, c := range cases {
		if got := viewerArgv(c.viewer, files); !reflect.DeepEqual(got, c.want) {
			t.Errorf("viewerArgv(%q) = %q, want %q", c.viewer, got, c.want)
		}
	}
	if got := viewerArgv([]string{"~/bin/v"}, files)[0]; got == "~/bin/v" {
		t.Errorf("~ not expanded: %q", got)
	}
}
