// Package connedit edits the connections of a config file for the panel's
// connection form. Only the lines of the connection being changed are
// rewritten; comments, blank lines and everything else stay as they are.
package connedit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Fields are a connection's form fields, by config key. Lists (schemas) are
// "a, b"; maps (params) are "k=v, k=v".
type Fields map[string]string

// Keys are the form's fields, in order.
var Keys = []string{"alias", "group", "environment", "sample", "driver", "dataSourceName", "proto", "host", "port",
	"path", "dbName", "user", "passwd", "passwdCmd", "schema", "schemas", "params"}

// File is a config file's text and its parsed connections.
type File struct {
	lines []string
	trail bool       // the text ended with a newline
	seq   *yaml.Node // the connections sequence (nil: no such key)
	key   *yaml.Node // the "connections" key
}

// Parse reads a config file's text.
func Parse(text []byte) (*File, error) {
	f := &File{trail: len(text) == 0 || bytes.HasSuffix(text, []byte("\n"))}
	s := strings.TrimSuffix(string(text), "\n")
	if s != "" {
		f.lines = strings.Split(s, "\n")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(text, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return f, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("the config is not a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "connections" {
			f.key, f.seq = root.Content[i], root.Content[i+1]
			if f.seq.Kind != yaml.SequenceNode && !(f.seq.Kind == yaml.ScalarNode && f.seq.Tag == "!!null") {
				return nil, errors.New("connections: is not a list")
			}
		}
	}
	return f, nil
}

// Bytes is the text.
func (f *File) Bytes() []byte {
	s := strings.Join(f.lines, "\n")
	if f.trail && s != "" {
		s += "\n"
	}
	return []byte(s)
}

func (f *File) items() []*yaml.Node {
	if f.seq == nil || f.seq.Kind != yaml.SequenceNode {
		return nil
	}
	return f.seq.Content
}

func aliasOf(item *yaml.Node) string {
	if v := value(item, "alias"); v != nil {
		return v.Value
	}
	return ""
}

func value(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func (f *File) find(alias string) (int, *yaml.Node) {
	for i, it := range f.items() {
		if aliasOf(it) == alias {
			return i, it
		}
	}
	return -1, nil
}

// Aliases lists the connections' aliases, in order.
func (f *File) Aliases() []string {
	var out []string
	for _, it := range f.items() {
		out = append(out, aliasOf(it))
	}
	return out
}

// Get returns a connection's fields.
func (f *File) Get(alias string) (Fields, error) {
	_, it := f.find(alias)
	if it == nil {
		return nil, fmt.Errorf("no connection %q in the config", alias)
	}
	out := Fields{}
	for i := 0; i+1 < len(it.Content); i += 2 {
		k, v := it.Content[i].Value, it.Content[i+1]
		if !contains(Keys, k) {
			continue // sshConfig, …: not in the form, kept as they are
		}
		out[k] = flat(v)
	}
	return out, nil
}

// flat: a value as the form shows it.
func flat(v *yaml.Node) string {
	switch v.Kind {
	case yaml.ScalarNode:
		if v.Tag != "!!null" {
			return v.Value
		}
	case yaml.SequenceNode:
		var parts []string
		for _, c := range v.Content {
			parts = append(parts, c.Value)
		}
		return strings.Join(parts, ", ")
	case yaml.MappingNode:
		var parts []string
		for j := 0; j+1 < len(v.Content); j += 2 {
			parts = append(parts, v.Content[j].Value+"="+v.Content[j+1].Value)
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

// span: the lines (0-based, end exclusive) of item i: from its "- " line
// through the lines indented deeper than the dash, without trailing blank
// and comment lines (those belong to what follows).
func (f *File) span(i int) (start, end, dash int) {
	it := f.items()[i]
	start = it.Line - 1
	dash = strings.Index(f.lines[start], "-")
	end = start + 1
	last := start + 1
	for ; end < len(f.lines); end++ {
		l := f.lines[end]
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if indentOf(l) <= dash {
			break
		}
		last = end + 1
	}
	return start, last, dash
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// render: an item as lines at a dash column.
func render(item *yaml.Node, dash int) ([]string, error) {
	cp := *item
	cp.HeadComment, cp.FootComment = "", ""
	if len(cp.Content) > 0 {
		k := *cp.Content[0]
		k.HeadComment = ""
		cp.Content = append([]*yaml.Node{&k}, cp.Content[1:]...)
		lv := *cp.Content[len(cp.Content)-1]
		lv.FootComment = ""
		lk := *cp.Content[len(cp.Content)-2]
		lk.FootComment = ""
		cp.Content = append(append([]*yaml.Node(nil), cp.Content[:len(cp.Content)-2]...), &lk, &lv)
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{&cp}}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(seq); err != nil {
		return nil, err
	}
	_ = enc.Close()
	pad := strings.Repeat(" ", dash)
	var out []string
	for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if l == "" {
			out = append(out, l)
		} else {
			out = append(out, pad+l)
		}
	}
	return out, nil
}

func (f *File) replaceLines(start, end int, with []string) {
	out := append([]string(nil), f.lines[:start]...)
	out = append(out, with...)
	f.lines = append(out, f.lines[end:]...)
}

// Set writes a connection: replaces old (by alias), or adds a new one after
// the connection after ("" = at the end) when old is "".
func (f *File) Set(old string, fields Fields, after string) error {
	if strings.TrimSpace(fields["alias"]) == "" {
		return errors.New("the connection needs an alias")
	}
	if old == "" || old != fields["alias"] {
		if i, _ := f.find(fields["alias"]); i >= 0 {
			return fmt.Errorf("there is a connection %q already", fields["alias"])
		}
	}
	if old != "" {
		i, it := f.find(old)
		if it == nil {
			return fmt.Errorf("no connection %q in the config", old)
		}
		if err := apply(it, fields); err != nil {
			return err
		}
		start, end, dash := f.span(i)
		lines, err := render(it, dash)
		if err != nil {
			return err
		}
		f.replaceLines(start, end, lines)
		return nil
	}
	item := &yaml.Node{Kind: yaml.MappingNode}
	if err := apply(item, fields); err != nil {
		return err
	}
	items := f.items()
	switch {
	case len(items) > 0:
		at := len(items) - 1
		if after != "" {
			if i, _ := f.find(after); i >= 0 {
				at = i
			}
		}
		_, end, dash := f.span(at)
		lines, err := render(item, dash)
		if err != nil {
			return err
		}
		f.replaceLines(end, end, lines)
	case f.key != nil: // connections: (empty)
		ln := f.key.Line - 1
		lines, err := render(item, f.key.Column+1)
		if err != nil {
			return err
		}
		f.lines[ln] = strings.Repeat(" ", f.key.Column-1) + "connections:"
		f.replaceLines(ln+1, ln+1, lines)
	default:
		lines, err := render(item, 2)
		if err != nil {
			return err
		}
		f.lines = append(f.lines, append([]string{"connections:"}, lines...)...)
	}
	return nil
}

// Delete removes a connection.
func (f *File) Delete(alias string) error {
	i, _ := f.find(alias)
	if i < 0 {
		return fmt.Errorf("no connection %q in the config", alias)
	}
	start, end, _ := f.span(i)
	f.replaceLines(start, end, nil)
	return nil
}

// apply sets the fields on a mapping: empty values remove keys; other keys
// (sshConfig, …) are kept.
func apply(m *yaml.Node, fields Fields) error {
	keys := make([]string, 0, len(fields))
	for _, k := range Keys {
		if _, ok := fields[k]; ok {
			keys = append(keys, k)
		}
	}
	var extra []string
	for k := range fields {
		if !contains(Keys, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)
	for _, k := range keys {
		v := strings.TrimSpace(fields[k])
		node, err := valueNode(k, v)
		if err != nil {
			return err
		}
		idx := -1
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == k {
				idx = i
			}
		}
		if idx >= 0 && flat(m.Content[idx+1]) == v {
			continue // unchanged: keep it as written
		}
		switch {
		case node == nil && idx >= 0:
			m.Content = append(m.Content[:idx], m.Content[idx+2:]...)
		case node == nil:
		case idx >= 0:
			old := m.Content[idx+1]
			node.LineComment, node.HeadComment, node.FootComment = old.LineComment, old.HeadComment, old.FootComment
			if old.Kind == yaml.ScalarNode && node.Kind == yaml.ScalarNode && old.Style != 0 && node.Tag == "!!str" {
				node.Style = old.Style // keep its quotes
			}
			m.Content[idx+1] = node
		default:
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, node)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// valueNode: the YAML node of a form value (nil: remove the key).
func valueNode(key, v string) (*yaml.Node, error) {
	if v == "" {
		return nil, nil
	}
	switch key {
	case "port":
		if _, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("port: %q is not a number", v)
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v}, nil
	case "schemas":
		n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p})
			}
		}
		return n, nil
	case "params":
		n := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
		for _, p := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("params: %q is not key=value", p)
			}
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: strings.TrimSpace(k)},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: strings.TrimSpace(val)})
		}
		return n, nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}, nil
}

// WriteFile replaces path's contents atomically, keeping its permissions.
func WriteFile(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // a dotfiles link stays a link
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
