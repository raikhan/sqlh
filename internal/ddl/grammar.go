package ddl

import (
	"strings"

	"github.com/raikhan/sqlh/internal/sqlsplit"
	"github.com/raikhan/sqlh/token"
)

func (p *parser) statement() {
	if p.atCursor() {
		return // nothing typed yet: the regular completer offers statements
	}
	t := p.cur()
	switch {
	case t.Is("CREATE"):
		p.a.Kind = "CREATE"
		p.i++
		p.create()
	case t.Is("ALTER"):
		p.a.Kind = "ALTER"
		p.i++
		p.alter()
	case t.Is("DROP"):
		p.a.Kind = "DROP"
		p.i++
		p.drop()
	case t.Is("TRUNCATE"):
		p.a.Kind = "TRUNCATE"
		p.i++
		p.truncate()
	case t.Is("COMMENT"):
		p.a.Kind = "COMMENT"
		p.i++
		p.comment()
	case t.Is("GRANT", "REVOKE"):
		p.a.Kind = t.Upper
		p.i++
		p.grant(t.Upper == "GRANT")
	case t.Is("RENAME") && p.d == sqlsplit.Oracle:
		p.a.Kind = "RENAME"
		p.i++
		p.rename()
	case t.Is("INSERT", "UPDATE", "DELETE", "MERGE"):
		p.dml()
	case t.Is("WITH"):
		// WITH … INSERT/UPDATE/DELETE/MERGE: the tails of the statement
		depth := 0
		for j := p.i + 1; j < len(p.t); j++ {
			switch {
			case p.t[j].cursor:
				p.a.Kind, p.a.DML = "WITH", true
				p.handoff()
			case p.t[j].Kind == token.LParen:
				depth++
			case p.t[j].Kind == token.RParen:
				depth--
			case depth == 0 && p.t[j].Is("INSERT", "UPDATE", "DELETE", "MERGE") && p.t[j-1].Kind == token.RParen:
				p.i = j
				p.dml()
				return
			}
		}
	}
}

// handoff: the cursor is in a part the regular completer handles.
func (p *parser) handoff() {
	p.a.Handoff = true
	p.stop()
}

// body: the query of CREATE VIEW/TABLE … AS, left to the regular completer
// (and linted as a query).
func (p *parser) body() {
	p.at(p.kwSlot("SELECT", "WITH", "VALUES@ps"))
	p.a.Body = p.i
	for j := p.i; j < len(p.t); j++ {
		if p.t[j].cursor {
			p.handoff()
		}
	}
	p.i = len(p.t)
	p.stop()
}

// ---- CREATE ---------------------------------------------------------------

func (p *parser) create() {
	p.at(p.kwSlot(createKinds...))
	if p.seq("OR", "REPLACE") {
		p.at(p.kwSlot(afterOrReplace...))
	}
	materialized := false
	for {
		t := p.cur()
		next, ok := afterModifier[t.Upper]
		if !ok || t.Quoted {
			break
		}
		p.i++
		materialized = materialized || t.Upper == "MATERIALIZED"
		p.at(p.kwSlot(next...))
	}
	switch {
	case p.accept("TABLE"):
		p.a.Kind = "CREATE TABLE"
		p.createTable()
	case p.accept("VIEW"):
		p.a.Kind = "CREATE VIEW"
		if materialized {
			p.a.Kind = "CREATE MATERIALIZED VIEW"
		}
		p.createView(materialized)
	case p.accept("INDEX"):
		p.a.Kind = "CREATE INDEX"
		p.createIndex()
	case p.accept("TRIGGER"):
		p.a.Kind = "CREATE TRIGGER"
		p.createTrigger()
	case p.accept("SEQUENCE"):
		p.a.Kind = "CREATE SEQUENCE"
		p.createSequence()
	case p.accept("SYNONYM"):
		p.a.Kind = "CREATE SYNONYM"
		p.createSynonym()
	case p.accept("TYPE", "DOMAIN"):
		p.a.Kind = "CREATE " + p.prev().Upper
		ine := p.ifNotExists(Slot{})
		n := p.newName(Slot{})
		p.ref(Ref{Kind: ObjType, Name: n, Role: RoleCreate, IfExists: ine})
		p.stop()
	case p.accept("SCHEMA"):
		p.a.Kind = "CREATE SCHEMA"
		ine := p.ifNotExists(p.kwSlot("IF NOT EXISTS@pg"))
		n := p.newName(Slot{})
		p.ref(Ref{Kind: ObjSchema, Name: n, Role: RoleCreate, IfExists: ine})
		p.stop()
	default:
		// functions, procedures, packages, extensions, …: not followed here
		p.give()
	}
}

func (p *parser) createTable() {
	s := p.kwSlot("IF NOT EXISTS@psg")
	ine := p.ifNotExists(s)
	if ine {
		s = Slot{}
	}
	name := p.newName(s)
	p.ref(Ref{Kind: ObjTable, Name: name, Role: RoleCreate, IfExists: ine})
	p.at(p.kwSlot("AS", "PARTITION OF@p", "OF@p", "USING@s"))
	switch {
	case p.accept("AS"):
		p.body()
	case p.seq("PARTITION", "OF"):
		p.a.HasLike = true
		parent := p.objName(Slot{Kinds: []ObjKind{ObjTable}})
		p.ref(Ref{Kind: ObjTable, Name: parent, Role: RoleUse})
		p.stop()
	case p.isKind(token.LParen):
		p.tableElements(name)
		p.tableTail()
	default:
		p.give() // OF type, USING module (virtual tables), …
	}
}

func isTableConstraintStart(t Tok) bool {
	return t.Is("CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "EXCLUDE")
}

// tableElements: ( column definitions and table constraints ).
func (p *parser) tableElements(table Name) {
	p.i++ // (
	for {
		p.at(p.kwSlot(tableConstraintStarts...)) // or a new column's name
		t := p.cur()
		switch {
		case isTableConstraintStart(t):
			p.tableConstraint(table, true)
		case t.Is("LIKE"):
			p.i++
			p.a.HasLike = true
			src := p.objName(Slot{Kinds: Relations})
			p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: src, Role: RoleUse})
			p.skipTo(p.kwSlot("INCLUDING ALL", "INCLUDING DEFAULTS", "INCLUDING CONSTRAINTS", "INCLUDING INDEXES", "EXCLUDING ALL"), token.RParen)
		case t.IsWord():
			p.columnDef(table, true, false)
		default:
			p.give()
		}
		p.at(Slot{})
		switch p.cur().Kind {
		case token.Comma:
			p.i++
		case token.RParen:
			p.i++
			return
		default:
			p.give()
		}
	}
}

func (p *parser) tableTail() {
	for {
		p.at(p.kwSlot(tableTail...))
		switch {
		case p.accept("INHERITS"):
			p.a.HasLike = true
			p.expectKind(token.LParen, Slot{})
			for {
				n := p.objName(Slot{Kinds: []ObjKind{ObjTable}})
				p.ref(Ref{Kind: ObjTable, Name: n, Role: RoleUse})
				p.at(Slot{})
				if p.acceptKind(token.Comma) {
					continue
				}
				p.expectKind(token.RParen, Slot{})
				break
			}
		case p.accept("AS"):
			p.body()
		case p.isKind(token.LParen):
			p.skipParen(Slot{})
		default:
			p.i++
		}
	}
}

// columnDef: name [type] [constraints…] (in CREATE TABLE or ALTER TABLE ADD).
func (p *parser) columnDef(table Name, create, ifNotExists bool) {
	col := p.word(Slot{})
	if create {
		p.a.Defined = append(p.a.Defined, col)
	} else {
		p.ref(Ref{Kind: ObjColumn, Name: col, Table: &table, Role: RoleCreate, IfExists: ifNotExists})
	}
	p.at(Slot{Types: true})
	cont := p.dataType()
	p.columnConstraints(table, create, cont)
}

// typeContinuations: words that may follow a type's first word.
var typeContinuations = map[string][]string{
	"DOUBLE":    {"PRECISION"},
	"CHARACTER": {"VARYING"},
	"CHAR":      {"VARYING@pg"},
	"NCHAR":     {"VARYING"},
	"NATIONAL":  {"CHARACTER", "CHARACTER VARYING", "CHAR"},
	"BIT":       {"VARYING@p"},
	"LONG":      {"RAW@o"},
	"TIMESTAMP": {"WITH TIME ZONE@pog", "WITHOUT TIME ZONE@pg", "WITH LOCAL TIME ZONE@o"},
	"TIME":      {"WITH TIME ZONE@pg", "WITHOUT TIME ZONE@pg"},
	"INTERVAL":  {"YEAR TO MONTH@pog", "DAY TO SECOND@pog"},
}

// typeArgSlot: what fits inside a type's parentheses.
func (p *parser) typeArgSlot(first string) Slot {
	if p.d == sqlsplit.Oracle && (first == "VARCHAR2" || first == "CHAR" || first == "VARCHAR") {
		return p.kwSlot("CHAR", "BYTE")
	}
	return Slot{}
}

var columnConstraintWords = []string{
	"CONSTRAINT", "NOT", "NULL", "DEFAULT", "PRIMARY", "UNIQUE", "CHECK", "REFERENCES",
	"COLLATE", "GENERATED", "AUTOINCREMENT", "IDENTITY",
}

// dataType reads a column type; it returns the continuations still possible
// right after it (TIMESTAMP → WITH TIME ZONE, …).
func (p *parser) dataType() []string {
	t := p.cur()
	if !t.IsWord() || (!t.Quoted && t.Is(columnConstraintWords...)) {
		return nil // SQLite: a column without a type
	}
	p.i++
	if p.i < len(p.t) && p.t[p.i].Kind == token.Period { // schema.type
		p.i++
		p.at(Slot{Types: true})
		if !p.cur().IsWord() {
			p.give()
		}
		p.i++
		return nil
	}
	first := t.Upper
	args := func() {
		if p.isKind(token.LParen) {
			p.skipParen(p.typeArgSlot(first))
		}
	}
	args()
	conts := p.kw(typeContinuations[first]...)
	if len(conts) == 0 || t.Quoted {
		p.arrayBrackets()
		return nil
	}
	// consume a continuation, word by word
	consumed := 0
	var got []string
	for {
		var rest []string
		complete := false
		for _, c := range conts {
			words := strings.Fields(c)
			if consumed > len(words) || strings.Join(words[:consumed], " ") != strings.Join(got, " ") {
				continue
			}
			if len(words) > consumed {
				rest = append(rest, strings.Join(words[consumed:], " "))
			} else if consumed > 0 {
				complete = true
			}
		}
		if complete || len(rest) == 0 {
			break
		}
		if consumed > 0 {
			p.at(Slot{Keywords: rest})
		}
		cur, ok := p.peek(0)
		if !ok {
			if consumed == 0 {
				break // offered by the caller with the constraints
			}
			p.stop()
		}
		match := false
		for _, r := range rest {
			if cur.Is(strings.Fields(r)[0]) {
				match = true
				break
			}
		}
		if !match {
			if consumed > 0 {
				p.give()
			}
			break
		}
		got = append(got, cur.Upper)
		p.i++
		consumed++
		args()
	}
	p.arrayBrackets()
	if consumed > 0 {
		return nil
	}
	return conts
}

// arrayBrackets: PostgreSQL int[] / int[3].
func (p *parser) arrayBrackets() {
	for p.isKind(token.LBracket) {
		p.i++
		for !p.isKind(token.RBracket) {
			p.at(Slot{})
			p.i++
		}
		p.i++
	}
}

// columnConstraints reads constraints up to the ',' or ')' ending a column
// definition (or the end of an ALTER TABLE action).
func (p *parser) columnConstraints(table Name, create bool, cont []string) {
	follow := append(p.kw(columnConstraints...), cont...)
	checkSlot := p.exprSlot(table, create)
	for {
		p.at(Slot{Keywords: follow})
		follow = p.kw(columnConstraints...)
		t := p.cur()
		if t.Kind == token.Comma || t.Kind == token.RParen {
			return
		}
		switch {
		case p.accept("CONSTRAINT"):
			n := p.word(Slot{})
			p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &table, Role: RoleCreate})
			p.at(p.kwSlot(columnConstraintKinds...))
		case p.accept("NOT"):
			p.at(p.kwSlot("NULL", "DEFERRABLE@pos"))
			if !p.accept("NULL", "DEFERRABLE") {
				p.give()
			}
		case p.accept("NULL", "UNIQUE", "AUTOINCREMENT", "ASC", "DESC", "ENABLE", "DISABLE",
			"VALIDATE", "NOVALIDATE", "DEFERRABLE", "VISIBLE", "INVISIBLE", "IDENTITY", "RELY",
			"NORELY", "STORED", "VIRTUAL"):
		case p.accept("INITIALLY"):
			p.at(p.kwSlot("DEFERRED", "IMMEDIATE"))
			p.i++
		case p.accept("DEFAULT"):
			if p.is("ON") { // Oracle DEFAULT ON NULL
				p.i++
				p.at(p.kwSlot("NULL"))
				p.accept("NULL")
			}
			p.expr(merge(Slot{Functions: true}, p.kwSlot(defaultValues...)), Slot{Keywords: follow}, isColumnConstraintWord)
		case p.seq("PRIMARY", "KEY"):
			follow = append(follow, p.kw("AUTOINCREMENT@s", "ASC@s", "DESC@s")...)
		case p.accept("CHECK"):
			p.expectKind(token.LParen, Slot{})
			p.i--
			p.skipParen(checkSlot)
		case p.accept("REFERENCES"):
			col := ""
			if create && len(p.a.Defined) > 0 {
				col = p.a.Defined[len(p.a.Defined)-1].Name
			} else if !create {
				for k := len(p.a.Refs) - 1; k >= 0; k-- {
					if p.a.Refs[k].Kind == ObjColumn {
						col = p.a.Refs[k].Name.Name
						break
					}
				}
			}
			p.references(col, follow)
		case p.accept("GENERATED"):
			p.generated(checkSlot)
		case p.accept("COLLATE"):
			p.word(Slot{})
		case p.accept("ON"): // SQLite: ON CONFLICT …
			p.at(p.kwSlot("CONFLICT"))
			p.accept("CONFLICT")
			p.at(p.kwSlot("ROLLBACK", "ABORT", "FAIL", "IGNORE", "REPLACE"))
			p.i++
		case t.Kind == token.LParen:
			p.skipParen(Slot{})
		default:
			p.i++
		}
	}
}

func isColumnConstraintWord(t Tok) bool {
	return t.Is("CONSTRAINT", "NOT", "NULL", "PRIMARY", "UNIQUE", "CHECK", "REFERENCES", "COLLATE", "GENERATED")
}

// exprSlot: what fits in an expression on a table's columns (CHECK, WHERE).
func (p *parser) exprSlot(table Name, create bool) Slot {
	s := Slot{Functions: true, Keywords: p.kw(exprOperands...)}
	if create {
		s.Defined = true
	} else {
		t := table
		s.Kinds, s.Table = []ObjKind{ObjColumn}, &t
	}
	return s
}

// generated: GENERATED {ALWAYS | BY DEFAULT [ON NULL]} AS {IDENTITY [(…)] | (expr) [STORED]}
func (p *parser) generated(exprSlot Slot) {
	p.at(p.kwSlot("ALWAYS AS IDENTITY@pog", "BY DEFAULT AS IDENTITY@pog", "ALWAYS AS"))
	if !p.accept("ALWAYS") {
		if !p.seq("BY", "DEFAULT") {
			p.give()
		}
		p.at(p.kwSlot("AS IDENTITY", "ON NULL AS IDENTITY@o"))
		if p.seq("ON", "NULL") {
			p.at(p.kwSlot("AS IDENTITY"))
		}
	} else {
		p.at(p.kwSlot("AS IDENTITY@pog", "AS"))
	}
	if !p.accept("AS") {
		p.give()
	}
	p.at(p.kwSlot("IDENTITY@pog"))
	if p.accept("IDENTITY") {
		if p.isKind(token.LParen) {
			p.skipParen(p.kwSlot(sequenceOptions...))
		}
		return
	}
	if p.isKind(token.LParen) {
		p.skipParen(exprSlot)
	}
}

// references: REFERENCES table [(columns)] [actions]; fromCol names the
// referencing column (to guess the table).
func (p *parser) references(fromCol string, follow []string) {
	tbl := p.objName(Slot{Kinds: []ObjKind{ObjTable}, Guess: fromCol})
	p.ref(Ref{Kind: ObjTable, Name: tbl, Role: RoleUse})
	after := Slot{Keywords: append(p.kw(referenceActions...), follow...)}
	p.at(after)
	if p.isKind(token.LParen) {
		p.i++
		for {
			c := p.listItem(Slot{Kinds: []ObjKind{ObjColumn}, Table: &tbl, PreferKeys: true})
			t := tbl
			p.ref(Ref{Kind: ObjColumn, Name: c, Table: &t, Role: RoleUse})
			p.at(Slot{})
			if p.acceptKind(token.Comma) {
				continue
			}
			p.expectKind(token.RParen, Slot{})
			break
		}
	}
	for {
		p.at(after)
		switch {
		case p.is("ON"):
			p.i++
			p.at(p.kwSlot("DELETE", "UPDATE@pgs"))
			if !p.accept("DELETE", "UPDATE") {
				p.give()
			}
			p.at(p.kwSlot(referentialActions...))
			switch {
			case p.accept("CASCADE", "RESTRICT"):
			case p.accept("SET"):
				p.at(p.kwSlot("NULL", "DEFAULT@pgs"))
				p.i++
			case p.accept("NO"):
				p.at(p.kwSlot("ACTION"))
				p.i++
			default:
				p.give()
			}
		case p.accept("MATCH"):
			p.at(p.kwSlot("FULL", "PARTIAL", "SIMPLE"))
			p.i++
		case p.is("NOT"):
			if n, ok := p.peek(1); ok && n.Is("DEFERRABLE") {
				p.i += 2
				continue
			}
			return
		case p.accept("DEFERRABLE", "ENABLE", "DISABLE", "VALIDATE", "NOVALIDATE", "RELY"):
		case p.accept("INITIALLY"):
			p.at(p.kwSlot("DEFERRED", "IMMEDIATE"))
			p.i++
		default:
			return
		}
	}
}

// tableConstraint: [CONSTRAINT name] PRIMARY KEY (…) | UNIQUE (…) | FOREIGN
// KEY (…) REFERENCES … | CHECK (…) | EXCLUDE …
func (p *parser) tableConstraint(table Name, create bool) {
	if p.accept("CONSTRAINT") {
		n := p.word(Slot{})
		p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &table, Role: RoleCreate})
	}
	p.at(p.kwSlot(constraintKinds...))
	switch {
	case p.seq("PRIMARY", "KEY"), p.accept("UNIQUE"):
		p.columnNames(table, create, RoleUse)
	case p.seq("FOREIGN", "KEY"):
		cols := p.columnNames(table, create, RoleUse)
		p.at(p.kwSlot("REFERENCES"))
		if !p.accept("REFERENCES") {
			p.give()
		}
		guess := ""
		if len(cols) == 1 {
			guess = cols[0].Name
		}
		p.references(guess, nil)
	case p.accept("CHECK"):
		p.at(Slot{})
		p.skipParen(p.exprSlot(table, create))
	case p.accept("EXCLUDE"):
		p.skipTo(Slot{}, token.RParen)
		return
	default:
		p.give()
	}
	for {
		p.at(p.kwSlot(constraintStates...))
		switch {
		case p.accept("NOT"):
			p.at(p.kwSlot("DEFERRABLE", "VALID@p"))
			p.i++
		case p.accept("DEFERRABLE", "ENABLE", "DISABLE", "VALIDATE", "NOVALIDATE", "RELY", "NORELY"):
		case p.accept("INITIALLY"):
			p.at(p.kwSlot("DEFERRED", "IMMEDIATE"))
			p.i++
		case p.accept("USING"):
			p.skipTo(Slot{}, token.RParen)
			return
		default:
			return
		}
	}
}

// columnNames: ( col, … ) of table; in CREATE TABLE the columns the
// statement defines.
func (p *parser) columnNames(table Name, create bool, role Role) []Name {
	slot := Slot{Kinds: []ObjKind{ObjColumn}, Table: &table}
	if create {
		slot = Slot{Defined: true}
		role = RoleDefined
	}
	p.expectKind(token.LParen, Slot{})
	var out []Name
	for {
		n := p.listItem(slot, "ASC", "DESC")
		t := table
		p.ref(Ref{Kind: ObjColumn, Name: n, Table: &t, Role: role})
		out = append(out, n)
		p.at(p.kwSlot("ASC@ps", "DESC@ps"))
		p.accept("ASC", "DESC")
		p.at(Slot{})
		if p.acceptKind(token.Comma) {
			continue
		}
		p.expectKind(token.RParen, Slot{})
		return out
	}
}

func (p *parser) createView(materialized bool) {
	s := p.kwSlot("IF NOT EXISTS@ps")
	ine := p.ifNotExists(s)
	if ine {
		s = Slot{}
	}
	name := p.newName(s)
	kind := ObjView
	if materialized {
		kind = ObjMatView
	}
	p.ref(Ref{Kind: kind, Name: name, Role: RoleCreate, IfExists: ine})
	for {
		p.at(p.kwSlot(viewAfterName...))
		switch {
		case p.accept("AS"):
			p.body()
		case p.isKind(token.LParen):
			p.skipParen(Slot{}) // the view's column names: new
		case p.accept("WITH"):
			if p.isKind(token.LParen) {
				p.skipParen(Slot{})
			}
		default:
			p.i++ // Oracle BUILD IMMEDIATE, REFRESH …, TABLESPACE …
		}
	}
}

func (p *parser) createIndex() {
	p.at(p.kwSlot("ON", "IF NOT EXISTS@ps", "CONCURRENTLY@p"))
	if p.accept("CONCURRENTLY") {
		p.at(p.kwSlot("ON", "IF NOT EXISTS@p"))
	}
	ine := p.ifNotExists(Slot{})
	if !p.is("ON") || ine {
		n := p.newName(Slot{})
		p.ref(Ref{Kind: ObjIndex, Name: n, Role: RoleCreate, IfExists: ine})
	}
	p.at(p.kwSlot("ON"))
	if !p.accept("ON") {
		p.give()
	}
	tblSlot := Slot{Kinds: []ObjKind{ObjTable, ObjMatView}}
	p.at(merge(tblSlot, p.kwSlot("ONLY@p")))
	p.accept("ONLY")
	tbl := p.objName(tblSlot)
	p.ref(Ref{Kind: ObjTable, Kinds: []ObjKind{ObjTable, ObjMatView}, Name: tbl, Role: RoleUse})
	p.at(p.kwSlot("USING@p"))
	if p.accept("USING") {
		p.word(Slot{Keywords: indexMethods})
	}
	p.indexElements(tbl)
	for {
		p.at(p.kwSlot(indexTail...))
		switch {
		case p.accept("WHERE"):
			p.expr(p.exprSlot(tbl, false), Slot{}, nil)
			p.give()
		case p.accept("INCLUDE"):
			p.columnNames(tbl, false, RoleUse)
		case p.isKind(token.LParen):
			p.skipParen(Slot{})
		default:
			p.i++
		}
	}
}

// indexElements: ( column | expression | function(…) [opclass] [ASC|DESC] …, … )
func (p *parser) indexElements(tbl Name) {
	p.expectKind(token.LParen, Slot{})
	el := p.exprSlot(tbl, false)
	el.Keywords = nil
	for {
		p.at(el)
		t := p.cur()
		switch {
		case t.Kind == token.LParen:
			p.skipParen(el)
		case t.IsWord():
			p.i++
			if p.i < len(p.t) && p.t[p.i].Kind == token.LParen {
				p.skipParen(el) // function(…)
				break
			}
			tt := tbl
			p.ref(Ref{Kind: ObjColumn, Name: Name{Name: t.Text, Quoted: t.Quoted, From: t.From, To: t.To}, Table: &tt, Role: RoleUse})
		default:
			p.give()
		}
		for {
			p.at(p.kwSlot(indexElementTail...))
			t := p.cur()
			if t.Kind == token.Comma {
				p.i++
				break
			}
			if t.Kind == token.RParen {
				p.i++
				return
			}
			if t.Kind == token.LParen {
				p.skipParen(Slot{})
				continue
			}
			p.i++ // ASC, DESC, NULLS FIRST, an operator class, …
		}
	}
}

func (p *parser) createTrigger() {
	ine := p.ifNotExists(Slot{})
	n := p.newName(Slot{})
	p.ref(Ref{Kind: ObjTrigger, Name: n, Role: RoleCreate, IfExists: ine})
	nameEnd := p.i
	for {
		prev := p.prev()
		var slot Slot
		switch {
		case p.i == nameEnd:
			slot = p.kwSlot(triggerTiming...)
		case prev.Is("INSTEAD"):
			slot = p.kwSlot("OF")
		case prev.Is("BEFORE", "AFTER", "OR") || (prev.Is("OF") && p.t[p.i-2].Is("INSTEAD")):
			slot = p.kwSlot(triggerEvents...)
		case prev.Is("OF"):
			slot = Slot{} // UPDATE OF column
		case prev.Is("INSERT", "UPDATE", "DELETE", "TRUNCATE"):
			slot = p.kwSlot("ON", "OR", "OF@pos")
		default:
			slot = p.kwSlot("ON")
		}
		p.at(slot)
		if p.accept("ON") {
			break
		}
		p.i++
	}
	tbl := p.objName(Slot{Kinds: []ObjKind{ObjTable, ObjView}})
	p.ref(Ref{Kind: ObjTable, Kinds: []ObjKind{ObjTable, ObjView}, Name: tbl, Role: RoleUse})
	p.at(p.kwSlot(triggerTail...))
	p.stop()
}

func (p *parser) createSequence() {
	ine := p.ifNotExists(p.kwSlot("IF NOT EXISTS@ps"))
	n := p.newName(Slot{})
	p.ref(Ref{Kind: ObjSequence, Name: n, Role: RoleCreate, IfExists: ine})
	nameEnd := p.i
	opts := p.kwSlot(sequenceOptions...)
	for {
		prev := p.prev()
		if p.i == nameEnd || prev.Kind == token.Number ||
			prev.Is("CYCLE", "NOCYCLE", "NOCACHE", "ORDER", "NOORDER", "NOMINVALUE", "NOMAXVALUE") ||
			(prev.Is("MINVALUE", "MAXVALUE") && p.i > 1 && p.t[p.i-2].Is("NO")) {
			p.at(opts)
		} else {
			p.at(Slot{})
		}
		p.i++
	}
}

func (p *parser) createSynonym() {
	n := p.newName(Slot{})
	p.ref(Ref{Kind: ObjSynonym, Name: n, Role: RoleCreate})
	p.at(p.kwSlot("FOR"))
	if !p.accept("FOR") {
		p.give()
	}
	p.objName(Slot{Kinds: Relations}) // may also be a package, …: not checked
}

// ---- ALTER ------------------------------------------------------------------

func (p *parser) alter() {
	p.at(p.kwSlot(alterKinds...))
	switch {
	case p.accept("TABLE"):
		p.a.Kind = "ALTER TABLE"
		p.alterTable()
	case p.accept("VIEW"):
		p.a.Kind = "ALTER VIEW"
		p.alterObject(ObjView, alterViewActions)
	case p.seq("MATERIALIZED", "VIEW"):
		p.a.Kind = "ALTER MATERIALIZED VIEW"
		p.alterObject(ObjMatView, alterViewActions)
	case p.accept("INDEX"):
		p.a.Kind = "ALTER INDEX"
		p.alterObject(ObjIndex, alterIndexActions)
	case p.accept("SEQUENCE"):
		p.a.Kind = "ALTER SEQUENCE"
		p.alterObject(ObjSequence, sequenceOptions)
	case p.accept("TRIGGER"):
		p.a.Kind = "ALTER TRIGGER"
		p.alterObject(ObjTrigger, alterTriggerActions)
	case p.accept("TYPE", "DOMAIN"):
		p.a.Kind = "ALTER " + p.prev().Upper
		p.alterObject(ObjType, alterTypeActions)
	default:
		p.give()
	}
}

// alterObject: ALTER VIEW|INDEX|SEQUENCE|TRIGGER|TYPE name action … (only the
// name and RENAME are followed).
func (p *parser) alterObject(kind ObjKind, actions []string) {
	s := Slot{Kinds: []ObjKind{kind}, Keywords: p.kw("IF EXISTS@p")}
	ie := p.ifExists(s)
	s.Keywords = nil
	n := p.objName(s)
	r := Ref{Kind: kind, Name: n, Role: RoleUse, IfExists: ie}
	if kind == ObjTrigger && p.d == sqlsplit.PostgreSQL {
		p.at(p.kwSlot("ON"))
		if p.accept("ON") {
			tbl := p.objName(Slot{Kinds: []ObjKind{ObjTable, ObjView}})
			p.ref(Ref{Kind: ObjTable, Kinds: []ObjKind{ObjTable, ObjView}, Name: tbl, Role: RoleUse})
			r.Table = &tbl
		}
	}
	p.ref(r)
	p.at(p.kwSlot(actions...))
	if p.accept("RENAME") {
		at := p.kwSlot("TO")
		if kind == ObjView || kind == ObjMatView {
			at = p.kwSlot("TO", "COLUMN@p")
		}
		p.at(at)
		if p.accept("COLUMN") {
			c := p.word(Slot{Kinds: []ObjKind{ObjColumn}, Table: &n})
			nn := n
			p.ref(Ref{Kind: ObjColumn, Name: c, Table: &nn, Role: RoleUse})
			p.at(p.kwSlot("TO"))
			p.accept("TO")
			p.word(Slot{})
			p.stop()
		}
		if p.accept("TO") {
			nn := p.newName(Slot{})
			old := n
			p.ref(Ref{Kind: kind, Name: nn, Role: RoleCreate, Renamed: &old})
		}
	}
	p.stop()
}

func (p *parser) alterTable() {
	s := Slot{Kinds: []ObjKind{ObjTable}, Keywords: p.kw("IF EXISTS@p", "ONLY@p")}
	ie := p.ifExists(s)
	s.Keywords = p.kw("ONLY@p")
	p.at(s)
	p.accept("ONLY")
	s.Keywords = nil
	t := p.objName(s)
	p.ref(Ref{Kind: ObjTable, Name: t, Role: RoleUse, IfExists: ie})
	for {
		p.alterAction(t)
		p.at(Slot{})
		if !p.acceptKind(token.Comma) {
			p.give()
		}
	}
}

func (p *parser) colSlot(t Name, extra ...string) Slot {
	return Slot{Kinds: []ObjKind{ObjColumn}, Table: &t, Keywords: p.kw(extra...)}
}

func (p *parser) colRef(t Name, n Name, role Role, ifExists bool) {
	p.ref(Ref{Kind: ObjColumn, Name: n, Table: &t, Role: role, IfExists: ifExists})
}

func (p *parser) constraintSlot(t Name, extra ...string) Slot {
	return Slot{Kinds: []ObjKind{ObjConstraint}, Table: &t, Keywords: p.kw(extra...)}
}

func (p *parser) alterAction(t Name) {
	p.at(p.kwSlot(alterTableActions...))
	switch {
	case p.accept("ADD"):
		p.at(p.kwSlot(afterAdd...)) // or a new column's name
		switch {
		case p.accept("COLUMN"):
			ine := p.ifNotExists(p.kwSlot("IF NOT EXISTS@p"))
			p.at(Slot{})
			p.columnDef(t, false, ine)
		case isTableConstraintStart(p.cur()):
			p.tableConstraint(t, false)
		case p.isKind(token.LParen): // Oracle: ADD (col type, …)
			p.i++
			for {
				p.at(p.kwSlot("CONSTRAINT", "PRIMARY KEY", "FOREIGN KEY", "UNIQUE", "CHECK"))
				if isTableConstraintStart(p.cur()) {
					p.tableConstraint(t, false)
				} else {
					p.columnDef(t, false, false)
				}
				p.at(Slot{})
				if p.acceptKind(token.Comma) {
					continue
				}
				p.expectKind(token.RParen, Slot{})
				return
			}
		default:
			p.columnDef(t, false, false)
		}
	case p.accept("DROP"):
		p.at(p.colSlot(t, "COLUMN", "CONSTRAINT@pog", "PRIMARY KEY@o", "UNIQUE@o", "IF EXISTS@p"))
		switch {
		case p.accept("CONSTRAINT"):
			ie := p.ifExists(p.constraintSlot(t, "IF EXISTS@p"))
			n := p.word(p.constraintSlot(t))
			p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &t, Role: RoleDrop, IfExists: ie})
		case p.seq("PRIMARY", "KEY"):
		case p.accept("UNIQUE"):
			p.columnNames(t, false, RoleUse)
		case p.isKind(token.LParen): // Oracle: DROP (a, b)
			p.columnNames(t, false, RoleDrop)
		default:
			if p.accept("COLUMN") {
				p.at(p.colSlot(t, "IF EXISTS@p"))
			}
			ie := p.ifExists(p.colSlot(t))
			c := p.word(p.colSlot(t))
			p.colRef(t, c, RoleDrop, ie)
		}
		for {
			p.at(p.kwSlot("CASCADE@pgo", "RESTRICT@pg", "CASCADE CONSTRAINTS@o", "KEEP INDEX@o", "DROP INDEX@o"))
			switch {
			case p.accept("CASCADE"):
				if p.is("CONSTRAINTS") {
					p.i++
				}
			case p.accept("RESTRICT"):
			case p.accept("KEEP", "DROP"):
				p.at(p.kwSlot("INDEX"))
				p.accept("INDEX")
			default:
				return
			}
		}
	case p.accept("ALTER"):
		p.at(p.colSlot(t, "COLUMN", "CONSTRAINT@p"))
		if p.accept("CONSTRAINT") {
			n := p.word(p.constraintSlot(t))
			p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &t, Role: RoleUse})
			p.skipTo(p.kwSlot(constraintStates...))
			return
		}
		if p.accept("COLUMN") {
			p.at(p.colSlot(t))
		}
		c := p.word(p.colSlot(t))
		p.colRef(t, c, RoleUse, false)
		p.at(p.kwSlot(afterAlterColumn...))
		switch {
		case p.accept("TYPE"):
			p.at(Slot{Types: true})
			p.dataType()
			p.skipTo(p.kwSlot("USING@p", "COLLATE@p"))
		case p.accept("SET"):
			p.at(p.kwSlot("DATA TYPE", "DEFAULT", "NOT NULL", "STATISTICS@p"))
			switch {
			case p.seq("DATA", "TYPE"):
				p.at(Slot{Types: true})
				p.dataType()
			case p.accept("DEFAULT"):
				p.expr(merge(Slot{Functions: true}, p.kwSlot(defaultValues...)), Slot{}, nil)
				return
			case p.seq("NOT", "NULL"):
			}
			p.skipTo(Slot{})
		case p.accept("DROP"):
			p.at(p.kwSlot("DEFAULT", "NOT NULL", "IDENTITY@p", "EXPRESSION@p"))
			p.skipTo(Slot{})
		default:
			p.skipTo(Slot{})
		}
	case p.accept("MODIFY"):
		p.at(p.colSlot(t, "CONSTRAINT"))
		if p.accept("COLUMN") { // MySQL: MODIFY COLUMN c type
			p.at(p.colSlot(t))
		}
		if p.accept("CONSTRAINT") {
			n := p.word(p.constraintSlot(t))
			p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &t, Role: RoleUse})
			p.skipTo(p.kwSlot(constraintStates...))
			return
		}
		if p.isKind(token.LParen) {
			p.i++
			for {
				p.modifyColumn(t)
				p.at(Slot{})
				if p.acceptKind(token.Comma) {
					continue
				}
				p.expectKind(token.RParen, Slot{})
				return
			}
		}
		p.modifyColumn(t)
	case p.accept("RENAME"):
		p.at(p.colSlot(t, "COLUMN", "TO", "CONSTRAINT@po"))
		switch {
		case p.accept("TO"):
			n := p.newName(Slot{})
			old := t
			p.ref(Ref{Kind: ObjTable, Name: n, Role: RoleCreate, Renamed: &old})
		case p.accept("CONSTRAINT"):
			c := p.word(p.constraintSlot(t))
			p.ref(Ref{Kind: ObjConstraint, Name: c, Table: &t, Role: RoleUse})
			p.at(p.kwSlot("TO"))
			p.accept("TO")
			p.word(Slot{})
		default:
			if p.accept("COLUMN") {
				p.at(p.colSlot(t))
			}
			c := p.word(p.colSlot(t))
			p.colRef(t, c, RoleUse, false)
			p.at(p.kwSlot("TO"))
			if !p.accept("TO") {
				p.give()
			}
			n := p.word(Slot{})
			old := c
			p.ref(Ref{Kind: ObjColumn, Name: n, Table: &t, Role: RoleCreate, Renamed: &old})
		}
	case p.accept("VALIDATE"):
		p.at(p.kwSlot("CONSTRAINT"))
		p.accept("CONSTRAINT")
		n := p.word(p.constraintSlot(t))
		p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &t, Role: RoleUse})
	case p.accept("ENABLE", "DISABLE"):
		p.at(p.kwSlot("TRIGGER@p", "CONSTRAINT@o", "ALL TRIGGERS@o", "PRIMARY KEY@o", "ROW LEVEL SECURITY@p", "NOVALIDATE CONSTRAINT@o", "VALIDATE CONSTRAINT@o"))
		if p.accept("NOVALIDATE", "VALIDATE") {
			p.at(p.kwSlot("CONSTRAINT"))
		}
		switch {
		case p.accept("TRIGGER"):
			s := Slot{Kinds: []ObjKind{ObjTrigger}, Table: &t, Keywords: p.kw("ALL", "USER")}
			p.at(s)
			if !p.accept("ALL", "USER") {
				n := p.word(s)
				p.ref(Ref{Kind: ObjTrigger, Name: n, Table: &t, Role: RoleUse})
			}
		case p.accept("CONSTRAINT"):
			n := p.word(p.constraintSlot(t))
			p.ref(Ref{Kind: ObjConstraint, Name: n, Table: &t, Role: RoleUse})
		}
		p.skipTo(Slot{})
	default:
		p.skipTo(Slot{}) // OWNER TO, SET SCHEMA, …
	}
}

// modifyColumn (Oracle): MODIFY col [type] [constraints].
func (p *parser) modifyColumn(t Name) {
	c := p.word(p.colSlot(t))
	p.colRef(t, c, RoleUse, false)
	p.at(merge(Slot{Types: true}, p.kwSlot(columnConstraints...)))
	var cont []string
	if !isColumnConstraintWord(p.cur()) && !p.is("DEFAULT") {
		cont = p.dataType()
	}
	p.columnConstraints(t, false, cont)
}

// ---- DROP / TRUNCATE / COMMENT / GRANT / RENAME -----------------------------

func (p *parser) drop() {
	p.at(p.kwSlot(dropKinds...))
	var kind ObjKind
	switch {
	case p.accept("TABLE"):
		kind = ObjTable
	case p.accept("VIEW"):
		kind = ObjView
	case p.seq("MATERIALIZED", "VIEW"):
		kind = ObjMatView
	case p.accept("INDEX"):
		kind = ObjIndex
		if p.d == sqlsplit.PostgreSQL {
			p.at(Slot{Kinds: []ObjKind{ObjIndex}, Keywords: p.kw("CONCURRENTLY", "IF EXISTS")})
			p.accept("CONCURRENTLY")
		}
	case p.accept("SEQUENCE"):
		kind = ObjSequence
	case p.accept("TRIGGER"):
		kind = ObjTrigger
	case p.accept("TYPE", "DOMAIN"):
		kind = ObjType
	case p.accept("SCHEMA"):
		kind = ObjSchema
	case p.accept("FUNCTION", "PROCEDURE"):
		kind = ObjFunction
	case p.accept("SYNONYM"):
		kind = ObjSynonym
	case p.seq("PUBLIC", "SYNONYM"):
		kind = ObjSynonym
	default:
		p.give()
	}
	p.a.Kind = "DROP " + strings.ToUpper(kind.String())
	s := Slot{Kinds: []ObjKind{kind}, Keywords: p.kw("IF EXISTS")}
	ie := p.ifExists(s)
	tail := p.kw(dropTail...)
	if kind == ObjTrigger {
		tail = append(tail, p.kw("ON@p")...)
	}
	for {
		n := p.objName(Slot{Kinds: []ObjKind{kind}})
		r := Ref{Kind: kind, Name: n, Role: RoleDrop, IfExists: ie, StrictKind: kind.IsRelation()}
		if kind == ObjFunction {
			p.at(Slot{Keywords: tail})
			if p.isKind(token.LParen) {
				p.skipParen(Slot{Types: true})
			}
		}
		if kind == ObjTrigger {
			p.at(Slot{Keywords: tail})
			if p.accept("ON") {
				tbl := p.objName(Slot{Kinds: []ObjKind{ObjTable, ObjView}})
				p.ref(Ref{Kind: ObjTable, Kinds: []ObjKind{ObjTable, ObjView}, Name: tbl, Role: RoleUse, IfExists: ie})
				r.Table = &tbl
			}
		}
		p.ref(r)
		p.at(Slot{Keywords: tail})
		if !p.acceptKind(token.Comma) {
			break
		}
	}
	for {
		p.at(Slot{Keywords: p.kw(dropTail...)})
		switch {
		case p.accept("CASCADE"):
			if p.is("CONSTRAINTS") {
				p.i++
			}
		case p.accept("RESTRICT", "PURGE", "FORCE", "VALIDATE"):
		default:
			p.give()
		}
	}
}

func (p *parser) truncate() {
	s := Slot{Kinds: []ObjKind{ObjTable}, Keywords: p.kw("TABLE", "ONLY@p")}
	p.at(s)
	if p.accept("TABLE") {
		s.Keywords = p.kw("ONLY@p")
		p.at(s)
	}
	p.accept("ONLY")
	s.Keywords = nil
	for {
		n := p.objName(s)
		p.ref(Ref{Kind: ObjTable, Name: n, Role: RoleUse})
		if p.isKind(token.Mult) {
			p.i++
		}
		p.at(p.kwSlot(truncateTail...))
		if !p.acceptKind(token.Comma) {
			break
		}
	}
	for {
		p.at(p.kwSlot(truncateTail...))
		switch {
		case p.accept("RESTART", "CONTINUE"):
			p.at(p.kwSlot("IDENTITY"))
			p.accept("IDENTITY")
		case p.accept("DROP", "REUSE"):
			p.at(p.kwSlot("STORAGE", "ALL STORAGE@o"))
			p.accept("ALL")
			p.accept("STORAGE")
		case p.accept("CASCADE", "RESTRICT"):
		default:
			p.give()
		}
	}
}

func (p *parser) comment() {
	p.at(p.kwSlot("ON"))
	if !p.accept("ON") {
		p.give()
	}
	p.at(p.kwSlot(commentKinds...))
	switch {
	case p.accept("TABLE"):
		n := p.objName(Slot{Kinds: Relations})
		p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: n, Role: RoleUse})
	case p.accept("VIEW"):
		n := p.objName(Slot{Kinds: []ObjKind{ObjView}})
		p.ref(Ref{Kind: ObjView, Name: n, Role: RoleUse})
	case p.seq("MATERIALIZED", "VIEW"):
		n := p.objName(Slot{Kinds: []ObjKind{ObjMatView}})
		p.ref(Ref{Kind: ObjMatView, Name: n, Role: RoleUse})
	case p.accept("COLUMN"):
		p.commentColumn()
	case p.accept("INDEX", "SEQUENCE", "TYPE", "DOMAIN"):
		kind := map[string]ObjKind{"INDEX": ObjIndex, "SEQUENCE": ObjSequence, "TYPE": ObjType, "DOMAIN": ObjType}[p.prev().Upper]
		n := p.objName(Slot{Kinds: []ObjKind{kind}})
		p.ref(Ref{Kind: kind, Name: n, Role: RoleUse})
	case p.accept("TRIGGER", "CONSTRAINT"):
		kind := ObjTrigger
		if p.prev().Is("CONSTRAINT") {
			kind = ObjConstraint
		}
		n := p.word(Slot{})
		p.at(p.kwSlot("ON"))
		if !p.accept("ON") {
			p.give()
		}
		tbl := p.objName(Slot{Kinds: Relations})
		p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: tbl, Role: RoleUse})
		p.ref(Ref{Kind: kind, Name: n, Table: &tbl, Role: RoleUse})
	default:
		p.give() // SCHEMA, FUNCTION, …
	}
	p.at(p.kwSlot("IS"))
	if !p.accept("IS") {
		p.give()
	}
	p.at(Slot{})
	p.i++
}

// commentColumn: [schema.]table.column
func (p *parser) commentColumn() {
	p.at(Slot{Kinds: Relations})
	a := p.cur()
	if !a.IsWord() {
		p.give()
	}
	p.i++
	p.at(Slot{})
	if !p.acceptKind(token.Period) {
		p.give()
	}
	first := Name{Name: a.Text, Quoted: a.Quoted, From: a.From, To: a.To}
	p.at(Slot{Kinds: []ObjKind{ObjColumn, ObjTable, ObjView, ObjMatView}, Table: &first, Qualifier: a.Text})
	b := p.cur()
	if !b.IsWord() {
		p.give()
	}
	p.i++
	tbl, col := first, Name{Name: b.Text, Quoted: b.Quoted, From: b.From, To: b.To}
	if p.i < len(p.t) && p.t[p.i].Kind == token.Period {
		p.i++
		tbl = Name{Schema: a.Text, Name: b.Text, Quoted: b.Quoted, From: a.From, To: b.To}
		c := p.word(Slot{Kinds: []ObjKind{ObjColumn}, Table: &tbl})
		col = c
	}
	p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: tbl, Role: RoleUse})
	t := tbl
	p.ref(Ref{Kind: ObjColumn, Name: col, Table: &t, Role: RoleUse})
}

func (p *parser) grant(isGrant bool) {
	for {
		prev := p.prev()
		slot := p.kwSlot("ON")
		if prev.Is("GRANT", "REVOKE") || prev.Kind == token.Comma {
			slot = p.kwSlot(grantPrivileges...)
		}
		p.at(slot)
		if p.accept("ON") {
			break
		}
		if p.isKind(token.LParen) {
			p.skipParen(Slot{})
			continue
		}
		p.i++
	}
	kinds := Relations
	soft := true // a sequence, procedure, package, type, …
	p.at(merge(Slot{Kinds: kinds}, p.kwSlot(grantOnKinds...)))
	switch {
	case p.accept("TABLE"):
		soft = false
	case p.accept("SEQUENCE"):
		kinds, soft = []ObjKind{ObjSequence}, false
	case p.is("ALL", "SCHEMA", "FUNCTION", "PROCEDURE", "TYPE", "DOMAIN", "DATABASE"):
		p.stop()
	}
	for {
		n := p.objName(Slot{Kinds: kinds})
		p.ref(Ref{Kind: kinds[0], Kinds: kinds, Name: n, Role: RoleUse, Soft: soft})
		to := "TO"
		if !isGrant {
			to = "FROM"
		}
		p.at(p.kwSlot(to))
		if p.acceptKind(token.Comma) {
			continue
		}
		if !p.accept(to) {
			p.give()
		}
		p.at(p.kwSlot("PUBLIC"))
		p.stop()
	}
}

// rename (Oracle): RENAME old TO new
func (p *parser) rename() {
	n := p.objName(Slot{Kinds: Relations})
	p.ref(Ref{Kind: ObjTable, Kinds: Relations, Name: n, Role: RoleUse, Soft: true}) // or a sequence, synonym
	p.at(p.kwSlot("TO"))
	if !p.accept("TO") {
		p.give()
	}
	nn := p.newName(Slot{})
	old := n
	p.ref(Ref{Kind: ObjTable, Name: nn, Role: RoleCreate, Renamed: &old})
}
