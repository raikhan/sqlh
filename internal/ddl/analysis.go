package ddl

import (
	"strings"

	"github.com/raikhan/sqlh/token"
)

// ObjKind is a kind of database object.
type ObjKind int

const (
	ObjTable ObjKind = iota + 1
	ObjView
	ObjMatView
	ObjIndex
	ObjSequence
	ObjTrigger
	ObjType // user-defined types and domains
	ObjSchema
	ObjFunction // functions and procedures
	ObjConstraint
	ObjColumn
	ObjSynonym
)

func (k ObjKind) String() string {
	switch k {
	case ObjTable:
		return "table"
	case ObjView:
		return "view"
	case ObjMatView:
		return "materialized view"
	case ObjIndex:
		return "index"
	case ObjSequence:
		return "sequence"
	case ObjTrigger:
		return "trigger"
	case ObjType:
		return "type"
	case ObjSchema:
		return "schema"
	case ObjFunction:
		return "function"
	case ObjConstraint:
		return "constraint"
	case ObjColumn:
		return "column"
	case ObjSynonym:
		return "synonym"
	}
	return "object"
}

// Relations: tables, views and materialized views.
var Relations = []ObjKind{ObjTable, ObjView, ObjMatView}

// IsRelation reports whether k is a table, view or materialized view.
func (k ObjKind) IsRelation() bool { return k == ObjTable || k == ObjView || k == ObjMatView }

// Name is an object name as written: [schema.]name.
type Name struct {
	Schema string
	Name   string
	Quoted bool
	// From, To: the whole name in the statement (token positions)
	From, To token.Pos
}

func (n Name) String() string {
	if n.Schema != "" {
		return n.Schema + "." + n.Name
	}
	return n.Name
}

// Upper is the name upper-cased (for case-insensitive matching).
func (n Name) Upper() string { return strings.ToUpper(n.Name) }

// Role is what a statement does with an object it names.
type Role int

const (
	// RoleUse: the object must exist (ALTER TABLE t, REFERENCES t, a column).
	RoleUse Role = iota + 1
	// RoleCreate: the statement creates it (CREATE TABLE t, ADD COLUMN c,
	// RENAME … TO n).
	RoleCreate
	// RoleDrop: the statement drops it (it must exist unless IfExists).
	RoleDrop
	// RoleDefined: a column the statement itself must define (PRIMARY KEY (c)
	// in CREATE TABLE).
	RoleDefined
)

// Ref is an object a statement names.
type Ref struct {
	Kind ObjKind
	// Kinds: the kinds that fit (DROP TABLE: table; COMMENT ON TABLE: any
	// relation); Kind is the first.
	Kinds []ObjKind
	Name  Name
	// Table: the owning table of a column, constraint or (PostgreSQL) trigger.
	Table *Name
	Role  Role
	// IfExists: IF EXISTS / IF NOT EXISTS was given.
	IfExists bool
	// StrictKind: another kind of object of that name is an error (DROP VIEW
	// on a table).
	StrictKind bool
	// Renamed: for RoleCreate by RENAME, the old name.
	Renamed *Name
	// Soft: may also name another kind of object (GRANT … ON x, Oracle
	// RENAME x): not reported when unknown.
	Soft bool
}

// Slot is what fits at a cursor position.
type Slot struct {
	// Kinds: names of objects of these kinds (Qualifier: in that schema;
	// ObjColumn/ObjConstraint/ObjTrigger with Table: of that table).
	Kinds     []ObjKind
	Qualifier string
	Table     *Name
	// Defined: the columns the statement defines (CREATE TABLE).
	Defined bool
	// Types: data types.
	Types bool
	// Functions: functions (an expression).
	Functions bool
	// Excluded: the EXCLUDED pseudo-table (ON CONFLICT … DO UPDATE).
	Excluded bool
	// Keywords that fit, multi-word ones as one item ("PRIMARY KEY").
	Keywords []string
	// Guess: a column name a referenced table may be guessed from
	// (customer_id → customer), to rank that table first.
	Guess string
	// PreferKeys: primary key / unique columns first (REFERENCES t (…)).
	PreferKeys bool
}

// Analysis is what a statement names and, with a cursor, what fits there.
type Analysis struct {
	// Kind: the statement ("CREATE TABLE", "ALTER TABLE", "DROP", "INSERT",
	// …); "" when this package doesn't handle it.
	Kind string
	// DML: an INSERT/UPDATE/DELETE/MERGE (only the tails are handled here).
	DML  bool
	Refs []Ref
	// Defined: the columns a CREATE TABLE defines, in order.
	Defined []Name
	// HasLike: CREATE TABLE … (LIKE t) / INHERITS / PARTITION OF: columns come
	// from elsewhere too.
	HasLike bool
	// Slot: what fits at the cursor; nil when the cursor isn't reached or
	// the position isn't understood.
	Slot *Slot
	// Handoff: the cursor is in a part the regular completer handles (the
	// query of CREATE VIEW … AS, the body of a DML statement).
	Handoff bool
	// Body: index (in the tokens) of the query of CREATE VIEW/TABLE … AS; -1
	// when none.
	Body int
	// Unsure: the walk stopped at something it doesn't understand.
	Unsure bool
}

// Target is the statement's main object: the created/altered/dropped one,
// or the DML target.
func (a *Analysis) Target() *Ref {
	for i := range a.Refs {
		if a.Refs[i].Kind.IsRelation() {
			return &a.Refs[i]
		}
	}
	return nil
}
