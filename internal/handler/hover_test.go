package handler

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/database"
	"github.com/raikhan/sqlh/internal/lsp"
)

var hoverTestCases = []struct {
	name   string
	input  string
	output string
	line   int
	col    int
}{
	{
		name:   "not found head",
		input:  "SELECT ID, Name FROM city",
		output: "",
		line:   0,
		col:    7,
	},
	{
		name:   "not found tail",
		input:  "SELECT ID, Name FROM city",
		output: "",
		line:   0,
		col:    16,
	},
	{
		name:   "not found duplicate ident",
		input:  "SELECT Name FROM city, country",
		output: "",
		line:   0,
		col:    9,
	},
	{
		name:   "select ident head",
		input:  "SELECT ID, Name FROM city",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    8,
	},
	{
		name:   "select ident tail",
		input:  "SELECT ID, Name FROM city",
		output: "`city`.`Name` column\n\n`char(35)`\n",
		line:   0,
		col:    15,
	},
	{
		name:   "select quoted ident head",
		input:  "SELECT `ID`, Name FROM city",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    8,
	},
	{
		name:   "select quoted ident head",
		input:  "SELECT `ID`, Name FROM city",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    11,
	},
	{
		name:   "table ident head",
		input:  "SELECT ID, Name FROM city",
		output: "`city` table\n\n```\nName         Type      PK   Default  Extra\n-----------  --------  ---  -------  --------------\nID           int(11)   PRI  <null>   auto_increment\nName         char(35)  -    -\nCountryCode  char(3)   MUL  -\nDistrict     char(20)  -    -\nPopulation   int(11)   -    -\n```\n\nRelationships\n\n→ `country`.Code = CountryCode\n",
		line:   0,
		col:    22,
	},
	{
		name:   "table ident tail",
		input:  "SELECT ID, Name FROM city",
		output: "`city` table\n\n```\nName         Type      PK   Default  Extra\n-----------  --------  ---  -------  --------------\nID           int(11)   PRI  <null>   auto_increment\nName         char(35)  -    -\nCountryCode  char(3)   MUL  -\nDistrict     char(20)  -    -\nPopulation   int(11)   -    -\n```\n\nRelationships\n\n→ `country`.Code = CountryCode\n",
		line:   0,
		col:    25,
	},
	{
		name:   "select member ident parent head",
		input:  "SELECT city.ID, city.Name FROM city",
		output: "`city` table\n\n```\nName         Type      PK   Default  Extra\n-----------  --------  ---  -------  --------------\nID           int(11)   PRI  <null>   auto_increment\nName         char(35)  -    -\nCountryCode  char(3)   MUL  -\nDistrict     char(20)  -    -\nPopulation   int(11)   -    -\n```\n\nRelationships\n\n→ `country`.Code = CountryCode\n",
		line:   0,
		col:    8,
	},
	{
		name:   "select member ident parent tail",
		input:  "SELECT city.ID, city.Name FROM city",
		output: "`city` table\n\n```\nName         Type      PK   Default  Extra\n-----------  --------  ---  -------  --------------\nID           int(11)   PRI  <null>   auto_increment\nName         char(35)  -    -\nCountryCode  char(3)   MUL  -\nDistrict     char(20)  -    -\nPopulation   int(11)   -    -\n```\n\nRelationships\n\n→ `country`.Code = CountryCode\n",
		line:   0,
		col:    20,
	},
	{
		name:   "select member ident child dot",
		input:  "SELECT city.ID, city.Name FROM city",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    12,
	},
	{
		name:   "select member ident child head",
		input:  "SELECT city.ID, city.Name FROM city",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    13,
	},
	{
		name:   "select member ident child tail",
		input:  "SELECT city.ID, city.Name FROM city",
		output: "`city`.`Name` column\n\n`char(35)`\n",
		line:   0,
		col:    25,
	},
	{
		name:   "select aliased member ident parent",
		input:  "SELECT ci.ID, ci.Name FROM city AS ci",
		output: "`city` table\n\n```\nName         Type      PK   Default  Extra\n-----------  --------  ---  -------  --------------\nID           int(11)   PRI  <null>   auto_increment\nName         char(35)  -    -\nCountryCode  char(3)   MUL  -\nDistrict     char(20)  -    -\nPopulation   int(11)   -    -\n```\n\nRelationships\n\n→ `country`.Code = CountryCode\n",
		line:   0,
		col:    8,
	},
	{
		name:   "select aliased member ident child",
		input:  "SELECT ci.ID, ci.Name FROM city AS ci",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    10,
	},
	{
		name:   "select subquery ident parent",
		input:  "SELECT ID, Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		output: "`it`.`ID` column (subquery)\n\n`int(11)` PRI auto_increment\n\nfrom `city`.`ID`\n",
		line:   0,
		col:    8,
	},
	{
		name:   "select subquery member ident parent",
		input:  "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		output: "`it` subquery\n\n```\nName         Type      From\n-----------  --------  ----------------\nID           int(11)   city.ID\nName         char(35)  city.Name\nCountryCode  char(3)   city.CountryCode\nDistrict     char(20)  city.District\nPopulation   int(11)   city.Population\n```\n",
		line:   0,
		col:    8,
	},
	{
		name:   "select subquery member ident child",
		input:  "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		output: "`it`.`ID` column (subquery)\n\n`int(11)` PRI auto_increment\n\nfrom `city`.`ID`\n",
		line:   0,
		col:    11,
	},
	{
		name:   "select subquery with asterisk ident parent",
		input:  "SELECT it.ID, it.Name FROM (SELECT * FROM city) as it",
		output: "`it` subquery\n\n```\nName         Type      From\n-----------  --------  ----------------\nID           int(11)   city.ID\nName         char(35)  city.Name\nCountryCode  char(3)   city.CountryCode\nDistrict     char(20)  city.District\nPopulation   int(11)   city.Population\n```\n",
		line:   0,
		col:    8,
	},
	{
		name:   "select subquery with asterisk ident child",
		input:  "SELECT it.ID, it.Name FROM (SELECT * FROM city) as it",
		output: "`it`.`ID` column (subquery)\n\n`int(11)` PRI auto_increment\n\nfrom `city`.`ID`\n",
		line:   0,
		col:    11,
	},
	{
		name:   "select aliased select identifier",
		input:  "SELECT ID AS city_id, Name AS city_name FROM city",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    14,
	},
	{
		name:   "select aliased select member identifier",
		input:  "SELECT city.ID AS city_id, city.Name AS city_name FROM city",
		output: "`city`.`ID` column\n\n`int(11)` PRI auto_increment\n",
		line:   0,
		col:    19,
	},
	{
		name: "multi line head",
		input: `SELECT
  ID,
  Name
FROM city
`,
		output: "`city`.`Name` column\n\n`char(35)`\n",
		line:   2,
		col:    3,
	},
	{
		name: "multi line tail",
		input: `SELECT
  ID,
  Name
FROM city
`,
		output: "`city`.`Name` column\n\n`char(35)`\n",
		line:   2,
		col:    6,
	},
}

func TestHoverMain(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range hoverTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			hoverParams := lsp.HoverParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: testFileURI,
					},
					Position: lsp.Position{
						Line:      tt.line,
						Character: tt.col - 1,
					},
				},
			}
			var got lsp.Hover
			err := tx.conn.Call(tx.ctx, "textDocument/hover", hoverParams, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/hover: %+v", err)
				return
			}

			if tt.output == "" && got.Contents.Value != "" {
				t.Errorf("found hover, %q", got.Contents.Value)
				return
			}
			if diff := cmp.Diff(tt.output, got.Contents.Value); diff != "" {
				t.Errorf("unmatch hover contents (- want, + got):\n%s", diff)
			}
		})
	}
}

func TestHoverNoneDBConnection(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range hoverTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			hoverParams := lsp.HoverParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: testFileURI,
					},
					Position: lsp.Position{
						Line:      tt.line,
						Character: tt.col - 1,
					},
				},
			}
			// Without a DB connection, it is not possible to provide functions using the DB connection, so just make sure that no errors occur.
			var got lsp.Hover
			err := tx.conn.Call(tx.ctx, "textDocument/hover", hoverParams, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/hover: %+v", err)
				return
			}
		})
	}
}
