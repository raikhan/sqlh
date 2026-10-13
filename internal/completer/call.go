package completer

import (
	"strings"
	"unicode"
)

// FunctionCall is the function call whose argument list encloses a position.
type FunctionCall struct {
	Qualifier string // pkg in pkg.fn(…), "" if none
	Name      string
	ArgIndex  int // 0-based index of the argument the position is in
}

// FunctionCallAt finds the innermost parenthesis open at (line, char) (LSP
// position, UTF-16 columns) and the name in front of it. It scans the text up
// to the position, skipping string literals, quoted identifiers and comments,
// so it works on incomplete statements.
func FunctionCallAt(text string, line, char int) (FunctionCall, bool) {
	end := offsetOf(text, line, char)
	if end < 0 {
		return FunctionCall{}, false
	}
	type open struct {
		pos    int // offset of "("
		commas int
	}
	var stack []open
	s := text[:end]
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			i = skipQuoted(s, i, '\'')
		case c == '"':
			i = skipQuoted(s, i, '"')
		case c == '`':
			i = skipQuoted(s, i, '`')
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(s)
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			if j := strings.Index(s[i+2:], "*/"); j >= 0 {
				i += j + 3
			} else {
				i = len(s)
			}
		case c == '(':
			stack = append(stack, open{pos: i})
		case c == ')':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case c == ',':
			if len(stack) > 0 {
				stack[len(stack)-1].commas++
			}
		case c == ';':
			stack = stack[:0]
		}
	}
	if len(stack) == 0 {
		return FunctionCall{}, false
	}
	top := stack[len(stack)-1]
	qual, name := nameBefore(s, top.pos)
	if name == "" {
		return FunctionCall{}, false
	}
	return FunctionCall{Qualifier: qual, Name: name, ArgIndex: top.commas}, true
}

// skipQuoted returns the offset of the quote closing the one at i (” and ""
// escapes are two quotes in a row, which this handles as close + reopen).
func skipQuoted(s string, i int, q byte) int {
	j := strings.IndexByte(s[i+1:], q)
	if j < 0 {
		return len(s)
	}
	return i + 1 + j
}

func isNameByte(c byte) bool {
	return c == '_' || c == '$' || c == '#' || c >= 0x80 ||
		unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}

// nameBefore reads [qualifier.]name immediately before offset (spaces allowed
// before the parenthesis).
func nameBefore(s string, pos int) (string, string) {
	i := pos
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
		i--
	}
	readPart := func(end int) (string, int) {
		if end > 0 && s[end-1] == '"' {
			start := strings.LastIndexByte(s[:end-1], '"')
			if start < 0 {
				return "", end
			}
			return s[start+1 : end-1], start
		}
		start := end
		for start > 0 && isNameByte(s[start-1]) {
			start--
		}
		return s[start:end], start
	}
	name, start := readPart(i)
	if name == "" || unicode.IsDigit(rune(name[0])) {
		return "", ""
	}
	if start > 0 && s[start-1] == '.' {
		qual, _ := readPart(start - 1)
		return qual, name
	}
	return "", name
}

// offsetOf converts an LSP position (UTF-16 column) to a byte offset in text.
func offsetOf(text string, line, char int) int {
	off := 0
	for l := 0; l < line; l++ {
		j := strings.IndexByte(text[off:], '\n')
		if j < 0 {
			return -1
		}
		off += j + 1
	}
	lineEnd := strings.IndexByte(text[off:], '\n')
	lineText := text[off:]
	if lineEnd >= 0 {
		lineText = text[off : off+lineEnd]
	}
	return off + byteOffsetForUTF16(lineText, char)
}
