package ddl

import (
	"github.com/raikhan/sqlh/token"
)

// dml: INSERT/UPDATE/DELETE/MERGE. Only the tails the sqls parser doesn't
// follow are completed here (RETURNING, ON CONFLICT, MERGE clauses); every
// other position is handed off to the regular completer.
func (p *parser) dml() {
	p.a.DML = true
	first := p.next()
	p.a.Kind = first.Upper
	switch first.Upper {
	case "INSERT":
		if p.atCursor() {
			p.handoff()
		}
		if !p.accept("INTO") {
			p.give() // Oracle INSERT ALL/FIRST
		}
		p.dmlTail("INSERT", p.dmlTarget())
	case "UPDATE":
		if p.atCursor() {
			p.handoff()
		}
		p.accept("ONLY")
		p.dmlTail("UPDATE", p.dmlTarget())
	case "DELETE":
		if p.atCursor() {
			p.handoff()
		}
		p.accept("FROM")
		p.dmlTail("DELETE", p.dmlTarget())
	case "MERGE":
		p.merge()
	}
}

// dmlTarget reads the target table; the cursor there is the regular
// completer's (it offers tables and schemas already).
func (p *parser) dmlTarget() Name {
	if p.atCursor() {
		p.handoff()
	}
	if p.is("ONLY") {
		p.i++
	}
	if p.atCursor() {
		p.handoff()
	}
	t := p.cur()
	if !t.IsWord() {
		p.give()
	}
	p.i++
	n := Name{Name: t.Text, Quoted: t.Quoted, From: t.From, To: t.To}
	if p.i < len(p.t) && p.t[p.i].Kind == token.Period {
		p.i++
		if p.atCursor() {
			p.handoff()
		}
		u := p.cur()
		if !u.IsWord() {
			p.give()
		}
		p.i++
		n = Name{Schema: t.Text, Name: u.Text, Quoted: u.Quoted, From: t.From, To: u.To}
	}
	p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: n, Role: RoleUse})
	return n
}

// skipParenHandoff skips a parenthesised group; the cursor inside belongs
// to the regular completer.
func (p *parser) skipParenHandoff() {
	depth := 0
	for {
		if p.atCursor() {
			p.handoff()
		}
		t := p.cur()
		p.i++
		switch t.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

// valueWords: words that are values, not columns.
var valueWords = []string{
	"NULL", "TRUE", "FALSE", "DEFAULT", "CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME",
	"LOCALTIMESTAMP", "LOCALTIME", "SYSDATE", "SYSTIMESTAMP", "USER", "CURRENT_USER", "SESSION_USER",
	"CASE", "NOT", "CAST",
}

func isOperatorTok(t Tok) bool {
	switch t.Kind {
	case token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq, token.Plus, token.Minus,
		token.Mult, token.Div, token.Mod, token.Caret, token.Operator, token.DoubleColon:
		return true
	}
	return t.Is(exprWords...)
}

// dmlTail walks an INSERT/UPDATE/DELETE after its target.
func (p *parser) dmlTail(kind string, target Name) {
	tbl := target
	cols := Slot{Kinds: []ObjKind{ObjColumn}, Table: &tbl}
	state := ""
	for {
		if p.atCursor() {
			p.dmlSlot(state, cols)
		}
		if p.atEnd() {
			p.stop()
		}
		t := p.t[p.i]
		prev := p.prev()
		switch {
		case t.Kind == token.LParen:
			switch state {
			case "conflict":
				p.i++
				for {
					c := p.listItem(cols)
					p.colRef(tbl, c, RoleUse, false)
					p.at(Slot{})
					if p.acceptKind(token.Comma) {
						continue
					}
					p.expectKind(token.RParen, Slot{})
					break
				}
				state = "conflictTarget"
			case "values":
				p.skipParenHandoff()
				state = "valuesDone"
			default:
				p.skipParenHandoff()
			}
			continue
		case t.Is("RETURNING", "RETURN"):
			state = "returning"
		case t.Is("VALUES") && kind == "INSERT":
			state = "values"
		case t.Kind == token.Comma && state == "valuesDone":
			state = "values"
		case t.Is("ON") && kind == "INSERT" && state != "conflict":
			p.i++
			p.at(p.kwSlot("CONFLICT"))
			if !p.accept("CONFLICT") {
				p.give()
			}
			state = "conflict"
			continue
		case t.Is("ON") && state == "conflict":
			p.i++
			p.at(p.kwSlot("CONSTRAINT"))
			if !p.accept("CONSTRAINT") {
				p.give()
			}
			n := p.word(p.constraintSlot(tbl))
			p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &tbl, Role: RoleUse})
			state = "conflictTarget"
			continue
		case t.Is("DO") && (state == "conflict" || state == "conflictTarget"):
			state = "do"
		case t.Is("NOTHING") && state == "do":
			state = "end"
		case t.Is("UPDATE") && state == "do":
			state = "doUpdate"
		case t.Is("SET") && state == "doUpdate":
			state = "set"
		case t.Is("WHERE") && state == "conflictTarget":
			state = "conflictWhere"
		case t.Is("WHERE") && state == "set":
			state = "setWhere"
		case t.Is("SELECT", "WITH") && (state == "" || state == "values"):
			state = "query"
		case t.Is("EXCLUDED") && p.i+1 < len(p.t) && p.t[p.i+1].Kind == token.Period:
			p.i += 2
			c := p.word(cols)
			p.colRef(tbl, c, RoleUse, false)
			continue
		case state == "returning" && t.IsWord() && (prev.Is("RETURNING", "RETURN") || prev.Kind == token.Comma) && !t.Is(valueWords...):
			if n, ok := p.peek(1); !ok || (n.Kind != token.LParen && n.Kind != token.Period) {
				p.colRef(tbl, Name{Name: t.Text, Quoted: t.Quoted, From: t.From, To: t.To}, RoleUse, false)
			}
		case state == "set" && t.IsWord() && (prev.Is("SET") || prev.Kind == token.Comma):
			if n, ok := p.peek(1); ok && n.Kind == token.Eq {
				p.colRef(tbl, Name{Name: t.Text, Quoted: t.Quoted, From: t.From, To: t.To}, RoleUse, false)
			}
		}
		p.i++
	}
}

// dmlSlot: what fits at the cursor in a DML tail (stops; hands off when the
// position isn't a tail).
func (p *parser) dmlSlot(state string, cols Slot) {
	prev := p.prev()
	switch state {
	case "returning":
		if prev.Is("RETURNING", "RETURN") || prev.Kind == token.Comma {
			p.at(merge(cols, Slot{Functions: true}))
		}
		if prev.IsWord() || prev.Kind == token.RParen {
			p.at(p.kwSlot("INTO@o", "AS@psg"))
		}
	case "conflict":
		p.at(p.kwSlot(afterOnConflict...))
	case "conflictTarget":
		p.at(p.kwSlot(afterConflictTarget...))
	case "do":
		p.at(p.kwSlot(afterDo...))
	case "doUpdate":
		p.at(p.kwSlot("SET"))
	case "set":
		switch {
		case prev.Is("SET") || prev.Kind == token.Comma:
			p.at(cols)
		case isOperatorTok(prev):
			p.at(merge(cols, Slot{Excluded: true, Functions: true}, p.kwSlot(exprOperands...)))
		default:
			p.at(merge(p.kwSlot(afterSetList...), Slot{Keywords: exprOperators}))
		}
	case "valuesDone":
		p.at(p.kwSlot(afterValues...))
	case "end":
		p.at(p.kwSlot("RETURNING@ps"))
	}
	p.handoff()
}

// merge: MERGE INTO target [alias] USING source [alias] ON (…) WHEN … THEN …
func (p *parser) merge() {
	p.at(p.kwSlot("INTO"))
	if !p.accept("INTO") {
		p.give()
	}
	target := p.objName(Slot{Kinds: Relations})
	p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: target, Role: RoleUse})
	tbl := target
	cols := Slot{Kinds: []ObjKind{ObjColumn}, Table: &tbl}
	p.at(p.kwSlot("USING", "AS@p")) // or an alias
	p.alias("USING")
	p.at(p.kwSlot("USING"))
	if !p.accept("USING") {
		p.give()
	}
	p.at(Slot{Kinds: Relations})
	if p.isKind(token.LParen) {
		p.skipParenHandoff()
	} else {
		src := p.objName(Slot{Kinds: Relations})
		p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: src, Role: RoleUse})
	}
	p.at(p.kwSlot("ON"))
	p.alias("ON")
	p.at(p.kwSlot("ON"))
	if !p.accept("ON") {
		p.give()
	}
	// the join condition: the regular completer's
	for {
		if p.atCursor() {
			if p.prev().Kind == token.RParen {
				p.at(p.kwSlot(mergeWhen...))
			}
			p.handoff()
		}
		if p.is("WHEN") {
			break
		}
		if p.isKind(token.LParen) {
			p.skipParenHandoff()
			continue
		}
		p.next()
	}
	for {
		p.at(p.kwSlot(mergeWhen...))
		if !p.accept("WHEN") {
			p.give()
		}
		p.at(p.kwSlot("MATCHED", "NOT MATCHED"))
		p.accept("NOT")
		p.at(p.kwSlot("MATCHED"))
		if !p.accept("MATCHED") {
			p.give()
		}
		for !p.is("THEN") {
			if p.atCursor() {
				if p.prev().Is("MATCHED", "SOURCE", "TARGET") {
					p.at(p.kwSlot("THEN", "AND", "BY SOURCE@p", "BY TARGET@p"))
				}
				p.handoff()
			}
			if p.isKind(token.LParen) {
				p.skipParenHandoff()
				continue
			}
			p.next()
		}
		p.i++ // THEN
		p.at(p.kwSlot(mergeThen...))
		switch {
		case p.accept("UPDATE"):
			p.at(p.kwSlot("SET"))
			if !p.accept("SET") {
				p.give()
			}
			p.mergeAssignments(tbl, cols)
		case p.accept("INSERT"):
			p.at(p.kwSlot("VALUES"))
			if p.isKind(token.LParen) {
				p.i++
				for {
					c := p.listItem(cols)
					p.colRef(tbl, c, RoleUse, false)
					p.at(Slot{})
					if p.acceptKind(token.Comma) {
						continue
					}
					p.expectKind(token.RParen, Slot{})
					break
				}
			}
			p.at(p.kwSlot("VALUES"))
			p.accept("VALUES")
			p.skipToWhen()
		case p.accept("DELETE"):
			p.skipToWhen()
		case p.seq("DO", "NOTHING"):
		default:
			p.give()
		}
	}
}

// alias skips an optional alias before the keyword next.
func (p *parser) alias(next string) {
	if p.is(next) {
		return
	}
	if p.accept("AS") {
		p.at(Slot{})
	}
	if t := p.cur(); t.IsWord() && !t.Is(next) {
		p.i++
	}
}

// mergeAssignments: col = expr, … (values are the regular completer's).
func (p *parser) mergeAssignments(tbl Name, cols Slot) {
	for {
		c := p.word(cols)
		if p.i < len(p.t) && p.t[p.i].Kind == token.Period { // alias.col
			p.i++
			c = p.word(cols)
		}
		p.colRef(tbl, c, RoleUse, false)
		p.at(Slot{})
		if !p.acceptKind(token.Eq) {
			p.give()
		}
		depth := 0
		for {
			if p.atCursor() {
				if depth == 0 && isOperand(p.prev()) && !isOperatorTok(p.prev()) {
					p.at(merge(p.kwSlot(mergeWhen...), p.kwSlot("WHERE@o", "DELETE WHERE@o")))
				}
				p.handoff()
			}
			if p.atEnd() {
				p.stop()
			}
			t := p.t[p.i]
			if depth == 0 && (t.Kind == token.Comma || t.Is("WHEN", "WHERE", "DELETE")) {
				break
			}
			switch t.Kind {
			case token.LParen:
				depth++
			case token.RParen:
				depth--
			}
			p.i++
		}
		if p.acceptKind(token.Comma) {
			continue
		}
		p.skipToWhen()
		return
	}
}

// skipToWhen skips to the next WHEN clause (the cursor on the way is the
// regular completer's).
func (p *parser) skipToWhen() {
	for {
		if p.atCursor() {
			if p.prev().Kind == token.RParen {
				p.at(p.kwSlot(mergeWhen...))
			}
			p.handoff()
		}
		if p.atEnd() {
			p.stop()
		}
		if p.is("WHEN") {
			return
		}
		if p.isKind(token.LParen) {
			p.skipParenHandoff()
			continue
		}
		p.i++
	}
}
