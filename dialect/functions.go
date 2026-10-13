package dialect

import (
	"strings"
)

type FunctionKind int

const (
	FunctionScalar FunctionKind = iota
	FunctionAggregate
	FunctionWindow
	FunctionProcedure
)

func (k FunctionKind) String() string {
	switch k {
	case FunctionAggregate:
		return "aggregate"
	case FunctionWindow:
		return "window function"
	case FunctionProcedure:
		return "procedure"
	default:
		return "function"
	}
}

// Function describes one signature of a callable SQL function.
type Function struct {
	// Qualifier the function must be called with: a schema (PostgreSQL), an
	// owner or a package (Oracle). "" = callable unqualified.
	Qualifier string
	Name      string
	// Args: the parameter list without parentheses ("" = none).
	Args string
	// Suffix: syntax after the argument list, e.g. WITHIN GROUP (ORDER BY …).
	Suffix string
	Result string
	Doc    string
	Kind   FunctionKind
	// NoParens: called without parentheses (CURRENT_DATE, SYSDATE).
	NoParens bool
	// Builtin: from the curated list rather than the database.
	Builtin bool
}

// Signature renders name(args) → result.
func (f *Function) Signature() string {
	var b strings.Builder
	b.WriteString(f.Name)
	if !f.NoParens {
		b.WriteString("(" + f.Args + ")")
	}
	if f.Suffix != "" {
		b.WriteString(" " + f.Suffix)
	}
	if f.Result != "" {
		b.WriteString(" → " + f.Result)
	}
	return b.String()
}

// Params splits Args into parameters at top-level commas.
func (f *Function) Params() []string {
	return SplitParams(f.Args)
}

// SplitParams splits a parameter list at commas outside parentheses and
// quotes. Optional groups "a [, b [, c]]" become separate parameters, shown as
// "[b]" and "[c]"; other brackets (text[], [LEADING | TRAILING]) are kept.
func SplitParams(args string) []string {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}
	var params []string
	var cur strings.Builder
	optional := false
	flush := func() {
		p := strings.TrimSpace(cur.String())
		cur.Reset()
		if p == "" {
			return
		}
		if optional {
			p = "[" + p + "]"
		}
		params = append(params, p)
	}
	depth := 0
	var quote rune
	var brackets []bool // true: an optional group "[, …]"
	rs := []rune(args)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == '[' && depth == 0:
			j := i + 1
			for j < len(rs) && rs[j] == ' ' {
				j++
			}
			if j < len(rs) && rs[j] == ',' {
				brackets = append(brackets, true)
				flush()
				optional = true
				i = j
				continue
			}
			brackets = append(brackets, false)
		case r == ']' && depth == 0 && len(brackets) > 0:
			group := brackets[len(brackets)-1]
			brackets = brackets[:len(brackets)-1]
			if group {
				continue
			}
		case r == ',' && depth == 0:
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return params
}

// IsVariadic: the last parameter repeats ("…", "...", "VARIADIC").
func (f *Function) IsVariadic() bool {
	ps := f.Params()
	if len(ps) == 0 {
		return false
	}
	last := strings.ToUpper(ps[len(ps)-1])
	return strings.Contains(last, "...") || strings.Contains(last, "…") || strings.HasPrefix(last, "VARIADIC")
}

// BuiltinFunctions returns the curated built-in functions of a dialect.
func BuiltinFunctions(driver DatabaseDriver) []*Function {
	switch driver {
	case DatabaseDriverPostgreSQL:
		return postgresBuiltins
	case DatabaseDriverOracle:
		return oracleBuiltins
	case DatabaseDriverSQLite3:
		return sqliteBuiltins
	}
	return nil
}

var (
	postgresBuiltins = parseFunctionTable(postgresFunctionTable)
	oracleBuiltins   = parseFunctionTable(oracleFunctionTable)
	sqliteBuiltins   = parseFunctionTable(sqliteFunctionTable)
)

// parseFunctionTable reads lines of the form
//
//	[agg|win] name(args) → result | description
//	name → result | description          (called without parentheses)
//
// Blank lines and lines starting with # are ignored.
func parseFunctionTable(table string) []*Function {
	var fns []*Function
	for _, line := range strings.Split(table, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := &Function{Builtin: true}
		switch {
		case strings.HasPrefix(line, "agg "):
			f.Kind = FunctionAggregate
			line = line[4:]
		case strings.HasPrefix(line, "win "):
			f.Kind = FunctionWindow
			line = line[4:]
		}
		if i := strings.LastIndex(line, " | "); i >= 0 {
			f.Doc = strings.TrimSpace(line[i+3:])
			line = line[:i]
		}
		if i := strings.Index(line, "→"); i >= 0 {
			f.Result = strings.TrimSpace(line[i+len("→"):])
			line = strings.TrimSpace(line[:i])
		}
		if i := strings.Index(line, "("); i >= 0 {
			f.Name = strings.TrimSpace(line[:i])
			end := matchingParen(line, i)
			f.Args = strings.TrimSpace(line[i+1 : end])
			if end+1 < len(line) {
				f.Suffix = strings.TrimSpace(line[end+1:])
			}
		} else {
			f.Name = strings.TrimSpace(line)
			f.NoParens = true
		}
		fns = append(fns, f)
	}
	return fns
}

// matchingParen returns the index of the parenthesis closing the one at open
// (len(s) when unbalanced).
func matchingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(s)
}

const postgresFunctionTable = `
# conditional (syntax forms, not in pg_proc)
coalesce(value, ...) → same type | first of its arguments that is not null
nullif(value1, value2) → same type | null if value1 = value2, else value1
greatest(value, ...) → same type | largest of the arguments (nulls ignored)
least(value, ...) → same type | smallest of the arguments (nulls ignored)
cast(expression AS type) → type | type conversion (also expression::type)
# date/time
current_date → date | current date (start of the transaction)
current_time → time with time zone | current time of day (start of the transaction)
current_timestamp → timestamp with time zone | current date and time (start of the transaction)
localtime → time | current time of day, without time zone
localtimestamp → timestamp | current date and time, without time zone
now() → timestamp with time zone | current date and time (start of the transaction)
clock_timestamp() → timestamp with time zone | current date and time (changes during the statement)
statement_timestamp() → timestamp with time zone | start of the current statement
extract(field FROM source) → numeric | subfield (year, month, day, hour, epoch, dow, …) of a date/time/interval
date_part(field text, source timestamp) → double precision | subfield of a date/time value (like extract)
date_trunc(field text, source timestamp [, time_zone text]) → timestamp | truncate to the precision of field ('day', 'month', …)
age(timestamp, timestamp) → interval | symbolic difference (years, months, days)
make_date(year int, month int, day int) → date | date from its parts
make_interval(years int, months int, weeks int, days int, hours int, mins int, secs double precision) → interval | interval from its parts
to_char(value, format text) → text | format a date/time or number as text
to_date(text, format text) → date | parse a date
to_timestamp(text, format text) → timestamp with time zone | parse a timestamp (or from Unix epoch: to_timestamp(double precision))
to_number(text, format text) → numeric | parse a number
generate_series(start, stop [, step]) → setof | series of values (numbers or timestamps)
# strings
length(text) → integer | number of characters
char_length(text) → integer | number of characters
lower(text) → text | lower case
upper(text) → text | upper case
initcap(text) → text | first letter of each word upper case
trim([LEADING | TRAILING | BOTH] [characters] FROM string) → text | remove characters (default spaces) from the ends
btrim(string text [, characters text]) → text | remove characters from both ends
ltrim(string text [, characters text]) → text | remove characters from the start
rtrim(string text [, characters text]) → text | remove characters from the end
substring(string FROM start FOR count) → text | extract a substring (also substring(string, start, count), substring(string FROM pattern))
substr(string text, start int [, count int]) → text | extract a substring
left(string text, n int) → text | first n characters
right(string text, n int) → text | last n characters
position(substring IN string) → integer | location of substring (1-based, 0 if absent)
strpos(string text, substring text) → integer | location of substring (1-based, 0 if absent)
replace(string text, from text, to text) → text | replace all occurrences
translate(string text, from text, to text) → text | replace characters by position
concat(value, ...) → text | concatenate, ignoring nulls
concat_ws(separator text, value, ...) → text | concatenate with a separator, ignoring nulls
format(formatstr text, value, ...) → text | sprintf-like formatting (%s, %I, %L)
lpad(string text, length int [, fill text]) → text | pad on the left
rpad(string text, length int [, fill text]) → text | pad on the right
repeat(string text, number int) → text | repeat a string
reverse(text) → text | reverse a string
split_part(string text, delimiter text, n int) → text | n-th field (negative: from the end)
starts_with(string text, prefix text) → boolean | string starts with prefix
regexp_replace(string text, pattern text, replacement text [, flags text]) → text | replace regex matches ('g' flag: all)
regexp_match(string text, pattern text [, flags text]) → text[] | captures of the first match
regexp_matches(string text, pattern text [, flags text]) → setof text[] | captures of each match ('g' flag)
regexp_split_to_array(string text, pattern text [, flags text]) → text[] | split at regex matches
regexp_split_to_table(string text, pattern text [, flags text]) → setof text | split at regex matches
md5(text) → text | MD5 hash, hex
quote_ident(text) → text | quote as an identifier if needed
quote_literal(text) → text | quote as a string literal
quote_nullable(text) → text | quote as a literal, NULL for null
# numbers
abs(x) → same type | absolute value
round(x [, digits int]) → numeric | round
trunc(x [, digits int]) → numeric | truncate toward zero
ceil(x) → numeric | smallest integer not less than x
floor(x) → numeric | largest integer not greater than x
mod(y, x) → same type | remainder of y/x
power(a, b) → double precision | a raised to b
sqrt(x) → double precision | square root
random() → double precision | random value in [0, 1)
# arrays, json
array_agg(expression [ORDER BY …]) → array | collect values into an array
array_length(array, dimension int) → integer | length of a dimension
array_to_string(array, delimiter text [, null_string text]) → text | join array elements
string_to_array(string text, delimiter text [, null_string text]) → text[] | split into an array
unnest(array) → setof | expand an array to rows
cardinality(array) → integer | total number of elements
json_build_object(key, value, ...) → json | object from alternating keys and values
jsonb_build_object(key, value, ...) → jsonb | object from alternating keys and values
jsonb_build_array(value, ...) → jsonb | array from the arguments
to_jsonb(value) → jsonb | convert any value to jsonb
jsonb_array_elements(jsonb) → setof jsonb | expand a JSON array to rows
jsonb_array_elements_text(jsonb) → setof text | expand a JSON array to rows of text
jsonb_array_length(jsonb) → integer | number of elements of a JSON array
jsonb_each(jsonb) → setof (key text, value jsonb) | expand an object to key/value rows
jsonb_each_text(jsonb) → setof (key text, value text) | expand an object to key/value rows
jsonb_object_keys(jsonb) → setof text | keys of an object
jsonb_typeof(jsonb) → text | object, array, string, number, boolean or null
jsonb_set(target jsonb, path text[], new_value jsonb [, create_if_missing boolean]) → jsonb | replace the item at path
jsonb_strip_nulls(jsonb) → jsonb | remove object fields that are null
jsonb_pretty(jsonb) → text | indented JSON text
jsonb_path_query(target jsonb, path jsonpath [, vars jsonb [, silent boolean]]) → setof jsonb | items matched by a JSON path
jsonb_path_query_array(target jsonb, path jsonpath [, vars jsonb [, silent boolean]]) → jsonb | items matched by a JSON path, as an array
jsonb_path_query_first(target jsonb, path jsonpath [, vars jsonb [, silent boolean]]) → jsonb | first item matched by a JSON path
jsonb_path_exists(target jsonb, path jsonpath [, vars jsonb [, silent boolean]]) → boolean | a JSON path matches
# aggregates
agg count(expression | *) → bigint | number of rows (non-null values)
agg sum(expression) → numeric | sum of the values
agg avg(expression) → numeric | average of the values
agg min(expression) → same type | minimum value
agg max(expression) → same type | maximum value
agg string_agg(value text, delimiter text [ORDER BY …]) → text | concatenate values with a delimiter
agg json_agg(expression) → json | collect values into a JSON array
agg jsonb_agg(expression) → jsonb | collect values into a JSON array
agg jsonb_object_agg(key, value) → jsonb | collect key/value pairs into an object
agg bool_and(boolean) → boolean | true if all values are true
agg bool_or(boolean) → boolean | true if any value is true
agg every(boolean) → boolean | true if all values are true
agg stddev(expression) → numeric | sample standard deviation
agg variance(expression) → numeric | sample variance
agg percentile_cont(fraction) WITHIN GROUP (ORDER BY expression) → double precision | continuous percentile
agg percentile_disc(fraction) WITHIN GROUP (ORDER BY expression) → same type | discrete percentile
agg mode() WITHIN GROUP (ORDER BY expression) → same type | most frequent value
# window functions
win row_number() → bigint | number of the row within its partition, from 1
win rank() → bigint | rank with gaps
win dense_rank() → bigint | rank without gaps
win percent_rank() → double precision | relative rank (rank - 1) / (rows - 1)
win cume_dist() → double precision | cumulative distribution
win ntile(num_buckets int) → integer | bucket number from 1 to num_buckets
win lag(value [, offset int [, default]]) → same type | value of the row offset rows before
win lead(value [, offset int [, default]]) → same type | value of the row offset rows after
win first_value(value) → same type | value of the first row of the frame
win last_value(value) → same type | value of the last row of the frame
win nth_value(value, n int) → same type | value of the n-th row of the frame
# system
current_user → name | user name of the current execution context
session_user → name | session user name
current_schema → name | first schema of the search path
current_database() → name | name of the current database
pg_typeof(any) → regtype | data type of the value
pg_size_pretty(bigint) → text | human-readable size
pg_total_relation_size(regclass) → bigint | disk space of a table with indexes and TOAST
gen_random_uuid() → uuid | random version 4 UUID
`

const oracleFunctionTable = `
# null handling and conditionals
nvl(expr1, expr2) → type of expr1 | expr2 if expr1 is null, else expr1
nvl2(expr1, expr2, expr3) → type of expr2 | expr2 if expr1 is not null, else expr3
coalesce(expr, ...) → type of expr | first non-null argument
nullif(expr1, expr2) → type of expr1 | null if expr1 = expr2, else expr1
decode(expr, search, result, ..., default) → type of result | compare expr to each search, return the matching result
greatest(expr, ...) → type of expr | largest of the arguments (null if any is null)
least(expr, ...) → type of expr | smallest of the arguments (null if any is null)
lnnvl(condition) → boolean | true if the condition is false or unknown
# strings
length(char) → NUMBER | number of characters
lengthb(char) → NUMBER | number of bytes
lower(char) → VARCHAR2 | lower case
upper(char) → VARCHAR2 | upper case
initcap(char) → VARCHAR2 | first letter of each word upper case
substr(char, position [, substring_length]) → VARCHAR2 | extract a substring (negative position: from the end)
instr(string, substring [, position [, occurrence]]) → NUMBER | location of substring (0 if absent)
replace(char, search_string [, replacement_string]) → VARCHAR2 | replace (or remove) all occurrences
translate(expr, from_string, to_string) → VARCHAR2 | replace characters by position
trim([LEADING | TRAILING | BOTH] [trim_character] FROM trim_source) → VARCHAR2 | remove characters (default spaces) from the ends
ltrim(char [, set]) → VARCHAR2 | remove characters from the start
rtrim(char [, set]) → VARCHAR2 | remove characters from the end
lpad(expr1, n [, expr2]) → VARCHAR2 | pad on the left to length n
rpad(expr1, n [, expr2]) → VARCHAR2 | pad on the right to length n
concat(char1, char2) → VARCHAR2 | concatenate two strings (also ||)
chr(n) → VARCHAR2 | character with the given code
ascii(char) → NUMBER | code of the first character
regexp_like(source_char, pattern [, match_param]) → condition | string matches the regular expression
regexp_substr(source_char, pattern [, position [, occurrence [, match_param [, subexpr]]]]) → VARCHAR2 | substring matching the regular expression
regexp_instr(source_char, pattern [, position [, occurrence [, return_opt [, match_param [, subexpr]]]]]) → NUMBER | location of a regular expression match
regexp_replace(source_char, pattern [, replace_string [, position [, occurrence [, match_param]]]]) → VARCHAR2 | replace regular expression matches
regexp_count(source_char, pattern [, position [, match_param]]) → NUMBER | number of matches
soundex(char) → VARCHAR2 | phonetic representation
dump(expr [, return_fmt [, start_position [, length]]]) → VARCHAR2 | internal representation of a value
# numbers
abs(n) → NUMBER | absolute value
round(n [, integer]) → NUMBER | round a number (or a date to a unit: round(date, fmt))
trunc(n [, integer]) → NUMBER | truncate a number (or a date to a unit: trunc(date, fmt))
ceil(n) → NUMBER | smallest integer not less than n
floor(n) → NUMBER | largest integer not greater than n
mod(n2, n1) → NUMBER | remainder of n2/n1
remainder(n2, n1) → NUMBER | n2 - n1 * round(n2/n1)
power(n2, n1) → NUMBER | n2 raised to n1
sqrt(n) → NUMBER | square root
sign(n) → NUMBER | -1, 0 or 1
# conversion
to_char(expr [, fmt [, 'nlsparam']]) → VARCHAR2 | format a date or number as text
to_date(char [, fmt [, 'nlsparam']]) → DATE | parse a date
to_timestamp(char [, fmt [, 'nlsparam']]) → TIMESTAMP | parse a timestamp
to_number(expr [, fmt [, 'nlsparam']]) → NUMBER | parse a number
to_clob(char) → CLOB | convert to CLOB
cast(expr AS type_name) → type_name | type conversion
# dates
sysdate → DATE | current date and time of the database server
systimestamp → TIMESTAMP WITH TIME ZONE | current timestamp of the database server
current_date → DATE | current date and time in the session time zone
current_timestamp → TIMESTAMP WITH TIME ZONE | current timestamp in the session time zone
localtimestamp → TIMESTAMP | current timestamp in the session time zone, without time zone
add_months(date, integer) → DATE | date plus a number of months
months_between(date1, date2) → NUMBER | months between two dates
last_day(date) → DATE | last day of the month
next_day(date, char) → DATE | first named weekday after date
extract(field FROM expr) → NUMBER | year, month, day, hour, minute, second… of a date/timestamp/interval
numtodsinterval(n, 'interval_unit') → INTERVAL DAY TO SECOND | number to an interval ('DAY', 'HOUR', …)
numtoyminterval(n, 'interval_unit') → INTERVAL YEAR TO MONTH | number to an interval ('YEAR', 'MONTH')
# JSON
json_value(expr, path_expression [RETURNING type] [ON ERROR …]) → VARCHAR2 | scalar value at a JSON path
json_query(expr, path_expression [WITH WRAPPER] [ON ERROR …]) → VARCHAR2 | object or array at a JSON path
json_exists(expr, path_expression) → condition | a JSON path matches
json_object(KEY key VALUE value, ...) → VARCHAR2 | JSON object from key/value pairs
json_array(value, ...) → VARCHAR2 | JSON array from the arguments
json_table(expr, path_expression COLUMNS (…)) → rows | project JSON data as rows and columns
# misc
sys_guid() → RAW | globally unique identifier
user → VARCHAR2 | name of the session user
uid → NUMBER | id of the session user
sys_context('namespace', 'parameter' [, length]) → VARCHAR2 | context value, e.g. SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')
userenv('parameter') → VARCHAR2 | session information (legacy; prefer SYS_CONTEXT)
ora_hash(expr [, max_bucket [, seed_value]]) → NUMBER | hash value
standard_hash(expr [, 'method']) → RAW | SHA1 (default), SHA256, … hash
rownum → NUMBER | number of the row in the result (pseudo-column)
# aggregates
agg count(expr | *) → NUMBER | number of rows (non-null values)
agg sum(expr) → NUMBER | sum of the values
agg avg(expr) → NUMBER | average of the values
agg min(expr) → type of expr | minimum value
agg max(expr) → type of expr | maximum value
agg listagg(measure_expr [, 'delimiter']) WITHIN GROUP (ORDER BY …) → VARCHAR2 | concatenate values with a delimiter
agg median(expr) → NUMBER | median value
agg stddev(expr) → NUMBER | sample standard deviation
agg variance(expr) → NUMBER | sample variance
agg json_arrayagg(expr [ORDER BY …]) → VARCHAR2 | collect values into a JSON array
agg json_objectagg(KEY key VALUE value) → VARCHAR2 | collect key/value pairs into a JSON object
agg collect(expr) → collection | collect values into a nested table
# analytic
win row_number() OVER (…) → NUMBER | number of the row within its partition, from 1
win rank() OVER (…) → NUMBER | rank with gaps
win dense_rank() OVER (…) → NUMBER | rank without gaps
win ntile(expr) OVER (…) → NUMBER | bucket number
win lag(value_expr [, offset [, default]]) OVER (…) → type of value_expr | value of the row offset rows before
win lead(value_expr [, offset [, default]]) OVER (…) → type of value_expr | value of the row offset rows after
win first_value(expr [IGNORE NULLS]) OVER (…) → type of expr | value of the first row of the window
win last_value(expr [IGNORE NULLS]) OVER (…) → type of expr | value of the last row of the window
win nth_value(measure_expr, n) OVER (…) → type of measure_expr | value of the n-th row of the window
win ratio_to_report(expr) OVER (…) → NUMBER | value divided by the sum over the window
`

const sqliteFunctionTable = `
# core
coalesce(x, y, ...) → any | first non-null argument
ifnull(x, y) → any | y if x is null, else x
nullif(x, y) → any | null if x = y, else x
iif(condition, x, y) → any | x if condition is true, else y
abs(x) → number | absolute value
length(x) → integer | characters of a string, bytes of a blob
lower(x) → text | lower case (ASCII)
upper(x) → text | upper case (ASCII)
trim(x [, y]) → text | remove characters in y (default spaces) from both ends
ltrim(x [, y]) → text | remove characters from the start
rtrim(x [, y]) → text | remove characters from the end
substr(x, start [, length]) → text | substring (1-based)
substring(x, start [, length]) → text | substring (1-based)
instr(x, y) → integer | location of y in x (0 if absent)
replace(x, y, z) → text | replace all occurrences of y with z
printf(format, ...) → text | sprintf-like formatting
format(format, ...) → text | sprintf-like formatting
concat(x, ...) → text | concatenate, ignoring nulls
concat_ws(separator, x, ...) → text | concatenate with a separator, ignoring nulls
round(x [, digits]) → real | round
random() → integer | random 64-bit integer
hex(x) → text | hexadecimal rendering
quote(x) → text | SQL literal of the value
typeof(x) → text | null, integer, real, text or blob
unicode(x) → integer | code point of the first character
char(x, ...) → text | string from code points
min(x, y, ...) → any | smallest argument (scalar form)
max(x, y, ...) → any | largest argument (scalar form)
last_insert_rowid() → integer | rowid of the last insert
changes() → integer | rows changed by the last statement
glob(pattern, x) → integer | x matches the glob pattern
like(pattern, x [, escape]) → integer | x matches the LIKE pattern
cast(expr AS type) → type | type conversion
# date and time
date(time_value, modifier, ...) → text | date as YYYY-MM-DD
time(time_value, modifier, ...) → text | time as HH:MM:SS
datetime(time_value, modifier, ...) → text | YYYY-MM-DD HH:MM:SS
julianday(time_value, modifier, ...) → real | Julian day number
unixepoch(time_value, modifier, ...) → integer | seconds since 1970-01-01
strftime(format, time_value, modifier, ...) → text | formatted date/time
timediff(time_value1, time_value2) → text | difference as +YYYY-MM-DD HH:MM:SS.SSS
current_date → text | current date (UTC)
current_time → text | current time (UTC)
current_timestamp → text | current date and time (UTC)
# JSON
json(x) → text | minified JSON
json_extract(json, path, ...) → any | value(s) at the path(s)
json_array(value, ...) → text | JSON array
json_object(label, value, ...) → text | JSON object
json_array_length(json [, path]) → integer | number of elements
json_type(json [, path]) → text | type of the element
json_valid(json) → integer | 1 if well-formed JSON
json_set(json, path, value, ...) → text | insert or replace values
json_insert(json, path, value, ...) → text | insert values (no overwrite)
json_replace(json, path, value, ...) → text | replace existing values
json_remove(json, path, ...) → text | remove elements
json_each(json [, path]) → table | rows for the elements of an array/object
json_tree(json [, path]) → table | rows for all elements, recursively
# aggregates
agg count(x | *) → integer | number of rows (non-null values)
agg sum(x) → number | sum (error on overflow)
agg total(x) → real | sum as a float (0.0 for no rows)
agg avg(x) → real | average
agg min(x) → any | minimum value
agg max(x) → any | maximum value
agg group_concat(x [, separator]) → text | concatenate values
agg string_agg(x, separator) → text | concatenate values
agg json_group_array(x) → text | JSON array of the values
agg json_group_object(name, value) → text | JSON object of name/value pairs
# window functions
win row_number() → integer | number of the row within its partition, from 1
win rank() → integer | rank with gaps
win dense_rank() → integer | rank without gaps
win percent_rank() → real | relative rank
win cume_dist() → real | cumulative distribution
win ntile(n) → integer | bucket number
win lag(expr [, offset [, default]]) → any | value of the row offset rows before
win lead(expr [, offset [, default]]) → any | value of the row offset rows after
win first_value(expr) → any | value of the first row of the frame
win last_value(expr) → any | value of the last row of the frame
win nth_value(expr, n) → any | value of the n-th row of the frame
`
