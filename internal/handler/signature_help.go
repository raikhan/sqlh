package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/raikhan/sqlh/dialect"
	"github.com/raikhan/sqlh/internal/completer"

	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
	"github.com/raikhan/sqlh/parser"
	"github.com/raikhan/sqlh/parser/parseutil"
	"github.com/raikhan/sqlh/token"
	"github.com/sourcegraph/jsonrpc2"
)

func (s *Server) handleTextDocumentSignatureHelp(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.SignatureHelpParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	f, ok := s.files[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	var driver dialect.DatabaseDriver
	if s.dbConn != nil {
		driver = s.dbConn.Driver
	}
	cfg := s.getConfig()
	if res := FunctionSignatureHelp(f.Text, params, s.worker.Cache(), driver, cfg.LowercaseKeywords, cfg.LowercaseIdentifiers); res != nil {
		return res, nil
	}
	res, err := SignatureHelp(f.Text, params, s.worker.Cache())
	if err != nil {
		return nil, err
	}
	return res, nil
}

func SignatureHelp(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache) (*lsp.SignatureHelp, error) {
	if dbCache == nil {
		return nil, nil
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
	types := getSignatureHelpTypes(nodeWalker)

	switch {
	case signatureHelpIs(types, SignatureHelpTypeInsertValue):
		insert, err := parseutil.ExtractInsert(parsed, pos)
		if err != nil {
			return nil, err
		}
		if !insert.Enable() {
			return nil, err
		}

		table := insert.GetTable()
		cols := insert.GetColumns()
		paramIdx := insert.GetValues().GetIndex(pos)
		tableName := table.Name

		params := []lsp.ParameterInformation{}
		for _, col := range cols.GetIdentifiers() {
			colName := col.String()
			colDoc := ""
			colDesc, ok := dbCache.Column(tableName, colName)
			if ok {
				colDoc = colDesc.OnelineDesc()
			}
			p := lsp.ParameterInformation{
				Label:         colName,
				Documentation: colDoc,
			}
			params = append(params, p)
		}

		signatureLabel := fmt.Sprintf("%s (%s)", tableName, cols.String())
		sh := &lsp.SignatureHelp{
			Signatures: []lsp.SignatureInformation{
				{
					Label:         signatureLabel,
					Documentation: fmt.Sprintf("%s table columns", tableName),
					Parameters:    params,
				},
			},
			ActiveSignature: 0.0,
			ActiveParameter: float64(paramIdx),
		}
		return sh, nil
	default:
		// pass
		return nil, nil
	}
}

type signatureHelpType int

const (
	_ signatureHelpType = iota
	SignatureHelpTypeInsertValue
	SignatureHelpTypeUnknown = 99
)

func (sht signatureHelpType) String() string {
	switch sht {
	case SignatureHelpTypeInsertValue:
		return "InsertValue"
	default:
		return ""
	}
}

func getSignatureHelpTypes(nw *parseutil.NodeWalker) []signatureHelpType {
	syntaxPos := parseutil.CheckSyntaxPosition(nw)
	types := []signatureHelpType{}
	switch {
	case syntaxPos == parseutil.InsertValue:
		types = []signatureHelpType{
			SignatureHelpTypeInsertValue,
		}
	default:
		// pass
	}
	return types
}

func signatureHelpIs(types []signatureHelpType, expect signatureHelpType) bool {
	for _, t := range types {
		if t == expect {
			return true
		}
	}
	return false
}

// maxSignatures caps the overloads shown (PostgreSQL has dozens for some).
const maxSignatures = 30

// FunctionSignatureHelp: the signatures of the function whose argument list
// encloses the position, with the argument being typed highlighted. Nil when
// the position is not in a call of a known function.
func FunctionSignatureHelp(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, driver dialect.DatabaseDriver, lowercaseKeywords, lowercaseIdentifiers bool) *lsp.SignatureHelp {
	call, ok := completer.FunctionCallAt(text, params.Position.Line, params.Position.Character)
	if !ok {
		return nil
	}
	catalog := database.NewFunctionCatalog(driver, nil)
	if dbCache != nil && dbCache.Functions != nil {
		catalog = dbCache.Functions
	}
	fns := catalog.Lookup(call.Qualifier, call.Name)
	if len(fns) == 0 {
		return nil
	}
	if len(fns) > maxSignatures {
		fns = fns[:maxSignatures]
	}
	help := &lsp.SignatureHelp{ActiveSignature: -1}
	for i, f := range fns {
		sig, active := signatureInformation(f, call, lowercaseKeywords, lowercaseIdentifiers)
		help.Signatures = append(help.Signatures, sig)
		if help.ActiveSignature < 0 && active {
			help.ActiveSignature = float64(i)
			help.ActiveParameter = sig.ActiveParameter
		}
	}
	if help.ActiveSignature < 0 {
		help.ActiveSignature = 0
		help.ActiveParameter = help.Signatures[0].ActiveParameter
	}
	return help
}

// signatureInformation renders one overload; active: it can take the argument
// at call.ArgIndex.
func signatureInformation(f *dialect.Function, call completer.FunctionCall, lowercaseKeywords, lowercaseIdentifiers bool) (lsp.SignatureInformation, bool) {
	name := f.Name
	qualifier := f.Qualifier
	switch {
	case f.Builtin && lowercaseKeywords:
		name = strings.ToLower(name)
	case f.Builtin:
		name = strings.ToUpper(name)
	case lowercaseIdentifiers:
		if name == strings.ToUpper(name) {
			name = strings.ToLower(name)
		}
		if qualifier == strings.ToUpper(qualifier) {
			qualifier = strings.ToLower(qualifier)
		}
	}
	if qualifier != "" {
		name = qualifier + "." + name
	}
	label := name + "("
	var ps []lsp.ParameterInformation
	params := f.Params()
	for i, p := range params {
		if i > 0 {
			label += ", "
		}
		start := utf16Len(label)
		label += p
		ps = append(ps, lsp.ParameterInformation{Label: []int{start, utf16Len(label)}})
	}
	label += ")"
	if f.Suffix != "" {
		label += " " + f.Suffix
	}
	if f.Result != "" {
		label += " → " + f.Result
	}
	doc := f.Doc
	if f.Kind != dialect.FunctionScalar {
		doc = strings.TrimSpace(doc + " (" + f.Kind.String() + ")")
	}
	sig := lsp.SignatureInformation{Label: label, Documentation: doc, Parameters: ps}
	idx := call.ArgIndex
	active := idx < len(params) || (idx == 0 && len(params) == 0)
	if f.IsVariadic() && idx >= len(params) && len(params) > 0 {
		idx = len(params) - 1
		active = true
	}
	sig.ActiveParameter = float64(idx)
	return sig, active
}
