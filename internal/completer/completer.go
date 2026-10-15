package completer

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/ast/astutil"
	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
)

type completionType int

const (
	_ completionType = iota
	CompletionTypeKeyword
	CompletionTypeFunction
	CompletionTypeColumn
	CompletionTypeTable
	CompletionTypeReferencedTable
	CompletionTypeView
	CompletionTypeSubQuery
	CompletionTypeSubQueryColumn
	CompletionTypeChange
	CompletionTypeUser
	CompletionTypeSchema
	CompletionTypeJoin
	CompletionTypeJoinOn
	CompletionTypeCTE
)

func (ct completionType) String() string {
	switch ct {
	case CompletionTypeKeyword:
		return "Keyword"
	case CompletionTypeFunction:
		return "Function"
	case CompletionTypeColumn:
		return "Column"
	case CompletionTypeTable:
		return "Table"
	case CompletionTypeReferencedTable:
		return "ReferencedTable"
	case CompletionTypeView:
		return "View"
	case CompletionTypeChange:
		return "Change"
	case CompletionTypeUser:
		return "User"
	case CompletionTypeSchema:
		return "Schema"
	case CompletionTypeSubQuery:
		return "SubQuery"
	case CompletionTypeSubQueryColumn:
		return "SubQueryColumn"
	case CompletionTypeJoin:
		return "Join clause"
	case CompletionTypeJoinOn:
		return "Join On condition"
	case CompletionTypeCTE:
		return "CTE"
	default:
		return ""
	}
}

type Completer struct {
	DBCache              *database.DBCache
	Driver               dialect.DatabaseDriver
	LowercaseIdentifiers bool
	// HideMaybe leaves out join conditions guessed from column names.
	HideMaybe bool
}

func NewCompleter(dbCache *database.DBCache) *Completer {
	return &Completer{
		DBCache: dbCache,
	}
}

func completionTypeIs(completionTypes []completionType, expect completionType) bool {
	for _, t := range completionTypes {
		if t == expect {
			return true
		}
	}
	return false
}

func (c *Completer) Complete(text string, params lsp.CompletionParams, lowercaseKeywords bool) ([]lsp.CompletionItem, error) {
	// A space is a trigger character so that a list of names comes up right
	// after DROP VIEW, FROM, … (editors only auto-complete after typed word
	// characters); everywhere else a space-triggered request gets nothing.
	spaceTrigger := isSpaceTrigger(params)
	// DDL statements and DML tails (RETURNING, ON CONFLICT, MERGE)
	if items, ok := c.ddlComplete(text, params, lowercaseKeywords, spaceTrigger); ok {
		return items, nil
	}
	parsed, err := parser.Parse(text)
	if err != nil {
		return nil, err
	}

	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character,
	}

	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	if spaceTrigger {
		switch parseutil.CheckSyntaxPosition(nodeWalker) {
		case parseutil.TableReference, parseutil.JoinClause:
		default:
			return []lsp.CompletionItem{}, nil
		}
	}
	ctx := getCompletionTypes(nodeWalker)
	if err != nil {
		return nil, err
	}

	definedTables, err := parseutil.ExtractTable(parsed, pos)
	if err != nil {
		return nil, err
	}
	definedSubQueries, err := parseutil.ExtractSubQueryViews(parsed, pos)
	if err != nil {
		return nil, err
	}
	var ctes []*parseutil.SubQueryInfo
	if completionTypeIs(ctx.types, CompletionTypeCTE) {
		if ctes, err = parseutil.ExtractCTEs(parsed, pos); err != nil {
			return nil, err
		}
	}

	lastWord := getLastWord(text, params.Position.Line+1, params.Position.Character)
	withBackQuote := strings.HasPrefix(lastWord, "`")

	var items []lsp.CompletionItem

	if c.DBCache != nil {
		if completionTypeIs(ctx.types, CompletionTypeColumn) {
			candidates := c.columnCandidates(definedTables, ctx.parent)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeReferencedTable) {
			candidates := c.ReferencedTableCandidates(definedTables)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeTable) {
			excl := definedTables
			if completionTypeIs(ctx.types, CompletionTypeJoin) {
				excl = nil
			}
			candidates := c.TableCandidates(ctx.parent, excl)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeSchema) {
			candidates := c.SchemaCandidates()
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeCTE) {
			candidates := c.CTECandidates(ctes)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeSubQuery) {
			candidates := c.SubQueryCandidates(definedSubQueries)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeSubQueryColumn) {
			candidates := c.SubQueryColumnCandidates(definedSubQueries, ctx.parent)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		joinOn := completionTypeIs(ctx.types, CompletionTypeJoinOn)
		if completionTypeIs(ctx.types, CompletionTypeJoin) || joinOn {
			table, err := parseutil.ExtractLastTable(parsed, pos)
			if err != nil {
				return nil, err
			}
			tables, err := parseutil.ExtractPrevTables(parsed, pos)
			if err != nil {
				return nil, err
			}
			candidates := c.joinCandidates(table, tables, definedTables, joinOn, lowercaseKeywords)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates) // what to do here?
			}
			items = append(candidates, items...)
		}
	}

	if completionTypeIs(ctx.types, CompletionTypeKeyword) {
		drivers := dialect.DataBaseKeywords(c.Driver)
		items = append(items, c.keywordCandidates(lowercaseKeywords, drivers)...)
	}
	if completionTypeIs(ctx.types, CompletionTypeFunction) {
		parenFollows := charAfterCursor(text, params.Position.Line, params.Position.Character) == '('
		switch {
		case ctx.parent.Type == ParentTypeTable:
			// pkg.function (Oracle packages, owners; PostgreSQL schemas)
			items = append(items, c.functionCandidates(ctx.parent.Name, lowercaseKeywords, parenFollows)...)
		case lastWord == "":
			// hundreds of functions: only once a prefix is typed
		case dialect.BuiltinFunctions(c.Driver) == nil && isMySQL(c.Driver):
			items = append(items, c.legacyFunctionCandidates(lowercaseKeywords, dialect.DataBaseFunctions(c.Driver))...)
		default:
			items = append(items, c.functionCandidates("", lowercaseKeywords, parenFollows)...)
		}
	}

	if c.LowercaseIdentifiers {
		lowercaseIdentifierItems(items)
	}
	items = filterCandidates(items, lastWord)
	populateSortText(items)

	return items, nil
}

// isSpaceTrigger: the request was triggered by typing a space.
func isSpaceTrigger(params lsp.CompletionParams) bool {
	tc := params.CompletionContext.TriggerCharacter
	return params.CompletionContext.TriggerKind == lsp.CompletionTriggerKindTriggerCharacter && tc != nil && *tc == " "
}

var upperIdentRe = regexp.MustCompile(`\b[A-Z][A-Z0-9_$#]*\b`)

// lowercaseUpperIdents lower-cases all-upper-case identifier tokens, leaving
// mixed-case tokens and the ON/AND keywords of join snippets alone.
func lowercaseUpperIdents(s string) string {
	return upperIdentRe.ReplaceAllStringFunc(s, func(tok string) string {
		if tok == "ON" || tok == "AND" {
			return tok
		}
		return strings.ToLower(tok)
	})
}

func lowercaseIdentifierItems(items []lsp.CompletionItem) {
	for i := range items {
		switch items[i].Kind {
		case lsp.FieldCompletion, lsp.ClassCompletion, lsp.ModuleCompletion,
			lsp.SnippetCompletion, lsp.VariableCompletion:
			items[i].Label = lowercaseUpperIdents(items[i].Label)
			items[i].InsertText = lowercaseUpperIdents(items[i].InsertText)
		}
	}
}

// Override the sort text for each completion item.
// A SortText set by a candidate generator is kept as the prefix (rank).
func populateSortText(items []lsp.CompletionItem) {
	for i := range items {
		prefix := items[i].SortText
		if prefix == "" {
			prefix = getSortTextPrefix(items[i].Kind)
		}
		items[i].SortText = prefix + items[i].Label
	}
}

// Some completion kinds are more relevant than others.
// This prefix defines the alphabetic priority of each kind.
func getSortTextPrefix(kind lsp.CompletionItemKind) string {
	switch kind {
	case lsp.SnippetCompletion:
		return "00"
	case lsp.FieldCompletion:
		return "0"
	case lsp.ClassCompletion:
		return "1"
	case lsp.ModuleCompletion:
		return "2"
	case lsp.FunctionCompletion:
		return "10"
	case
		lsp.ColorCompletion,
		lsp.ConstantCompletion,
		lsp.ConstructorCompletion,
		lsp.EnumCompletion,
		lsp.EnumMemberCompletion,
		lsp.EventCompletion,
		lsp.FileCompletion,
		lsp.FolderCompletion,
		lsp.InterfaceCompletion,
		lsp.KeywordCompletion,
		lsp.MethodCompletion,
		lsp.OperatorCompletion,
		lsp.PropertyCompletion,
		lsp.ReferenceCompletion,
		lsp.StructCompletion,
		lsp.TextCompletion,
		lsp.TypeParameterCompletion,
		lsp.UnitCompletion,
		lsp.ValueCompletion,
		lsp.VariableCompletion:
		return "9999"
	default:
		return "9999"
	}
}

type ParentType int

const (
	_ ParentType = iota
	ParentTypeNone
	ParentTypeSchema
	ParentTypeTable
	ParentTypeSubQuery
)

type completionParent struct {
	Type ParentType
	Name string
}

var noneParent = &completionParent{Type: ParentTypeNone}

type CompletionContext struct {
	types  []completionType
	parent *completionParent
}

func getCompletionTypes(nw *parseutil.NodeWalker) *CompletionContext {
	memberIdentifierMatcher := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier},
	}

	syntaxPos := parseutil.CheckSyntaxPosition(nw)
	var t []completionType
	p := noneParent
	switch {
	case syntaxPos == parseutil.ColName:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeSubQueryColumn,
				CompletionTypeView,
				CompletionTypeFunction,
			}
			p = &completionParent{
				Type: ParentTypeTable,
				Name: mi.Parent.String(),
			}
		} else {
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeSubQueryColumn,
				CompletionTypeSubQuery,
				CompletionTypeView,
				CompletionTypeFunction,
			}
			p = noneParent
		}
	case syntaxPos == parseutil.AliasName:
		// pass
	case syntaxPos == parseutil.SelectExpr || syntaxPos == parseutil.CaseValue:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeFunction,
			}
			p = &completionParent{
				Type: ParentTypeTable,
				Name: mi.ParentTok.NoQuoteString(),
			}
		} else {
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeSubQuery,
				CompletionTypeFunction,
			}
		}
	case syntaxPos == parseutil.TableReference:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				CompletionTypeTable,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
			}
			p = &completionParent{
				Type: ParentTypeSchema,
				Name: mi.ParentTok.NoQuoteString(),
			}
		} else {
			t = []completionType{
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeSchema,
				CompletionTypeView,
				CompletionTypeCTE,
			}
		}
	case syntaxPos == parseutil.WhereCondition:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeFunction,
			}
			p = &completionParent{
				Type: ParentTypeTable,
				Name: mi.ParentTok.NoQuoteString(),
			}
		} else {
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeSubQuery,
				CompletionTypeFunction,
			}
		}
	case syntaxPos == parseutil.JoinClause:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// "join schema." -> tables of that schema
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				CompletionTypeTable,
				CompletionTypeView,
			}
			p = &completionParent{
				Type: ParentTypeSchema,
				Name: mi.ParentTok.NoQuoteString(),
			}
		} else {
			t = []completionType{
				CompletionTypeJoin,
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeSchema,
				CompletionTypeView,
				CompletionTypeCTE,
			}
		}
	case syntaxPos == parseutil.JoinOn:
		t = []completionType{
			CompletionTypeJoinOn,
			CompletionTypeColumn,
			CompletionTypeReferencedTable,
			CompletionTypeSubQueryColumn,
			CompletionTypeSubQuery,
		}
	case syntaxPos == parseutil.InsertColumn:
		t = []completionType{
			CompletionTypeColumn,
			CompletionTypeView,
		}
	default:
		// elsewhere (PL/SQL blocks, DDL, …): keywords, and functions once a
		// prefix is typed; pkg.| lists the package's functions
		if nw.CurNodeIs(memberIdentifierMatcher) {
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{CompletionTypeFunction}
			p = &completionParent{Type: ParentTypeTable, Name: mi.Parent.String()}
		} else {
			t = []completionType{
				CompletionTypeKeyword,
				CompletionTypeFunction,
			}
		}
	}
	return &CompletionContext{
		types:  t,
		parent: p,
	}
}

func filterCandidates(candidates []lsp.CompletionItem, lastWord string) []lsp.CompletionItem {
	filtered := []lsp.CompletionItem{}
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToUpper(candidate.Label), strings.ToUpper(lastWord)) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func isMySQL(d dialect.DatabaseDriver) bool {
	switch d {
	case dialect.DatabaseDriverMySQL, dialect.DatabaseDriverMySQL8,
		dialect.DatabaseDriverMySQL57, dialect.DatabaseDriverMySQL56:
		return true
	}
	return false
}

// charAfterCursor returns the first non-space character after the cursor on
// its line (0 if none).
func charAfterCursor(text string, line, char int) rune {
	l := getLine(text, line+1)
	rest := l[byteOffsetForUTF16(l, char):]
	for _, r := range rest {
		if r != ' ' && r != '\t' {
			return r
		}
	}
	return 0
}

func getLine(text string, line int) string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	i := 1
	for scanner.Scan() {
		if i == line {
			return scanner.Text()
		}
		i++
	}
	return ""
}

func getLastWord(text string, line, char int) string {
	t := getBeforeCursorText(text, line, char)
	s := getLine(t, line)

	reg := regexp.MustCompile("[\\w`]+$")
	ss := reg.FindAllString(s, -1)
	if len(ss) == 0 {
		return ""
	}
	return ss[len(ss)-1]
}

func getBeforeCursorText(text string, line, char int) string {
	writer := bytes.NewBufferString("")
	scanner := bufio.NewScanner(strings.NewReader(text))

	i := 1
	for scanner.Scan() {
		if i == line {
			t := scanner.Text()
			writer.WriteString(t[:byteOffsetForUTF16(t, char)])
			break
		}
		fmt.Fprintln(writer, scanner.Text())
		i++
	}
	return writer.String()
}

// byteOffsetForUTF16 converts an LSP UTF-16 character offset to a byte
// offset in s, clamping to the end of the string.
func byteOffsetForUTF16(s string, char int) int {
	utf16Pos := 0
	for bytePos, r := range s {
		if utf16Pos >= char {
			return bytePos
		}
		utf16Pos++
		if r > 0xFFFF {
			utf16Pos++
		}
	}
	return len(s)
}

func toQuotedCandidates(candidates []lsp.CompletionItem) []lsp.CompletionItem {
	quotedCandidates := make([]lsp.CompletionItem, len(candidates))
	for i, candidate := range candidates {
		candidate.Label = fmt.Sprintf("`%s`", candidate.Label)
		quotedCandidates[i] = candidate
	}
	return quotedCandidates
}
