package ddl

import (
	"regexp"
	"strings"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// partialWordRe: the word being typed at the cursor.
var partialWordRe = regexp.MustCompile(`[\w$#]+$`)

// AtCursor analyses the statement being typed at a 0-based line and UTF-16
// character: the statement's text up to the cursor, without the word being
// typed (completion filters by it). ok is false when there is no statement
// there or it can't be tokenized.
func AtCursor(text string, line, char int, d sqlsplit.Dialect) (*Analysis, bool) {
	before := textBefore(text, line, char)
	before = before[:len(before)-len(partialWordRe.FindString(before))]
	stmts := sqlsplit.Split(before, d)
	if len(stmts) == 0 {
		return nil, false
	}
	last := stmts[len(stmts)-1]
	if last.Terminated {
		return nil, false
	}
	toks, ok := Tokenize(last.SQL)
	if !ok {
		return nil, false
	}
	return Analyze(withCursor(toks), d), true
}

// Partial is the word being typed at a 0-based line and UTF-16 character
// (Oracle names may contain $ and #).
func Partial(text string, line, char int) string {
	return partialWordRe.FindString(textBefore(text, line, char))
}

// textBefore is text up to a 0-based line and UTF-16 character.
func textBefore(text string, line, char int) string {
	off := 0
	for l := 0; l < line; l++ {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			return text
		}
		off += i + 1
	}
	rest := text[off:]
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[:i]
	}
	units := 0
	for b, r := range rest {
		if units >= char {
			return text[:off+b]
		}
		units++
		if r > 0xFFFF {
			units++
		}
	}
	return text[:off+len(strings.TrimRight(rest, "\r"))]
}

// InStatement analyses a whole statement (for hover and lint).
func InStatement(sql string, d sqlsplit.Dialect) (*Analysis, []Tok, bool) {
	toks, ok := Tokenize(sql)
	if !ok {
		return nil, nil, false
	}
	return Analyze(toks, d), toks, true
}
