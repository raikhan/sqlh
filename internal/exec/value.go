package exec

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/raikhan/sqlh/internal/sqlsplit"
)

// ValueFormatter renders driver values as text for CSV files and tables.
//
// Driver behaviour this is based on (probed against go-ora, pgx, go-sqlite3):
//   - Oracle NUMBER and PostgreSQL numeric arrive as exact decimal strings and
//     are passed through untouched (no float rounding).
//   - Oracle DATE/TIMESTAMP and PostgreSQL TIMESTAMP/DATE carry no time zone
//     but arrive as time.Time in UTC; they are printed as wall-clock values
//     without a fake "Z". Types with a time zone keep their offset.
//   - Binary types (RAW, BLOB, BYTEA) arrive as []byte; JSON arrives as
//     UTF-8 []byte and is printed as text.
type ValueFormatter struct {
	Dialect sqlsplit.Dialect
	// Null is written for SQL NULL (default ""). Note Oracle returns '' as NULL.
	Null string
}

const (
	layoutDate     = "2006-01-02"
	layoutDateTime = "2006-01-02 15:04:05.999999999"
	layoutTZ       = "2006-01-02 15:04:05.999999999-07:00"
)

// Format renders v. dbType is the column's DatabaseTypeName (may be empty).
func (f ValueFormatter) Format(v any, dbType string) string {
	switch x := v.(type) {
	case nil:
		return f.Null
	case string:
		return x
	case []byte:
		if isBinaryType(dbType) || !utf8.Valid(x) {
			return "0x" + hex.EncodeToString(x)
		}
		return string(x)
	case time.Time:
		return f.formatTime(x, dbType)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case bool:
		return strconv.FormatBool(x)
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

func isBinaryType(dbType string) bool {
	t := strings.ToUpper(dbType)
	for _, b := range []string{"RAW", "BLOB", "BYTEA", "BINARY", "IMAGE"} {
		if strings.Contains(t, b) {
			return true
		}
	}
	return false
}

func (f ValueFormatter) formatTime(t time.Time, dbType string) string {
	ut := strings.ToUpper(dbType)
	switch {
	case strings.Contains(ut, "TZ") || strings.Contains(ut, "TIME ZONE"):
		// an instant: keep the offset
		return t.Format(layoutTZ)
	case ut == "DATE" && f.Dialect != Oracle && isMidnight(t):
		// PostgreSQL/SQLite DATE is a calendar date. Oracle DATE has a time of
		// day, so it always prints one (consistent within a column).
		return t.Format(layoutDate)
	default:
		// no time zone: print the wall clock as stored
		return t.Format(layoutDateTime)
	}
}

func isMidnight(t time.Time) bool {
	return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
}

// Oracle is re-exported for readability in this package.
const Oracle = sqlsplit.Oracle
