// Package jsoncomplete completes keys inside JSON documents stored in
// columns: PostgreSQL json/jsonb operators (col->'key', col #> '{a,b}',
// jsonb_path_*(col, '$.a')) and Oracle SQL/JSON functions
// (json_value(col, '$.a.b')). Keys are sampled from the data.
package jsoncomplete

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// Style is how the key appears in SQL.
type Style int

const (
	// StyleArrow: PostgreSQL col->'key' / col->>'key' / col ? 'key'.
	StyleArrow Style = iota
	// StyleArray: PostgreSQL col #> '{a,b,key}' (text[] path).
	StyleArray
	// StyleJSONPath: SQL/JSON path '$.a.key' (Oracle json_*, PG jsonb_path_*).
	StyleJSONPath
)

// Position is an LSP position (0-based line, UTF-16 column).
type Position struct{ Line, Char int }

// Context describes a JSON key being typed at the cursor.
type Context struct {
	Style Style
	// Column reference as written, split: [schema.]table/alias qualifier and column.
	Qualifier string
	Column    string
	// Path to the object whose keys are wanted (keys, or array indexes as
	// decimal strings). Arrays along the path are looked through.
	Path []string
	// Prefix is what has been typed of the key.
	Prefix string
	// Replace is where the completion's text edit starts (to the cursor).
	Replace Position
}

type tkind int

const (
	tSpace tkind = iota
	tComment
	tString // '...'; open = unterminated
	tQuoted // "..."
	tWord
	tOp
	tPunct
)

type tok struct {
	kind  tkind
	text  string
	open  bool // unterminated string
	start int  // byte offset
}

// lex tokenizes SQL (enough to find JSON contexts). In PostgreSQL '#' is an
// operator character; in Oracle it is part of identifiers.
func lex(s string, d sqlsplit.Dialect) []tok {
	var out []tok
	opChars := "-+*/<>=!~^&|%:?@"
	if d != sqlsplit.Oracle {
		opChars += "#"
	}
	isWord := func(r rune) bool {
		return r == '_' || r == '$' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r) ||
			(d == sqlsplit.Oracle && r == '#')
	}
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		start := i
		switch {
		case unicode.IsSpace(r):
			for i < len(s) {
				r2, sz := utf8.DecodeRuneInString(s[i:])
				if !unicode.IsSpace(r2) {
					break
				}
				i += sz
			}
			out = append(out, tok{kind: tSpace, text: s[start:i], start: start})
		case strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = len(s)
			} else {
				i += j
			}
			out = append(out, tok{kind: tComment, text: s[start:i], start: start})
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				i = len(s)
			} else {
				i += j + 4
			}
			out = append(out, tok{kind: tComment, text: s[start:i], start: start})
		case r == '\'' || r == '"':
			i++
			open := true
			for i < len(s) {
				if s[i] == byte(r) {
					if i+1 < len(s) && s[i+1] == byte(r) { // doubled quote
						i += 2
						continue
					}
					i++
					open = false
					break
				}
				i++
			}
			k := tString
			if r == '"' {
				k = tQuoted
			}
			out = append(out, tok{kind: k, text: s[start:i], open: open, start: start})
		case isWord(r):
			for i < len(s) {
				r2, sz := utf8.DecodeRuneInString(s[i:])
				if !isWord(r2) {
					break
				}
				i += sz
			}
			out = append(out, tok{kind: tWord, text: s[start:i], start: start})
		case strings.ContainsRune(opChars, r):
			for i < len(s) && strings.IndexByte(opChars, s[i]) >= 0 {
				if strings.HasPrefix(s[i:], "--") || strings.HasPrefix(s[i:], "/*") {
					break
				}
				i++
			}
			out = append(out, tok{kind: tOp, text: s[start:i], start: start})
		default:
			i += size
			out = append(out, tok{kind: tPunct, text: s[start:i], start: start})
		}
	}
	return out
}

// sig returns tokens without whitespace and comments.
func sig(ts []tok) []tok {
	var out []tok
	for _, t := range ts {
		if t.kind != tSpace && t.kind != tComment {
			out = append(out, t)
		}
	}
	return out
}

// unquoteString returns the content of a '...' literal (open allowed).
func unquoteString(t tok) string {
	s := strings.TrimPrefix(t.text, "'")
	if !t.open {
		s = strings.TrimSuffix(s, "'")
	}
	return strings.ReplaceAll(s, "''", "'")
}

var jsonPathFuncs = map[string]bool{
	// Oracle SQL/JSON
	"JSON_VALUE": true, "JSON_QUERY": true, "JSON_EXISTS": true, "JSON_TABLE": true,
	// PostgreSQL jsonpath
	"JSONB_PATH_QUERY": true, "JSONB_PATH_QUERY_ARRAY": true, "JSONB_PATH_QUERY_FIRST": true,
	"JSONB_PATH_EXISTS": true, "JSONB_PATH_MATCH": true,
}

// Detect finds a JSON key context at the cursor (line, UTF-16 char).
func Detect(text string, line, char int, d sqlsplit.Dialect) (*Context, bool) {
	off, ok := byteOffset(text, line, char)
	if !ok {
		return nil, false
	}
	ts := sig(lex(text[:off], d))
	n := len(ts)
	if n < 2 || ts[n-1].kind != tString || !ts[n-1].open {
		return nil, false
	}
	lit := ts[n-1]
	content := unquoteString(lit)
	litContentStart := lit.start + 1

	// SQL/JSON path: <func>( <col> , '$...
	if n >= 5 && ts[n-2].text == "," && (ts[n-3].kind == tWord || ts[n-3].kind == tQuoted) &&
		ts[n-4].text == "(" && ts[n-5].kind == tWord && jsonPathFuncs[strings.ToUpper(ts[n-5].text)] {
		path, prefix, keyStart, ok := parseJSONPath(content)
		if !ok {
			return nil, false
		}
		q, col := splitRef(ts[n-3])
		return &Context{
			Style: StyleJSONPath, Qualifier: q, Column: col, Path: path, Prefix: prefix,
			Replace: positionOf(text, litContentStart+rawOffset(lit, keyStart)),
		}, true
	}

	// PostgreSQL operators
	if d == sqlsplit.Oracle || ts[n-2].kind != tOp {
		return nil, false
	}
	op := ts[n-2].text
	var (
		ctx = &Context{}
		k   = n - 3 // token before the operator
	)
	switch op {
	case "->", "->>", "?":
		ctx.Style = StyleArrow
		ctx.Prefix = content
		ctx.Replace = positionOf(text, litContentStart)
	case "#>", "#>>":
		if !strings.HasPrefix(content, "{") {
			return nil, false
		}
		ctx.Style = StyleArray
		inner := content[1:]
		parts := strings.Split(inner, ",")
		for _, p := range parts[:len(parts)-1] {
			ctx.Path = append(ctx.Path, strings.Trim(strings.TrimSpace(p), `"`))
		}
		last := parts[len(parts)-1]
		lead := len(last) - len(strings.TrimLeft(last, " "))
		ctx.Prefix = strings.TrimLeft(last, ` "`)
		keyStart := len(content) - len(last) + lead
		if strings.HasPrefix(strings.TrimLeft(last, " "), `"`) {
			keyStart++
		}
		ctx.Replace = positionOf(text, litContentStart+rawOffset(lit, keyStart))
	default:
		return nil, false
	}

	// Walk back over a chain: <colref> ( -> <segment> )* <op> '<prefix>
	var chain []string
	for k >= 0 {
		t := ts[k]
		if (t.kind == tString && !t.open) || (t.kind == tWord && isInt(t.text)) {
			if k-1 >= 0 && ts[k-1].kind == tOp && (ts[k-1].text == "->" || ts[k-1].text == "->>") {
				seg := t.text
				if t.kind == tString {
					seg = unquoteString(t)
				}
				chain = append([]string{seg}, chain...)
				k -= 2
				continue
			}
			return nil, false
		}
		if t.kind == tWord || t.kind == tQuoted {
			ctx.Qualifier, ctx.Column = splitRef(t)
			ctx.Path = append(chain, ctx.Path...)
			return ctx, ctx.Column != ""
		}
		return nil, false
	}
	return nil, false
}

func isInt(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// splitRef splits a column reference "a.b.c" into qualifier "a.b" and "c".
// Double-quoted identifiers keep their exact spelling.
func splitRef(t tok) (qualifier, column string) {
	if t.kind == tQuoted {
		return "", strings.Trim(t.text, `"`)
	}
	parts := strings.Split(t.text, ".")
	if len(parts) == 1 {
		return "", parts[0]
	}
	return strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1]
}

// parseJSONPath parses an incomplete SQL/JSON path ("$.a[*]."b c".pre")
// into the parent path, the typed prefix and the byte offset (in content)
// where the last step — including its leading '.' — starts.
func parseJSONPath(content string) (path []string, prefix string, stepStart int, ok bool) {
	s := strings.TrimLeft(content, " ")
	lead := len(content) - len(s)
	if !strings.HasPrefix(s, "$") {
		return nil, "", 0, false
	}
	i := 1
	stepStart = lead + i
	for i < len(s) {
		switch s[i] {
		case '.':
			stepStart = lead + i
			i++
			if i < len(s) && s[i] == '"' {
				j := strings.IndexByte(s[i+1:], '"')
				if j < 0 { // typing a quoted key
					return path, s[i+1:], stepStart, true
				}
				key := s[i+1 : i+1+j]
				i += j + 2
				if i >= len(s) {
					return path, key, stepStart, true
				}
				path = append(path, key)
				continue
			}
			// unquoted step: up to the next step. Spaces are allowed so a
			// partly typed key like "CAS R" still completes (to ."CAS RN").
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			key := s[i:j]
			if j >= len(s) {
				return path, key, stepStart, true
			}
			path = append(path, key)
			i = j
		case '[': // array step: looked through
			j := strings.IndexByte(s[i:], ']')
			if j < 0 {
				return nil, "", 0, false
			}
			i += j + 1
			stepStart = lead + i
		default:
			return nil, "", 0, false
		}
	}
	return path, "", lead + len(s), true // "$" or "$.a[*]": nothing typed yet
}

// rawOffset maps an offset in the unescaped literal content to the source
// text (where ” stands for ').
func rawOffset(lit tok, contentOff int) int {
	raw := strings.TrimPrefix(lit.text, "'")
	n, c := 0, 0
	for n < len(raw) && c < contentOff {
		if strings.HasPrefix(raw[n:], "''") {
			n += 2
		} else {
			n++
		}
		c++
	}
	return n
}

func byteOffset(text string, line, char int) (int, bool) {
	off := 0
	for l := 0; l < line; l++ {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			return 0, false
		}
		off += i + 1
	}
	col := 0
	for off < len(text) && col < char {
		r, size := utf8.DecodeRuneInString(text[off:])
		if r == '\n' {
			break
		}
		if r > 0xFFFF {
			col += 2
		} else {
			col++
		}
		off += size
	}
	return off, true
}

func positionOf(text string, off int) Position {
	line, col := 0, 0
	for _, r := range text[:off] {
		if r == '\n' {
			line, col = line+1, 0
		} else if r > 0xFFFF {
			col += 2
		} else {
			col++
		}
	}
	return Position{Line: line, Char: col}
}

// InString reports whether the cursor is inside an unterminated '...'
// literal (where ordinary completion is noise).
func InString(text string, line, char int, d sqlsplit.Dialect) bool {
	off, ok := byteOffset(text, line, char)
	if !ok {
		return false
	}
	ts := sig(lex(text[:off], d))
	return len(ts) > 0 && ts[len(ts)-1].kind == tString && ts[len(ts)-1].open
}
