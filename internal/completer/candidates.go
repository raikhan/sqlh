package completer

import (
	"fmt"
	"strings"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/parser/parseutil"
)

func (c *Completer) keywordCandidates(lower bool, keywords []string) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, k := range keywords {
		candidate := lsp.CompletionItem{
			Label:  k,
			Kind:   lsp.KeywordCompletion,
			Detail: "keyword",
		}
		if lower {
			candidate.Label = strings.ToLower(candidate.Label)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// legacyFunctionCandidates: name-only function lists of dialects without a
// curated catalog (MySQL).
func (c *Completer) legacyFunctionCandidates(lower bool, keywords []string) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, k := range keywords {
		candidate := lsp.CompletionItem{
			Label:  k,
			Kind:   lsp.FunctionCompletion,
			Detail: "Function",
		}
		if lower {
			candidate.Label = strings.ToLower(candidate.Label)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// functionCatalog: the connection's functions, or the dialect's built-ins.
func (c *Completer) functionCatalog() *database.FunctionCatalog {
	if c.DBCache != nil && c.DBCache.Functions != nil {
		return c.DBCache.Functions
	}
	return database.NewFunctionCatalog(c.Driver, nil)
}

// functionCandidates lists the functions callable with qualifier ("" =
// unqualified). parenFollows: the cursor is right before "(", so only the
// name is inserted.
func (c *Completer) functionCandidates(qualifier string, lowercaseKeywords, parenFollows bool) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, group := range c.functionCatalog().Names(qualifier) {
		if len(group) == 0 {
			continue
		}
		candidates = append(candidates, c.functionItem(group, lowercaseKeywords, parenFollows))
	}
	return candidates
}

// LowercaseSignature renders f.Signature() with an upper-case name (Oracle)
// in lower case, as inserted with lowercaseIdentifiers.
func LowercaseSignature(f *dialect.Function) string {
	cp := *f
	if cp.Name == strings.ToUpper(cp.Name) {
		cp.Name = strings.ToLower(cp.Name)
	}
	return cp.Signature()
}

func (c *Completer) functionItem(group []*dialect.Function, lowercaseKeywords, parenFollows bool) lsp.CompletionItem {
	first := group[0]
	builtin := false
	noParens, noArgs := true, true
	for _, f := range group {
		builtin = builtin || f.Builtin
		if !f.NoParens {
			noParens = false
		}
		if f.NoParens || f.Args != "" || f.Suffix != "" {
			noArgs = false
		}
	}
	name := first.Name
	switch {
	case builtin && lowercaseKeywords:
		name = strings.ToLower(name)
	case builtin:
		name = strings.ToUpper(name)
	case c.LowercaseIdentifiers && name == strings.ToUpper(name):
		name = strings.ToLower(name)
	}
	detail := first.Signature()
	if !builtin && c.LowercaseIdentifiers {
		detail = LowercaseSignature(first)
	}
	if len(group) > 1 {
		detail += fmt.Sprintf("  (+%d)", len(group)-1)
	}
	item := lsp.CompletionItem{
		Label:  name,
		Kind:   lsp.FunctionCompletion,
		Detail: detail,
		Documentation: &lsp.MarkupContent{
			Kind:  lsp.Markdown,
			Value: database.FunctionDoc(group),
		},
	}
	switch {
	case noParens || parenFollows:
		item.InsertText = name
	case noArgs:
		item.InsertText = name + "()"
	default:
		item.InsertText = name + "($0)"
		item.InsertTextFormat = lsp.SnippetTextFormat
	}
	return item
}

func (c *Completer) columnCandidates(targetTables []*parseutil.TableInfo, parent *completionParent) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}

	switch parent.Type {
	case ParentTypeNone:
		for _, table := range targetTables {
			if table.Outer {
				// enclosing query: qualify to use (keeps unqualified lists short)
				continue
			}
			if table.DatabaseSchema != "" && table.Name != "" {
				columns, ok := c.DBCache.ColumnDatabase(table.DatabaseSchema, table.Name)
				if !ok {
					continue
				}
				candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
			} else if table.Name != "" {
				columns, ok := c.DBCache.ColumnDescs(table.Name)
				if !ok {
					continue
				}
				candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
			}
		}
	case ParentTypeSchema:
		// pass
	case ParentTypeTable:
		for _, table := range targetTables {
			if !strings.EqualFold(table.Name, parent.Name) && !strings.EqualFold(table.Alias, parent.Name) {
				continue
			}
			var (
				columns []*database.ColumnDesc
				ok      bool
			)
			if table.DatabaseSchema != "" {
				// schema-qualified reference: look up in that schema, not the default one
				columns, ok = c.DBCache.ColumnDatabase(table.DatabaseSchema, table.Name)
			} else {
				columns, ok = c.DBCache.ColumnDescs(table.Name)
			}
			if !ok {
				continue
			}
			candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
		}
	case ParentTypeSubQuery:
		// pass
	}
	return candidates
}

func generateColumnCandidates(tableName string, columns []*database.ColumnDesc) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, column := range columns {
		candidate := lsp.CompletionItem{
			Label:  column.Name,
			Kind:   lsp.FieldCompletion,
			Detail: columnDetail(tableName),
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.ColumnDoc(tableName, column),
			},
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func columnDetail(tableName string) string {
	detail := strings.Join(
		[]string{
			"column from",
			"\"" + tableName + "\"",
		},
		" ",
	)
	return detail
}

func (c *Completer) ReferencedTableCandidates(targetTables []*parseutil.TableInfo) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}

	for _, targetTable := range targetTables {
		includeTables := []*parseutil.TableInfo{}
		for _, table := range c.DBCache.SortedTables() {
			if strings.EqualFold(table, targetTable.Name) {
				includeTables = append(includeTables, targetTable)
			}
		}
		genCands := generateTableCandidatesByInfos(includeTables, c.DBCache)
		candidates = append(candidates, genCands...)
	}
	return candidates
}

func (c *Completer) TableCandidates(parent *completionParent, targetTables []*parseutil.TableInfo) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}

	switch parent.Type {
	case ParentTypeNone:
		excludeTables := []string{}
		for _, table := range c.DBCache.SortedTables() {
			isExclude := false
			for _, targetTable := range targetTables {
				if strings.EqualFold(table, targetTable.Name) {
					isExclude = true
				}
			}
			if isExclude {
				continue
			}
			excludeTables = append(excludeTables, table)
		}
		candidates = append(candidates, generateTableCandidates(excludeTables, c.DBCache)...)
	case ParentTypeSchema:
		tables, ok := c.DBCache.SortedTablesByDBName(parent.Name)
		if ok {
			candidates = append(candidates, generateTableCandidates(tables, c.DBCache)...)
		}
	case ParentTypeTable:
		// pass
	case ParentTypeSubQuery:
		// pass
	}
	return candidates
}

func (c *Completer) joinCandidates(lastTable *parseutil.TableInfo,
	targetTables, allTables []*parseutil.TableInfo,
	joinOn, lowercaseKeywords bool) []lsp.CompletionItem {
	var candidates []lsp.CompletionItem
	joinKeys := c.DBCache.JoinKeys(!c.HideMaybe)
	if len(joinKeys) == 0 {
		return candidates
	}

	// FK maps are keyed by the database's spelling of table names.
	lastTable = c.canonicalTable(lastTable)
	canonTargets := make([]*parseutil.TableInfo, len(targetTables))
	for i, t := range targetTables {
		canonTargets[i] = c.canonicalTable(t)
	}
	targetTables = canonTargets

	tMap := make(map[string]*parseutil.TableInfo)
	for _, t := range targetTables {
		tMap[t.Name] = t
	}
	fkMap := make(map[string][][]*database.ForeignKey)
	if lastTable == nil {
		for t := range tMap {
			for k, v := range joinKeys[t] {
				fkMap[k] = append(fkMap[k], v)
			}
		}
	} else {
		delete(tMap, lastTable.Name)
		rTab := []*parseutil.TableInfo{lastTable}
		if !joinOn {
			rTab = resolveTables(lastTable, c.DBCache)
		}
		for _, lt := range rTab {
			for k, v := range joinKeys[lt.Name] {
				if _, ok := tMap[k]; ok {
					fkMap[lt.Name] = append(fkMap[lt.Name], v)
				}
			}
		}

		for _, t := range rTab {
			if _, ok := tMap[t.Name]; !ok {
				tMap[t.Name] = t
			}
		}
	}

	aliases := make(map[string]interface{})
	for _, t := range allTables {
		if t.Alias != "" {
			aliases[t.Alias] = true
		}
	}

	for k, v := range fkMap {
		for _, fks := range v {
			for _, fk := range fks {
				item := generateForeignKeyCandidate(k, tMap, aliases, fk, joinOn, lowercaseKeywords)
				if e := c.DBCache.Relations.EdgeOf(fk); e != nil {
					describeJoinEdge(&item, e)
				}
				candidates = append(candidates, item)
			}
		}
	}
	return candidates
}

// describeJoinEdge labels a join snippet by where the condition comes from;
// guesses sort after the others.
func describeJoinEdge(item *lsp.CompletionItem, e *database.Edge) {
	switch {
	case e.Inferred:
		item.Detail = "Join generator: maybe (guessed from column names)"
		item.SortText = "00~~" // after the snippets of foreign keys ("00")
	case e.Kind == database.EdgeViaView:
		item.Detail = "Join generator: view column from " + e.Via
		item.SortText = "00~"
	}
}

// canonicalTable returns a copy of t whose Name uses the cached spelling.
func (c *Completer) canonicalTable(t *parseutil.TableInfo) *parseutil.TableInfo {
	if t == nil {
		return nil
	}
	if n, ok := c.DBCache.CanonicalTableName(t.Name); ok && n != t.Name {
		cp := *t
		cp.Name = n
		return &cp
	}
	return t
}

func resolveTables(t *parseutil.TableInfo, cache *database.DBCache) []*parseutil.TableInfo {
	if _, ok := cache.ColumnDescs(t.Name); ok {
		return []*parseutil.TableInfo{t}
	}
	var rv []*parseutil.TableInfo
	targetName := strings.ToLower(t.Name)
	for _, cond := range cache.SortedTables() {
		if strings.Contains(strings.ToLower(cond), targetName) {
			rv = append(rv, &parseutil.TableInfo{
				Name: cond,
			})
		}
	}
	return rv
}

func generateTableAlias(target string,
	aliases map[string]interface{}) string {
	ch := []rune(target)[0]
	i := 1
	var rv string
	for {
		rv = fmt.Sprintf("%c%d", ch, i)
		if _, ok := aliases[rv]; ok {
			i++
			continue
		}
		break
	}
	return rv
}

func generateForeignKeyCandidate(target string,
	tMap map[string]*parseutil.TableInfo,
	aliases map[string]interface{},
	fk *database.ForeignKey,
	joinOn, lowercaseKeywords bool) lsp.CompletionItem {
	var tAlias string
	if joinOn {
		tAlias = tMap[target].Alias
		if tAlias == "" {
			tAlias = tMap[target].Name
		}
	} else {
		tAlias = generateTableAlias(target, aliases)
	}
	builder := []struct {
		sb    *strings.Builder
		alias string
	}{
		{
			sb:    &strings.Builder{},
			alias: tAlias,
		},
		{
			sb:    &strings.Builder{},
			alias: tAlias,
		},
	}
	if !joinOn {
		builder[1].alias = fmt.Sprintf("${1:%s}", tAlias)
		onKw := "ON"
		if lowercaseKeywords {
			onKw = "on"
		}
		for _, b := range builder {
			fmt.Fprintf(b.sb, "%s %s %s ", target, b.alias, onKw)
		}
	}
	andKw := " AND "
	if lowercaseKeywords {
		andKw = " and "
	}
	prefix := ""
	for _, cur := range *fk {
		tIdx, rIdx := 0, 1
		if cur[rIdx].Table == target {
			tIdx, rIdx = rIdx, tIdx
		}
		for _, b := range builder {
			b.sb.WriteString(prefix)
		}
		prefix = andKw
		for _, b := range builder {
			b.sb.WriteString(strings.Join([]string{b.alias, cur[tIdx].Name}, "."))
			b.sb.WriteString(" = ")
		}
		rAlias := tMap[cur[rIdx].Table].Alias
		if rAlias == "" {
			rAlias = cur[rIdx].Table
		}
		for _, b := range builder {
			b.sb.WriteString(strings.Join([]string{rAlias, cur[rIdx].Name}, "."))
		}
	}
	builder[1].sb.WriteString("$0")
	return lsp.CompletionItem{
		Label:            builder[0].sb.String(),
		Kind:             lsp.SnippetCompletion,
		Detail:           "Join generator for foreign key",
		InsertText:       builder[1].sb.String(),
		InsertTextFormat: lsp.SnippetTextFormat,
	}
}

func generateTableCandidates(tables []string, dbCache *database.DBCache) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, tableName := range tables {
		candidate := lsp.CompletionItem{
			Label:  tableName,
			Kind:   lsp.ClassCompletion,
			Detail: string(dbCache.Relations.Kind(tableName)),
		}
		cols, ok := dbCache.ColumnDescs(tableName)
		if ok {
			candidate.Documentation = &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: dbCache.TableDoc(tableName, cols),
			}
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func generateTableCandidatesByInfos(tables []*parseutil.TableInfo, dbCache *database.DBCache) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, table := range tables {
		name := table.Name
		detail := "referenced table"
		if table.Alias != "" {
			name = table.Alias
			detail = "aliased table"
		}
		candidate := lsp.CompletionItem{
			Label:  name,
			Kind:   lsp.ClassCompletion,
			Detail: detail,
		}
		cols, ok := dbCache.ColumnDescs(table.Name)
		if ok {
			candidate.Documentation = &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.TableDoc(table.Name, cols),
			}
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// SubQueryCandidates: the names of the derived tables and CTE references in
// scope, as qualifiers.
func (c *Completer) SubQueryCandidates(infos []*parseutil.SubQueryInfo) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	seen := map[string]bool{}
	for _, info := range infos {
		if info.Name == "" || seen[strings.ToUpper(info.Name)] {
			continue
		}
		seen[strings.ToUpper(info.Name)] = true
		detail := "subquery"
		if info.Kind == parseutil.SubQueryCTE {
			detail = "CTE"
			if !strings.EqualFold(info.CTE, info.Name) {
				detail = "CTE " + info.CTE
			}
		}
		candidates = append(candidates, lsp.CompletionItem{
			Label:  info.Name,
			Kind:   lsp.ClassCompletion,
			Detail: detail,
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.RelationDoc(info, c.DBCache),
			},
		})
	}
	return candidates
}

// CTECandidates: the WITH queries visible at the position, as table names.
func (c *Completer) CTECandidates(ctes []*parseutil.SubQueryInfo) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, info := range ctes {
		candidates = append(candidates, lsp.CompletionItem{
			Label:    info.Name,
			Kind:     lsp.ClassCompletion,
			SortText: "05", // before the database's tables
			Detail:   "CTE",
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.RelationDoc(info, c.DBCache),
			},
		})
	}
	return candidates
}

// SubQueryColumnCandidates: the columns of the derived tables and CTE
// references in scope; with a qualifier, only those of that relation.
func (c *Completer) SubQueryColumnCandidates(infos []*parseutil.SubQueryInfo, parent *completionParent) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, info := range infos {
		switch parent.Type {
		case ParentTypeTable:
			if !strings.EqualFold(info.Name, parent.Name) {
				continue
			}
		case ParentTypeNone:
			if info.Outer {
				continue
			}
		default:
			continue
		}
		detail := subQueryColumnDetail(info)
		for _, col := range database.RelationColumns(info, c.DBCache) {
			candidates = append(candidates, lsp.CompletionItem{
				Label:  col.Name,
				Kind:   lsp.FieldCompletion,
				Detail: detail,
				Documentation: &lsp.MarkupContent{
					Kind:  lsp.Markdown,
					Value: database.RelationColumnDoc(info, col),
				},
			})
		}
	}
	return candidates
}

func subQueryColumnDetail(info *parseutil.SubQueryInfo) string {
	if info.Kind == parseutil.SubQueryCTE {
		return "column from CTE \"" + info.Name + "\""
	}
	if info.Name == "" {
		return "subquery column"
	}
	return "subquery column from \"" + info.Name + "\""
}

func (c *Completer) SchemaCandidates() []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	dbs := c.DBCache.SortedSchemas()
	for _, db := range dbs {
		candidate := lsp.CompletionItem{
			Label:  db,
			Kind:   lsp.ModuleCompletion,
			Detail: "schema",
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}
