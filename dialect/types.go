package dialect

import "strings"

// DataType is a built-in column type.
type DataType struct {
	Name string // as written, upper-case ("DOUBLE PRECISION")
	// Params: its parameters, shown in the detail ("(n)", "(p, s)")
	Params string
	// Doc: a short description
	Doc string
}

// parseTypes reads "NAME|params|doc" lines.
func parseTypes(s string) []DataType {
	var out []DataType
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 3)
		for len(f) < 3 {
			f = append(f, "")
		}
		out = append(out, DataType{Name: f[0], Params: f[1], Doc: f[2]})
	}
	return out
}

var postgresTypes = parseTypes(`
INTEGER||4-byte signed integer
BIGINT||8-byte signed integer
SMALLINT||2-byte signed integer
SERIAL||autoincrementing 4-byte integer
BIGSERIAL||autoincrementing 8-byte integer
NUMERIC|(p, s)|exact number with selectable precision
DECIMAL|(p, s)|exact number (same as NUMERIC)
REAL||4-byte floating point
DOUBLE PRECISION||8-byte floating point
BOOLEAN||true/false
TEXT||variable-length string, unlimited
VARCHAR|(n)|variable-length string with a limit
CHAR|(n)|fixed-length, blank-padded string
CHARACTER VARYING|(n)|variable-length string with a limit
BYTEA||binary data
DATE||calendar date
TIME|(p)|time of day without time zone
TIMETZ|(p)|time of day with time zone
TIMESTAMP|(p)|date and time without time zone
TIMESTAMPTZ|(p)|date and time with time zone
INTERVAL|(fields) (p)|time span
UUID||universally unique identifier
JSON||JSON text
JSONB||binary JSON, indexable
XML||XML data
INET||IPv4 or IPv6 host address
CIDR||IPv4 or IPv6 network address
MACADDR||MAC address
MONEY||currency amount
BIT|(n)|fixed-length bit string
VARBIT|(n)|variable-length bit string
TSVECTOR||text search document
TSQUERY||text search query
POINT||geometric point
INT4RANGE||range of integer
INT8RANGE||range of bigint
NUMRANGE||range of numeric
TSRANGE||range of timestamp
TSTZRANGE||range of timestamptz
DATERANGE||range of date
OID||object identifier
REGCLASS||relation name
`)

var oracleTypes = parseTypes(`
VARCHAR2|(n [BYTE|CHAR])|variable-length string (up to 4000 bytes, 32767 extended)
NVARCHAR2|(n)|variable-length national character string
CHAR|(n [BYTE|CHAR])|fixed-length string
NCHAR|(n)|fixed-length national character string
NUMBER|(p, s)|number with precision p and scale s
INTEGER||NUMBER(38)
FLOAT|(p)|floating point NUMBER
BINARY_FLOAT||32-bit floating point
BINARY_DOUBLE||64-bit floating point
DATE||date and time to the second
TIMESTAMP|(p)|date and time with fractional seconds
TIMESTAMP WITH TIME ZONE|(p)|timestamp with a time zone
TIMESTAMP WITH LOCAL TIME ZONE|(p)|timestamp normalized to the database time zone
INTERVAL YEAR TO MONTH|(p)|period in years and months
INTERVAL DAY TO SECOND|(p, s)|period in days, hours, minutes, seconds
CLOB||character large object
NCLOB||national character large object
BLOB||binary large object
BFILE||binary file outside the database
RAW|(n)|binary data (up to 2000 bytes)
LONG RAW||binary data (deprecated)
LONG||character data (deprecated)
ROWID||row address
UROWID|(n)|universal row address
XMLTYPE||XML data
JSON||JSON (21c+)
BOOLEAN||true/false (23ai+)
`)

var sqliteTypes = parseTypes(`
INTEGER||integer affinity (INTEGER PRIMARY KEY = rowid)
TEXT||text affinity
REAL||8-byte floating point
BLOB||stored as given
NUMERIC||numeric affinity
ANY||any value (STRICT tables)
INT||integer affinity
VARCHAR|(n)|text affinity (length not enforced)
BOOLEAN||numeric affinity (0/1)
DATE||numeric affinity (store ISO-8601 text or a number)
DATETIME||numeric affinity (store ISO-8601 text or a number)
TIMESTAMP||numeric affinity
DOUBLE||real affinity
FLOAT||real affinity
DECIMAL|(p, s)|numeric affinity
JSON||text affinity (json functions)
`)

var genericTypes = parseTypes(`
INTEGER||
INT||
SMALLINT||
BIGINT||
DECIMAL|(p, s)|
NUMERIC|(p, s)|
REAL||
FLOAT|(p)|
DOUBLE PRECISION||
CHAR|(n)|
VARCHAR|(n)|
CHARACTER VARYING|(n)|
TEXT||
BOOLEAN||
DATE||
TIME||
TIMESTAMP||
INTERVAL||
BLOB||
CLOB||
BINARY|(n)|
VARBINARY|(n)|
`)

// DataTypes lists the built-in column types of a database.
func DataTypes(driver DatabaseDriver) []DataType {
	switch driver {
	case DatabaseDriverPostgreSQL:
		return postgresTypes
	case DatabaseDriverOracle:
		return oracleTypes
	case DatabaseDriverSQLite3:
		return sqliteTypes
	}
	return genericTypes
}
