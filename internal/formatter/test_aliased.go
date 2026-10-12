package formatter

import (
	"fmt"
	"github.com/raikhan/sqlh/ast"
	"github.com/raikhan/sqlh/parser"
)

func TestAliased() {
	sql := "LEFT JOIN USER AS u"
	fmt.Println("SQL:", sql)
	parsed, err := parser.Parse(sql)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	env := &formatEnvironment{}
	formatted := Eval(parsed, env)

	opts := &ast.RenderOptions{
		LowerCase: false,
	}
	fmt.Println("Formatted:", formatted.Render(opts))
}
