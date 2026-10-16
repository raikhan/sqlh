// Package header reads and writes a query file's connection header, a
// comment among the file's leading comments:
//
//	-- sqlh: conn=pg_local db=reports schema=sales
//
// The editor's server keeps it pointing at the connection (and database and
// schema, when switched) the file last ran on; `sqlh dump` runs the file there.
package header

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Keys written by sqlh; others are kept as they are.
const (
	KeyConn   = "conn"
	KeyDB     = "db"
	KeySchema = "schema"
)

// Header is a file's "-- sqlh:" line.
type Header struct {
	Conn   string
	DB     string
	Schema string
	// Line is the 0-based line of the header, -1 when the file has none.
	Line int
	// text is the header line as found (without the line break).
	text  string
	extra []pair // other keys, in order
}

type pair struct{ key, value string }

var headerRe = regexp.MustCompile(`(?i)^\s*--\s*sqlh\s*:(.*)$`)

// Parse finds the header among the leading comments of text (before the
// first line of SQL). A file without one gets Line -1.
func Parse(text string) Header {
	inBlock := false
	for i, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case inBlock:
			if strings.Contains(t, "*/") {
				inBlock = false
				if rest := strings.TrimSpace(t[strings.Index(t, "*/")+2:]); rest != "" && !strings.HasPrefix(rest, "--") {
					return Header{Line: -1} // SQL after the comment
				}
			}
		case t == "":
		case strings.HasPrefix(t, "--"):
			if m := headerRe.FindStringSubmatch(t); m != nil {
				h := parsePairs(m[1])
				h.Line, h.text = i, strings.TrimSuffix(line, "\r")
				return h
			}
		case strings.HasPrefix(t, "/*"):
			if end := strings.Index(t[2:], "*/"); end < 0 {
				inBlock = true
			} else if rest := strings.TrimSpace(t[end+4:]); rest != "" && !strings.HasPrefix(rest, "--") {
				return Header{Line: -1}
			}
		default:
			return Header{Line: -1}
		}
	}
	return Header{Line: -1}
}

// parsePairs reads `key=value key="value with spaces" …`.
func parsePairs(s string) Header {
	var h Header
	for _, f := range fields(s) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			h.extra = append(h.extra, pair{key: f})
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil && strings.HasPrefix(v, `"`) {
			v = uq
		}
		switch strings.ToLower(k) {
		case KeyConn:
			h.Conn = v
		case KeyDB:
			h.DB = v
		case KeySchema:
			h.Schema = v
		default:
			h.extra = append(h.extra, pair{k, v})
		}
	}
	return h
}

// fields splits on whitespace outside double quotes.
func fields(s string) []string {
	var out []string
	var b strings.Builder
	quoted, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case !quoted && unicode.IsSpace(r):
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// String renders the header line: conn, db, schema, then the other keys.
func (h Header) String() string {
	parts := []string{"-- sqlh:"}
	add := func(k, v string) {
		if v == "" {
			parts = append(parts, k)
			return
		}
		if strings.ContainsAny(v, " \t\"\\") {
			v = strconv.Quote(v)
		}
		parts = append(parts, k+"="+v)
	}
	if h.Conn != "" {
		add(KeyConn, h.Conn)
	}
	if h.DB != "" {
		add(KeyDB, h.DB)
	}
	if h.Schema != "" {
		add(KeySchema, h.Schema)
	}
	for _, p := range h.extra {
		add(p.key, p.value)
	}
	return strings.Join(parts, " ")
}

// Span is the header text on its line as [start, end) in UTF-16 code units
// (indentation excluded), for diagnostics.
func (h Header) Span() (start, end int) {
	return utf16Len(indent(h.text)), utf16Len(h.text)
}

// Edit replaces Line[0:OldLen] (UTF-16 code units, as LSP counts) with Text.
// OldLen 0 at Line 0 is an insertion at the start of the file.
type Edit struct {
	Line   int
	OldLen int
	Text   string
}

// Stamp returns the edit that makes text's header name conn, db and schema
// ("" drops db/schema), inserting a header at the top when there is none. ok
// is false when the header is already right.
func Stamp(text, conn, db, schema string) (e Edit, ok bool) {
	h := Parse(text)
	if h.Line >= 0 {
		if h.Conn == conn && h.DB == db && h.Schema == schema {
			return Edit{}, false
		}
		old := h.text
		h.Conn, h.DB, h.Schema = conn, db, schema
		return Edit{Line: h.Line, OldLen: utf16Len(old), Text: indent(old) + h.String()}, true
	}
	firstLine, _, _ := strings.Cut(text, "\n")
	nl := "\n"
	if strings.HasSuffix(firstLine, "\r") {
		nl = "\r\n"
	}
	ins := Header{Conn: conn, DB: db, Schema: schema}.String() + nl
	// a blank line between the header and SQL; none before other comments
	if first := strings.TrimSpace(firstLine); first != "" && !strings.HasPrefix(first, "--") && !strings.HasPrefix(first, "/*") {
		ins += nl
	}
	return Edit{Text: ins}, true
}

// Apply applies e to text (for tests and the standalone CLI).
func Apply(text string, e Edit) string {
	lines := strings.SplitAfter(text, "\n")
	if e.Line >= len(lines) {
		return text + e.Text
	}
	start := 0
	for _, l := range lines[:e.Line] {
		start += len(l)
	}
	end := start + byteOffset(lines[e.Line], e.OldLen)
	return text[:start] + e.Text + text[end:]
}

func indent(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// byteOffset converts a UTF-16 offset in s to a byte offset.
func byteOffset(s string, units int) int {
	n := 0
	for i, r := range s {
		if n >= units {
			return i
		}
		n += len(utf16.Encode([]rune{r}))
	}
	return len(s)
}
