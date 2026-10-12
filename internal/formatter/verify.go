package formatter

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// SkipError explains why a document was left unformatted.
type SkipError struct{ Reason string }

func (e *SkipError) Error() string { return "format skipped: " + e.Reason }

func skip(format string, a ...interface{}) error {
	return &SkipError{Reason: fmt.Sprintf(format, a...)}
}

// FormatVerified formats a whole document and only returns a result that is
// provably safe to apply:
//
//   - documents with constructs the formatter corrupts are skipped
//     (sqlsplit.FormatHazards);
//   - whitespace inside statements is collapsed first, so the result depends
//     only on the tokens (the raw formatter oscillates between layouts);
//   - the result must contain exactly the same tokens as the source: comments,
//     strings and quoted identifiers byte-for-byte, keywords and unquoted
//     identifiers case-insensitively, operators unsplit;
//   - formatting the result again must not change it (up to a few passes are
//     tried to reach such a fixed point, each one verified).
//
// It returns the complete new document text, or a *SkipError.
func FormatVerified(text string, params lsp.DocumentFormattingParams, cfg *config.Config) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}
	if hz := sqlsplit.FormatHazards(text); len(hz) > 0 {
		reason := fmt.Sprintf("%s on line %d", hz[0].What, hz[0].Line+1)
		if len(hz) > 1 {
			reason += fmt.Sprintf(" (+%d more)", len(hz)-1)
		}
		return "", skip("%s; the formatter would corrupt it", reason)
	}

	// Iterate to a fixed point: every pass must preserve the source's tokens,
	// and the result is accepted once formatting it again changes nothing.
	out, err := formatPass(text, params, cfg)
	for pass := 0; ; pass++ {
		if err != nil {
			return "", err
		}
		if line, ok := sameTokens(text, out); !ok {
			return "", skip("the formatter would drop or alter content near line %d", line+1)
		}
		if pass == maxFormatPasses {
			return "", skip("the formatter's output is not stable for this document")
		}
		var again string
		again, err = formatPass(out, params, cfg)
		if err == nil && again == out {
			return out, nil
		}
		out = again
	}
}

// formatPass formats text and puts back the source layout between statements
// (blank lines, comments trailing a ';'), which the formatter does not keep.
func formatPass(text string, params lsp.DocumentFormattingParams, cfg *config.Config) (string, error) {
	out, err := formatOnce(text, params, cfg)
	if err != nil {
		return "", err
	}
	return restoreGaps(out, text), nil
}

const maxFormatPasses = 3

// formatFn is the raw formatter (replaceable in tests).
var formatFn = Format

// formatOnce normalizes whitespace, runs the formatter (recovering from
// panics) and returns the complete resulting document.
func formatOnce(text string, params lsp.DocumentFormattingParams, cfg *config.Config) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = "", skip("the formatter cannot parse this document (%v)", r)
		}
	}()
	src := collapseWhitespace(text)
	edits, err := formatFn(src, params, cfg)
	if err != nil {
		return "", skip("%v", err)
	}
	if len(edits) == 0 {
		return src, nil
	}
	e := edits[0]
	start, ok1 := offsetOf(src, e.Range.Start)
	end, ok2 := offsetOf(src, e.Range.End)
	if !ok1 || !ok2 || start > end {
		return "", skip("the formatter returned an invalid range")
	}
	return src[:start] + e.NewText + src[end:], nil
}

// offsetOf converts an LSP position (UTF-16 columns) to a byte offset.
func offsetOf(text string, p lsp.Position) (int, bool) {
	line, off := 0, 0
	for line < p.Line {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			return 0, false
		}
		off += i + 1
		line++
	}
	col := 0
	for off < len(text) && col < p.Character {
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
	return off, col == p.Character
}

// FullRange returns the LSP range covering the whole document.
func FullRange(text string) lsp.Range {
	lines := strings.Split(text, "\n")
	last := lines[len(lines)-1]
	n := 0
	for _, r := range last {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return lsp.Range{End: lsp.Position{Line: len(lines) - 1, Character: n}}
}

// --- token-level comparison -------------------------------------------------

type vkind byte

const (
	vSpace vkind = iota
	vLineComment
	vBlockComment
	vString // '...'
	vQuoted // "..." or `...`
	vWord   // keywords, identifiers, numbers, params ($1, :x), dblinks (a@b)
	vOp     // run of operator characters, e.g. ->> :: || <>
	vPunct  // ( ) , ; [ ] { }
)

type vtoken struct {
	kind vkind
	text string
	line int
}

func isWordRune(r rune) bool {
	return r == '_' || r == '$' || r == '#' || r == '@' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

const opChars = "-+*/<>=!~^&|%:?@#"

// atOperator: '@' starting @> @? @@, and '#' starting #> #>>, are operators
// even right after a word (tags@>'{}', d#>>'{a}'), while a@link, @var and
// Oracle names like OBJ# stay words.
func atOperator(rs []rune, i int) bool {
	if i+1 >= len(rs) {
		return false
	}
	switch rs[i] {
	case '@':
		return rs[i+1] == '>' || rs[i+1] == '?' || rs[i+1] == '@'
	case '#':
		return rs[i+1] == '>'
	}
	return false
}

func vtokenize(s string) []vtoken {
	rs := []rune(s)
	var out []vtoken
	line := 0
	i := 0
	emit := func(k vkind, from, to int) {
		t := string(rs[from:to])
		out = append(out, vtoken{kind: k, text: t, line: line})
		line += strings.Count(t, "\n")
	}
	quoted := func(q rune) int { // returns end index (exclusive)
		j := i + 1
		for j < len(rs) {
			if rs[j] == q {
				if j+1 < len(rs) && rs[j+1] == q { // doubled quote
					j += 2
					continue
				}
				return j + 1
			}
			j++
		}
		return len(rs)
	}
	for i < len(rs) {
		r := rs[i]
		var j int
		switch {
		case unicode.IsSpace(r):
			j = i
			for j < len(rs) && unicode.IsSpace(rs[j]) {
				j++
			}
			emit(vSpace, i, j)
		case r == '-' && i+1 < len(rs) && rs[i+1] == '-':
			j = i
			for j < len(rs) && rs[j] != '\n' {
				j++
			}
			emit(vLineComment, i, j)
		case r == '/' && i+1 < len(rs) && rs[i+1] == '*':
			j = i + 2
			for j < len(rs) && !(rs[j] == '*' && j+1 < len(rs) && rs[j+1] == '/') {
				j++
			}
			j = min(j+2, len(rs))
			emit(vBlockComment, i, j)
		case r == '\'':
			j = quoted('\'')
			emit(vString, i, j)
		case r == '"' || r == '`':
			j = quoted(r)
			emit(vQuoted, i, j)
		case isWordRune(r) && !atOperator(rs, i):
			j = i
			for j < len(rs) && isWordRune(rs[j]) && !atOperator(rs, j) {
				j++
			}
			emit(vWord, i, j)
		case strings.ContainsRune(opChars, r):
			j = i
			for j < len(rs) && strings.ContainsRune(opChars, rs[j]) {
				// stop before a comment start
				if (rs[j] == '-' && j+1 < len(rs) && rs[j+1] == '-') || (rs[j] == '/' && j+1 < len(rs) && rs[j+1] == '*') {
					if j == i {
						j++ // unreachable: comments are handled above
					}
					break
				}
				j++
			}
			emit(vOp, i, j)
		default:
			j = i + 1
			emit(vPunct, i, j)
		}
		i = j
	}
	return out
}

func compareKey(t vtoken) string {
	switch t.kind {
	case vWord:
		return "w" + strings.ToLower(t.text)
	case vLineComment:
		return "c" + strings.TrimRightFunc(t.text, unicode.IsSpace)
	default:
		return string('0'+rune(t.kind)) + t.text
	}
}

// sameTokens reports whether a and b have the same significant tokens. On a
// mismatch it returns the 0-based line in a near the first difference.
func sameTokens(a, b string) (int, bool) {
	sig := func(s string) []vtoken {
		var out []vtoken
		for _, t := range vtokenize(s) {
			if t.kind != vSpace {
				out = append(out, t)
			}
		}
		return out
	}
	ta, tb := sig(a), sig(b)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		if i >= len(ta) || i >= len(tb) || compareKey(ta[i]) != compareKey(tb[i]) {
			if i < len(ta) {
				return ta[i].line, false
			}
			if len(ta) > 0 {
				return ta[len(ta)-1].line, false
			}
			return 0, false
		}
	}
	return 0, true
}

// collapseWhitespace rewrites the spacing inside statements so that it
// depends only on the token sequence, not on the source layout. Two documents
// with the same tokens then produce the same formatter output, which makes
// formatting stable. Kept as in the source: whitespace at the start/end of the
// document, between statements (after ';' and around comments sitting between
// statements), and adjacency where spacing can change meaning.
func collapseWhitespace(s string) string {
	toks := vtokenize(s)
	var b strings.Builder
	// index of the previous significant token and whether whitespace followed it
	prev, wsRun := -1, ""
	for i, t := range toks {
		if t.kind == vSpace {
			wsRun = t.text
			if i == len(toks)-1 { // trailing whitespace of the document
				b.WriteString(t.text)
			}
			continue
		}
		if prev < 0 {
			b.WriteString(wsRun) // leading whitespace
		} else {
			b.WriteString(separator(toks, prev, wsRun, t))
		}
		b.WriteString(t.text)
		prev, wsRun = i, ""
	}
	return b.String()
}

func separator(toks []vtoken, prev int, ws string, b vtoken) string {
	a := toks[prev]
	had := ws != ""
	keep := ""
	if had {
		keep = " "
	}
	isP := func(t vtoken, s string) bool { return t.kind == vPunct && t.text == s }
	switch {
	case a.kind == vLineComment:
		if strings.Contains(ws, "\n") && betweenStatements(toks, prev) {
			return ws
		}
		return "\n" // a line comment must end the line
	case isP(a, ";"):
		return ws // statement boundary: keep the user's layout
	case a.kind == vBlockComment && betweenStatements(toks, prev):
		return ws
	case a.kind == vBlockComment || b.kind == vLineComment || b.kind == vBlockComment:
		if b.kind == vLineComment && strings.Contains(ws, "\n") {
			return ws // comment on its own line: keep it there
		}
		return " "
	case isP(a, "(") || isP(b, ")") || isP(b, ",") || isP(b, ";"):
		return ""
	case isP(b, "(") && (a.kind == vWord || a.kind == vQuoted):
		// the formatter's parser reads "as(" as a function call (and then
		// drops a comma); never change whether a name touches its "("
		return keep
	case a.kind == vWord && (b.kind == vString || b.kind == vQuoted):
		return keep // e'..', N'..', x'..', U&"..": adjacency is significant
	case strings.HasSuffix(a.text, ".") || strings.HasPrefix(b.text, "."):
		return keep
	case a.kind == vOp || b.kind == vOp:
		return keep // unary minus, ::, ->> etc.
	default:
		return " "
	}
}

// betweenStatements reports whether token k only has comments between it and
// the previous ';' (or the document start).
func betweenStatements(toks []vtoken, k int) bool {
	for j := k - 1; j >= 0; j-- {
		switch toks[j].kind {
		case vSpace, vLineComment, vBlockComment:
			continue
		case vPunct:
			return toks[j].text == ";"
		default:
			return false
		}
	}
	return true
}

// layout splits a document into leading whitespace, significant tokens and
// the whitespace following each of them.
func layout(s string) (lead string, sig []vtoken, after []string) {
	for _, t := range vtokenize(s) {
		if t.kind == vSpace {
			if len(sig) == 0 {
				lead += t.text
			} else {
				after[len(after)-1] += t.text
			}
			continue
		}
		sig = append(sig, t)
		after = append(after, "")
	}
	return lead, sig, after
}

// isGap reports whether the whitespace after sig[k] lies between statements:
// sig[k] is a ';' or a comment preceded only by comments since the last ';'.
func isGap(sig []vtoken, k int) bool {
	if sig[k].kind == vPunct && sig[k].text == ";" {
		return true
	}
	if sig[k].kind != vLineComment && sig[k].kind != vBlockComment {
		return false
	}
	for j := k - 1; j >= 0; j-- {
		switch {
		case sig[j].kind == vLineComment || sig[j].kind == vBlockComment:
			continue
		case sig[j].kind == vPunct && sig[j].text == ";":
			return true
		default:
			return false
		}
	}
	return true
}

// restoreGaps rebuilds formatted with the reference's whitespace at the
// document start/end and between statements (newline structure only, so
// statements start at column 0). Both must have the same significant tokens;
// otherwise formatted is returned unchanged.
func restoreGaps(formatted, ref string) string {
	_, fSig, fAfter := layout(formatted)
	rLead, rSig, rAfter := layout(ref)
	if len(fSig) != len(rSig) {
		return formatted
	}
	for i := range fSig {
		if compareKey(fSig[i]) != compareKey(rSig[i]) {
			return formatted
		}
	}
	var b strings.Builder
	b.WriteString(rLead)
	for k, t := range fSig {
		b.WriteString(t.text)
		switch {
		case k == len(fSig)-1:
			b.WriteString(rAfter[k]) // trailing whitespace of the document
		case isGap(rSig, k) && rAfter[k] != "":
			if n := strings.Count(rAfter[k], "\n"); n > 0 {
				b.WriteString(strings.Repeat("\n", n))
			} else {
				b.WriteString(rAfter[k]) // e.g. "; -- note" on the same line
			}
		default:
			b.WriteString(fAfter[k])
		}
	}
	return b.String()
}
