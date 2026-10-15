package ddl

import (
	"strings"

	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/token"
)

// Analyze walks a statement's tokens. With a cursor marker (AtCursor) the
// walk stops there and reports the Slot.
func Analyze(toks []Tok, d sqlsplit.Dialect) *Analysis {
	a := &Analysis{Body: -1}
	if len(toks) == 0 {
		return a
	}
	p := &parser{t: toks, d: d, a: a}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(stopWalk); !ok {
					panic(r)
				}
			}
		}()
		p.statement()
		// a statement that goes on after what the grammar knows
		if p.i < len(p.t) && !p.t[p.i].cursor {
			a.Unsure = true
		}
	}()
	return a
}

type stopWalk struct{}

type parser struct {
	t []Tok
	i int
	d sqlsplit.Dialect
	a *Analysis
}

func (p *parser) stop() { panic(stopWalk{}) }

// give stops at something the grammar doesn't expect.
func (p *parser) give() {
	p.a.Unsure = true
	p.stop()
}

func (p *parser) atCursor() bool { return p.i < len(p.t) && p.t[p.i].cursor }
func (p *parser) atEnd() bool    { return p.i >= len(p.t) }

// at declares what fits at the current position: at the cursor the walk
// stops with that slot; at the end of the statement it just stops.
func (p *parser) at(s Slot) {
	if p.atCursor() {
		p.a.Slot = &s
		p.stop()
	}
	if p.atEnd() {
		p.stop()
	}
}

// cur is the current token. At the cursor or the end the walk stops (with
// no slot unless at() declared one).
func (p *parser) cur() Tok {
	if p.atEnd() || p.atCursor() {
		p.stop()
	}
	return p.t[p.i]
}

// peek looks k tokens ahead without stopping; ok false at the cursor or end.
func (p *parser) peek(k int) (Tok, bool) {
	j := p.i + k
	if j < 0 || j >= len(p.t) || p.t[j].cursor {
		return Tok{}, false
	}
	return p.t[j], true
}

func (p *parser) prev() Tok {
	if p.i == 0 || p.i > len(p.t) {
		return Tok{Kind: token.ILLEGAL}
	}
	return p.t[p.i-1]
}

func (p *parser) next() Tok {
	t := p.cur()
	p.i++
	return t
}

// is reports whether the current token is one of the keywords (no stop).
func (p *parser) is(kw ...string) bool {
	t, ok := p.peek(0)
	return ok && t.Is(kw...)
}

func (p *parser) isKind(k token.Kind) bool {
	t, ok := p.peek(0)
	return ok && t.Kind == k
}

// accept consumes the current token if it is one of the keywords.
func (p *parser) accept(kw ...string) bool {
	if p.cur().Is(kw...) {
		p.i++
		return true
	}
	return false
}

func (p *parser) acceptKind(k token.Kind) bool {
	if p.cur().Kind == k {
		p.i++
		return true
	}
	return false
}

// seq consumes a keyword sequence if its first word is current; the rest
// must follow (each offered at the cursor).
func (p *parser) seq(words ...string) bool {
	if !p.cur().Is(words[0]) {
		return false
	}
	p.i++
	for j := 1; j < len(words); j++ {
		p.at(Slot{Keywords: []string{strings.Join(words[j:], " ")}})
		if !p.cur().Is(words[j]) {
			p.give()
		}
		p.i++
	}
	return true
}

// expectKind requires a token kind (offering s before it).
func (p *parser) expectKind(k token.Kind, s Slot) {
	p.at(s)
	if !p.acceptKind(k) {
		p.give()
	}
}

func (p *parser) ref(r Ref) {
	if len(r.Kinds) == 0 {
		r.Kinds = []ObjKind{r.Kind}
	}
	if r.Kind == 0 {
		r.Kind = r.Kinds[0]
	}
	p.a.Refs = append(p.a.Refs, r)
}

// ifExists reads IF EXISTS (offering s, which should list it, first).
func (p *parser) ifExists(s Slot) bool {
	p.at(s)
	return p.seq("IF", "EXISTS")
}

func (p *parser) ifNotExists(s Slot) bool {
	p.at(s)
	return p.seq("IF", "NOT", "EXISTS")
}

// objName reads [schema.]name of an existing object; s is offered at its
// start, the kinds of s after "schema.".
func (p *parser) objName(s Slot) Name {
	p.at(s)
	first := p.cur()
	if !first.IsWord() {
		p.give()
	}
	p.i++
	n := Name{Name: first.Text, Quoted: first.Quoted, From: first.From, To: first.To}
	if p.i < len(p.t) && p.t[p.i].Kind == token.Period {
		p.i++
		p.at(Slot{Kinds: s.Kinds, Qualifier: first.Text})
		second := p.cur()
		if !second.IsWord() {
			p.give()
		}
		p.i++
		n = Name{Schema: first.Text, Name: second.Text, Quoted: second.Quoted, From: first.From, To: second.To}
	}
	return n
}

// newName reads [schema.]name of an object being created; s is offered at
// its start (keywords only: names are new).
func (p *parser) newName(s Slot) Name {
	p.at(s)
	first := p.cur()
	if !first.IsWord() {
		p.give()
	}
	p.i++
	n := Name{Name: first.Text, Quoted: first.Quoted, From: first.From, To: first.To}
	if p.i < len(p.t) && p.t[p.i].Kind == token.Period {
		p.i++
		p.at(Slot{})
		second := p.cur()
		if !second.IsWord() {
			p.give()
		}
		p.i++
		n = Name{Schema: first.Text, Name: second.Text, Quoted: second.Quoted, From: first.From, To: second.To}
	}
	return n
}

// word reads a single identifier (a column, constraint, …), offering s.
func (p *parser) word(s Slot) Name {
	p.at(s)
	t := p.cur()
	if !t.IsWord() {
		p.give()
	}
	p.i++
	return Name{Name: t.Text, Quoted: t.Quoted, From: t.From, To: t.To}
}

// listItem reads a name in a parenthesised list: it must be followed by ','
// or ')' (or one of the follow keywords), else the list holds expressions
// the walk doesn't follow and it gives up before the name is used.
func (p *parser) listItem(s Slot, follow ...string) Name {
	n := p.word(s)
	if t, ok := p.peek(0); ok && t.Kind != token.Comma && t.Kind != token.RParen && !t.Is(follow...) {
		p.give()
	}
	return n
}

// skipParen consumes a parenthesised group starting at the current '(',
// offering in inside it.
func (p *parser) skipParen(in Slot) {
	if p.cur().Kind != token.LParen {
		p.give()
	}
	p.i++
	depth := 1
	for depth > 0 {
		p.at(in)
		switch p.next().Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
	}
}

// expr consumes an expression up to a ',' or ')' at its level, or a word for
// which end returns true. in is offered where an operand fits, follow after
// a complete operand at the expression's own level.
func (p *parser) expr(in, follow Slot, end func(Tok) bool) {
	depth := 0
	start := p.i
	for {
		if p.atCursor() {
			if depth == 0 && p.i > start && isOperand(p.prev()) && !p.prev().Is(exprWords...) {
				p.at(merge(follow, Slot{Keywords: exprOperators}))
			}
			p.at(in)
		}
		if p.atEnd() {
			p.stop()
		}
		t := p.t[p.i]
		switch t.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			if depth == 0 {
				return
			}
			depth--
		case token.Comma:
			if depth == 0 {
				return
			}
		case token.SQLKeyword:
			if depth == 0 && end != nil && end(t) {
				return
			}
		}
		p.i++
	}
}

func isOperand(t Tok) bool {
	switch t.Kind {
	case token.SQLKeyword, token.Number, token.SingleQuotedString, token.NationalStringLiteral, token.RParen:
		return true
	}
	return false
}

// merge combines slots (keywords concatenated, flags or-ed).
func merge(slots ...Slot) Slot {
	var out Slot
	for _, s := range slots {
		out.Kinds = append(out.Kinds, s.Kinds...)
		if s.Qualifier != "" {
			out.Qualifier = s.Qualifier
		}
		if s.Table != nil {
			out.Table = s.Table
		}
		out.Defined = out.Defined || s.Defined
		out.Types = out.Types || s.Types
		out.Functions = out.Functions || s.Functions
		out.Excluded = out.Excluded || s.Excluded
		out.PreferKeys = out.PreferKeys || s.PreferKeys
		if s.Guess != "" {
			out.Guess = s.Guess
		}
		out.Keywords = append(out.Keywords, s.Keywords...)
	}
	return out
}

// skipTo consumes tokens up to (not including) a ',' at depth 0, or the end;
// s is offered everywhere on the way.
func (p *parser) skipTo(s Slot, stopAt ...token.Kind) {
	depth := 0
	for {
		p.at(s)
		t := p.t[p.i]
		switch t.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			if depth == 0 {
				for _, k := range stopAt {
					if k == token.RParen {
						return
					}
				}
				p.give()
			}
			depth--
		case token.Comma:
			if depth == 0 {
				return
			}
		}
		p.i++
	}
}
