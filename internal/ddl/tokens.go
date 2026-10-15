// Package ddl understands DDL statements (CREATE, ALTER, DROP, TRUNCATE,
// COMMENT, GRANT/REVOKE, RENAME) and the tails of DML statements the sqls
// parser doesn't follow (RETURNING, ON CONFLICT, MERGE): which objects a
// statement names, and what may come at a cursor position.
//
// It works on tokens with a small hand-written grammar per statement, not on
// the sqls AST: that AST treats every parenthesis as a list of columns and
// every "name(" as a function call, which is wrong for most of DDL.
//
// One walk serves completion (tokens up to the cursor, followed by a cursor
// marker: the walk stops there and reports a Slot), hover and lint (all
// tokens: the walk reports the Refs it read).
package ddl

import (
	"bytes"
	"strings"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/token"
)

// Tok is a significant token (no white space or comments).
type Tok struct {
	Kind   token.Kind
	Text   string // word value (unquoted), or the token's text
	Upper  string // upper-cased word; "" for other tokens
	Quoted bool
	From   token.Pos
	To     token.Pos
	cursor bool
}

// IsWord: an identifier or keyword (quoted or not).
func (t Tok) IsWord() bool { return t.Kind == token.SQLKeyword && !t.cursor }

// Is reports whether t is one of the (upper-case) keywords, unquoted.
func (t Tok) Is(kw ...string) bool {
	if !t.IsWord() || t.Quoted {
		return false
	}
	for _, k := range kw {
		if t.Upper == k {
			return true
		}
	}
	return false
}

func (t Tok) isCursor() bool { return t.cursor }

func tokenText(r *token.Token) string {
	switch v := r.Value.(type) {
	case *token.SQLWord:
		return v.Value
	case string:
		return v
	}
	return ""
}

// glueable: r continues the name last (a '$' or '#' glued to it, or a word
// or number glued to a name that ends with one).
func glueable(last Tok, r *token.Token) bool {
	if last.Kind != token.SQLKeyword || last.Quoted {
		return false
	}
	text := tokenText(r)
	if r.Kind == token.Char && (text == "$" || text == "#") {
		return true
	}
	if w, ok := r.Value.(*token.SQLWord); ok && w.QuoteStyle != 0 {
		return false
	}
	cont := r.Kind == token.SQLKeyword || r.Kind == token.Number
	return cont && (strings.HasSuffix(last.Text, "$") || strings.HasSuffix(last.Text, "#"))
}

// Tokenize lexes sql; ok is false when the lexer fails.
func Tokenize(sql string) ([]Tok, bool) {
	raw, err := token.NewTokenizer(bytes.NewBufferString(sql), &dialect.GenericSQLDialect{}).Tokenize()
	if err != nil {
		return nil, false
	}
	out := make([]Tok, 0, len(raw))
	for _, r := range raw {
		switch r.Kind {
		case token.Whitespace, token.Comment, token.MultilineComment:
			continue
		case token.ILLEGAL:
			return nil, false
		}
		t := Tok{Kind: r.Kind, From: r.From, To: r.To}
		// Oracle names with $ and # (V$SESSION, V500$ACT_GE): the lexer
		// splits them; glue the parts back.
		if n := len(out); n > 0 && out[n-1].From.Line == r.From.Line && out[n-1].To == r.From && glueable(out[n-1], r) {
			last := &out[n-1]
			last.Text += tokenText(r)
			last.Upper = strings.ToUpper(last.Text)
			last.To = r.To
			continue
		}
		switch v := r.Value.(type) {
		case *token.SQLWord:
			t.Text, t.Quoted = v.Value, v.QuoteStyle != 0
			t.Upper = strings.ToUpper(v.Value)
		case string:
			t.Text = v
			if r.Kind == token.SQLKeyword {
				t.Upper = strings.ToUpper(v)
			}
		}
		out = append(out, t)
	}
	return out, true
}

// withCursor appends the cursor marker.
func withCursor(toks []Tok) []Tok {
	var at token.Pos
	if n := len(toks); n > 0 {
		at = toks[n-1].To
	}
	return append(toks, Tok{Kind: token.ILLEGAL, cursor: true, From: at, To: at})
}
