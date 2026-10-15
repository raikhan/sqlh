package completer

import (
	"strings"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/ddl"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// ddlComplete completes DDL statements and DML tails (internal/ddl); ok is
// false when the position is the regular completer's.
func (c *Completer) ddlComplete(text string, params lsp.CompletionParams, lower, spaceTrigger bool) ([]lsp.CompletionItem, bool) {
	line, char := params.Position.Line, params.Position.Character
	a, ok := ddl.AtCursor(text, line, char, sqlsplit.DialectOf(string(c.Driver)))
	if !ok || a.Slot == nil || a.Handoff {
		return nil, false
	}
	if spaceTrigger && !listsNames(a.Slot) {
		return []lsp.CompletionItem{}, true // keywords only: not worth a popup at every space
	}
	lastWord := ddl.Partial(text, line, char)
	parenFollows := charAfterCursor(text, line, char) == '('
	items := c.slotCandidates(a, lower, lastWord, parenFollows)
	if c.LowercaseIdentifiers {
		lowercaseIdentifierItems(items)
	}
	items = filterCandidates(items, lastWord)
	populateSortText(items)
	return items, true
}

// listsNames: a slot with names or types, worth offering right after a space.
func listsNames(s *ddl.Slot) bool {
	return len(s.Kinds) > 0 || s.Defined || s.Types || s.Excluded
}

func (c *Completer) slotCandidates(a *ddl.Analysis, lower bool, lastWord string, parenFollows bool) []lsp.CompletionItem {
	s := a.Slot
	items := []lsp.CompletionItem{}
	if c.DBCache != nil {
		var relKinds []ddl.ObjKind
		for _, k := range s.Kinds {
			switch {
			case k.IsRelation():
				relKinds = append(relKinds, k)
			case k == ddl.ObjColumn:
				if s.Table != nil {
					items = append(items, c.namedTableColumns(*s.Table, s.PreferKeys)...)
				}
			case k == ddl.ObjFunction:
				items = append(items, c.functionCandidates(s.Qualifier, lower, true)...)
			case k == ddl.ObjSchema:
				items = append(items, c.SchemaCandidates()...)
			default:
				items = append(items, c.objectCandidates(k, s.Qualifier, s.Table)...)
			}
		}
		if len(relKinds) > 0 {
			items = append(items, c.relationCandidates(relKinds, s.Qualifier, s.Guess)...)
			if s.Qualifier == "" && s.Table == nil {
				items = append(items, c.SchemaCandidates()...)
			}
		}
	}
	if s.Defined {
		for _, n := range a.Defined {
			items = append(items, lsp.CompletionItem{Label: n.Name, Kind: lsp.FieldCompletion, Detail: "column of this table"})
		}
	}
	if s.Types {
		items = append(items, c.typeCandidates(lower)...)
	}
	if s.Functions && lastWord != "" {
		items = append(items, c.functionCandidates("", lower, parenFollows)...)
	}
	if s.Excluded {
		items = append(items, lsp.CompletionItem{Label: "EXCLUDED", Kind: lsp.ClassCompletion, Detail: "the row proposed for insertion"})
	}
	for _, k := range s.Keywords {
		if lower {
			k = strings.ToLower(k)
		}
		items = append(items, lsp.CompletionItem{Label: k, Kind: lsp.KeywordCompletion, Detail: "keyword"})
	}
	return items
}

var relKindOf = map[database.RelKind]ddl.ObjKind{
	database.RelTable:   ddl.ObjTable,
	database.RelView:    ddl.ObjView,
	database.RelMatView: ddl.ObjMatView,
}

// relationCandidates: tables/views/materialized views of the kinds (all
// relations while the kinds aren't known yet), in a schema.
func (c *Completer) relationCandidates(kinds []ddl.ObjKind, qualifier, guess string) []lsp.CompletionItem {
	var tables []string
	if qualifier != "" {
		t, ok := c.DBCache.SortedTablesByDBName(qualifier)
		if !ok {
			return nil
		}
		tables = t
	} else {
		tables = c.DBCache.SortedTables()
	}
	rel := c.DBCache.Relations
	if qualifier == "" && rel != nil && rel.ViewsLoaded && len(kinds) < len(ddl.Relations) {
		want := map[ddl.ObjKind]bool{}
		for _, k := range kinds {
			want[k] = true
		}
		var keep []string
		for _, t := range tables {
			if want[relKindOf[rel.Kind(t)]] {
				keep = append(keep, t)
			}
		}
		tables = keep
	}
	items := generateTableCandidates(tables, c.DBCache)
	if guess != "" {
		for i := range items {
			if guessedTable(guess, items[i].Label) {
				items[i].SortText = "0" // the likely target first
				items[i].Detail += " (guessed from " + guess + ")"
			}
		}
	}
	return items
}

// guessedTable: a column name like customer_id / customerId / customer_code
// refers to table (customer, customers).
func guessedTable(column, table string) bool {
	stem := strings.ToUpper(column)
	for _, suf := range []string{"_ID", "ID", "_CODE", "CODE", "_NO", "_KEY", "_FK"} {
		if strings.HasSuffix(stem, suf) && len(stem) > len(suf) {
			stem = strings.TrimSuffix(stem, suf)
			break
		}
	}
	t := strings.ToUpper(table)
	return t == stem || t == stem+"S" || t == stem+"ES" || (strings.HasSuffix(stem, "Y") && t == stem[:len(stem)-1]+"IES")
}

// namedTableColumns: the columns of a table named in the statement.
func (c *Completer) namedTableColumns(t ddl.Name, preferKeys bool) []lsp.CompletionItem {
	var (
		cols []*database.ColumnDesc
		ok   bool
	)
	if t.Schema != "" {
		cols, ok = c.DBCache.ColumnDatabase(t.Schema, t.Name)
	} else {
		cols, ok = c.DBCache.ColumnDescs(t.Name)
	}
	if !ok {
		return nil
	}
	items := generateColumnCandidates(t.Name, cols)
	if preferKeys {
		keys := map[string]bool{}
		schema := t.Schema
		if schema == "" {
			schema = c.DBCache.DefaultSchema()
		}
		for _, x := range c.DBCache.Objects.OfTable(database.ObjectConstraint, schema, t.Name) {
			if x.Detail == "primary key" || x.Detail == "unique" {
				for _, col := range x.Columns {
					keys[strings.ToUpper(col)] = true
				}
			}
		}
		for i, col := range cols {
			if database.IsPrimaryKey(col.Key) || keys[strings.ToUpper(col.Name)] {
				items[i].SortText = "00"
				items[i].Detail += " (key)"
			}
		}
	}
	return items
}

var objectKindOf = map[ddl.ObjKind]string{
	ddl.ObjIndex:      database.ObjectIndex,
	ddl.ObjSequence:   database.ObjectSequence,
	ddl.ObjTrigger:    database.ObjectTrigger,
	ddl.ObjType:       database.ObjectType,
	ddl.ObjConstraint: database.ObjectConstraint,
}

// objectCandidates: indexes, sequences, triggers, types or constraints (of
// a table, for constraints and triggers with one).
func (c *Completer) objectCandidates(k ddl.ObjKind, qualifier string, table *ddl.Name) []lsp.CompletionItem {
	kind, ok := objectKindOf[k]
	if !ok || c.DBCache.Objects == nil {
		return nil
	}
	schema := qualifier
	if schema == "" && table != nil {
		schema = table.Schema
	}
	if schema == "" {
		schema = c.DBCache.DefaultSchema()
	}
	var list []*database.SchemaObject
	if table != nil && (kind == database.ObjectConstraint || kind == database.ObjectTrigger) {
		list = c.DBCache.Objects.OfTable(kind, schema, table.Name)
	} else {
		list = c.DBCache.Objects.All(kind, schema)
	}
	items := make([]lsp.CompletionItem, 0, len(list))
	for _, x := range list {
		detail := kind
		if x.Detail != "" {
			detail = x.Detail + " " + kind
		}
		if x.Table != "" {
			detail += " on " + x.Table
		}
		it := lsp.CompletionItem{
			Label:         x.Name,
			Kind:          lsp.ClassCompletion,
			Detail:        detail,
			Documentation: &lsp.MarkupContent{Kind: lsp.Markdown, Value: x.Doc()},
		}
		if x.Generated {
			it.SortText = "3" // system-named: last
		}
		items = append(items, it)
	}
	return items
}

// typeCandidates: the dialect's built-in types, then the database's own.
func (c *Completer) typeCandidates(lower bool) []lsp.CompletionItem {
	var items []lsp.CompletionItem
	for _, t := range dialect.DataTypes(c.Driver) {
		label := t.Name
		if lower {
			label = strings.ToLower(label)
		}
		it := lsp.CompletionItem{Label: label, Kind: lsp.TypeParameterCompletion, Detail: "type " + strings.ToLower(t.Name) + t.Params, SortText: "1"}
		if t.Doc != "" {
			it.Documentation = &lsp.MarkupContent{Kind: lsp.Markdown, Value: t.Doc}
		}
		items = append(items, it)
	}
	if c.DBCache != nil {
		for _, x := range c.DBCache.Objects.All(database.ObjectType, c.DBCache.DefaultSchema()) {
			items = append(items, lsp.CompletionItem{
				Label: x.Name, Kind: lsp.ClassCompletion, Detail: strings.TrimSpace(x.Detail + " type"),
				Documentation: &lsp.MarkupContent{Kind: lsp.Markdown, Value: x.Doc()},
			})
		}
	}
	return items
}
