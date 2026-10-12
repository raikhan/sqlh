package database

import (
	"regexp"
	"strings"

	"github.com/raikhan/sqlh/dialect"
)

var (
	oraPlainRe = regexp.MustCompile(`^[A-Z][A-Z0-9_$#]*$`)
	pgPlainRe  = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)
	anyPlainRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// QuoteIdent renders a name as it should be typed in SQL: plain when the
// database would fold an unquoted name to it, double-quoted otherwise.
// lower inserts Oracle's upper-case names in lower case (lowercaseIdentifiers).
func QuoteIdent(name string, driver dialect.DatabaseDriver, lower bool) string {
	switch driver {
	case dialect.DatabaseDriverOracle:
		if oraPlainRe.MatchString(name) {
			if lower {
				return strings.ToLower(name)
			}
			return name
		}
	case dialect.DatabaseDriverPostgreSQL:
		if pgPlainRe.MatchString(name) {
			return name
		}
	default: // SQLite and others: case-insensitive names
		if anyPlainRe.MatchString(name) {
			return name
		}
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
