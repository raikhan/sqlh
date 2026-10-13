package completer

import (
	"strings"
	"unicode"

	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
)

// StatementContext describes the statement around a position, for the CLI's
// table picker.
type StatementContext struct {
	// InJoin: the position is where a JOIN's table goes.
	InJoin bool
	// Tables: the database tables of the position's query level (with InJoin,
	// those before it: the ones an ON condition can use).
	Tables []*parseutil.TableInfo
	// Focus: the table whose name or alias is under the position.
	Focus *parseutil.TableInfo
	// NeedSpace: the character before the position is not white space.
	NeedSpace bool
	// Indent: the leading white space of the position's line.
	Indent string
}

// CursorContext analyses text at a 0-based line and character.
func CursorContext(text string, line, char int) *StatementContext {
	sc := &StatementContext{}
	lines := strings.Split(text, "\n")
	if line < 0 || line >= len(lines) {
		return sc
	}
	cur := []rune(strings.TrimRight(lines[line], "\r"))
	if char > len(cur) {
		char = len(cur)
	}
	sc.Indent = string(cur[:len(cur)-len([]rune(strings.TrimLeft(string(cur), " \t")))])
	if char > 0 && !unicode.IsSpace(cur[char-1]) && cur[char-1] != '(' {
		sc.NeedSpace = true
	}

	parsed, err := parser.Parse(text)
	if err != nil {
		return sc
	}
	pos := token.Pos{Line: line, Col: char}
	nw := parseutil.NewNodeWalker(parsed, pos)
	sc.InJoin = parseutil.CheckSyntaxPosition(nw) == parseutil.JoinClause || prevWordIs(lines, line, char, "JOIN")

	var tables []*parseutil.TableInfo
	if sc.InJoin {
		tables, err = parseutil.ExtractPrevTables(parsed, pos)
	} else {
		tables, err = parseutil.ExtractTable(parsed, pos)
	}
	if err != nil {
		return sc
	}
	for _, t := range tables {
		if !t.Outer && t.Name != "" {
			sc.Tables = append(sc.Tables, t)
		}
	}

	if word := wordAt(cur, char); word != "" {
		for _, t := range sc.Tables {
			if strings.EqualFold(t.Alias, word) || strings.EqualFold(t.Name, word) {
				sc.Focus = t
				break
			}
		}
	}
	return sc
}

func isIdentRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' || r == '#'
}

// wordAt returns the identifier under (or just before) char.
func wordAt(line []rune, char int) string {
	start, end := char, char
	for start > 0 && isIdentRune(line[start-1]) {
		start--
	}
	for end < len(line) && isIdentRune(line[end]) {
		end++
	}
	return string(line[start:end])
}

// prevWordIs reports whether the last word before the position (across
// lines) is word, case-insensitively.
func prevWordIs(lines []string, line, char int, word string) bool {
	before := []rune(strings.TrimRight(lines[line], "\r"))
	if char < len(before) {
		before = before[:char]
	}
	text := string(before)
	for l := line - 1; l >= 0 && strings.TrimSpace(text) == ""; l-- {
		text = lines[l] + "\n" + text
	}
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[len(fields)-1], word)
}
