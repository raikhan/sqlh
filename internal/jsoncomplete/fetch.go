package jsoncomplete

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// SampleRows is how many non-null rows are sampled per column.
const SampleRows = 1000

// Target is a JSON column, in the database's exact spelling.
type Target struct {
	Schema, Table, Column string
}

// Key is one JSON object key found in the sample.
type Key struct {
	Path    []string // parent path (Oracle: from the dataguide)
	Name    string
	Types   []string // JSON types, most frequent first
	Count   int      // objects containing the key (PostgreSQL)
	Sampled int      // objects sampled at that path (PostgreSQL)
	// InArray counts occurrences inside elements of an array found at the
	// path (PostgreSQL); Count-InArray were in objects directly at the path.
	InArray int
}

// ArrayOnly: the key exists only inside array elements at the path, so
// path->>'key' is always NULL (an element must be chosen first).
func (k Key) ArrayOnly() bool { return k.InArray > 0 && k.InArray == k.Count }

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// pgTextArray renders a text[] literal: {"a","b"}.
func pgTextArray(path []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range path {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// PostgresKeys samples the keys of the objects at path in a json/jsonb
// column. Arrays at the path are looked through (keys of their elements).
func PostgresKeys(ctx context.Context, db querier, t Target, path []string) ([]Key, error) {
	q := fmt.Sprintf(`
with s as (
  select (%[1]s)::jsonb as j from %[2]s.%[3]s where %[1]s is not null limit %[4]d
), v as (
  select j #> $1::text[] as x from s
), o as (
  select x, false as elem from v where jsonb_typeof(x) = 'object'
  union all
  select e, true from v, jsonb_array_elements(case when jsonb_typeof(x) = 'array' then x else '[]'::jsonb end) e
  where jsonb_typeof(e) = 'object'
)
select k, jsonb_typeof(val), elem, count(*), (select count(*) from o)
  from o, jsonb_each(o.x) as t(k, val)
 group by 1, 2, 3`, pgIdent(t.Column), pgIdent(t.Schema), pgIdent(t.Table), SampleRows)
	rows, err := db.QueryContext(ctx, q, pgTextArray(path))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type agg struct {
		key     Key
		byType  map[string]int
		sampled int
	}
	m := map[string]*agg{}
	for rows.Next() {
		var k, typ string
		var elem bool
		var n, sampled int
		if err := rows.Scan(&k, &typ, &elem, &n, &sampled); err != nil {
			return nil, err
		}
		a := m[k]
		if a == nil {
			a = &agg{key: Key{Path: path, Name: k}, byType: map[string]int{}}
			m[k] = a
		}
		a.byType[typ] += n
		a.key.Count += n
		a.key.Sampled = sampled
		if elem {
			a.key.InArray += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Key, 0, len(m))
	for _, a := range m {
		for t := range a.byType {
			a.key.Types = append(a.key.Types, t)
		}
		sort.Slice(a.key.Types, func(i, j int) bool {
			bi, bj := a.byType[a.key.Types[i]], a.byType[a.key.Types[j]]
			if bi != bj {
				return bi > bj
			}
			return a.key.Types[i] < a.key.Types[j]
		})
		out = append(out, a.key)
	}
	sortKeys(out)
	return out, nil
}

func oraIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// OracleGuide samples a column holding JSON text with JSON_DATAGUIDE and
// returns every object key with its parent path.
func OracleGuide(ctx context.Context, db querier, t Target) ([]Key, error) {
	col := oraIdent(t.Column)
	q := fmt.Sprintf(`select json_dataguide(j) from (select %[1]s j from %[2]s.%[3]s
	  where %[1]s is not null and %[1]s is json and rownum <= %[4]d)`,
		col, oraIdent(t.Schema), oraIdent(t.Table), SampleRows)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var guide sql.NullString
	if rows.Next() {
		if err := rows.Scan(&guide); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return parseGuide(guide.String)
}

func parseGuide(s string) ([]Key, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var entries []struct {
		Path string `json:"o:path"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(s), &entries); err != nil {
		return nil, fmt.Errorf("dataguide: %w", err)
	}
	var out []Key
	seen := map[string]int{}
	for _, e := range entries {
		segs, ok := guidePath(e.Path)
		if !ok || len(segs) == 0 {
			continue
		}
		k := Key{Path: segs[:len(segs)-1], Name: segs[len(segs)-1], Types: []string{e.Type}}
		id := strings.Join(segs, "\x00")
		if i, dup := seen[id]; dup { // same key with another type
			out[i].Types = append(out[i].Types, e.Type)
			continue
		}
		seen[id] = len(out)
		out = append(out, k)
	}
	return out, nil
}

// guidePath splits a dataguide path ($.a."b c"[*].d) into keys; array
// steps are dropped (arrays are looked through). Paths ending in an array
// step describe elements, not keys.
func guidePath(p string) ([]string, bool) {
	if !strings.HasPrefix(p, "$") || strings.HasSuffix(p, "]") {
		return nil, false
	}
	path, prefix, _, ok := parseJSONPath(p)
	if !ok {
		return nil, false
	}
	if prefix != "" {
		path = append(path, prefix)
	}
	return path, true
}

// Children returns the keys directly under path.
func Children(all []Key, path []string) []Key {
	var out []Key
	for _, k := range all {
		if equalPath(k.Path, path) {
			out = append(out, k)
		}
	}
	sortKeys(out)
	return out
}

func equalPath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortKeys(ks []Key) {
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].ArrayOnly() != ks[j].ArrayOnly() {
			return !ks[i].ArrayOnly()
		}
		if ks[i].Count != ks[j].Count {
			return ks[i].Count > ks[j].Count
		}
		return ks[i].Name < ks[j].Name
	})
}

// --- presentation ---------------------------------------------------------

var simpleKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// InsertText is what replaces the typed prefix for a key.
func InsertText(style Style, key string) string {
	switch style {
	case StyleJSONPath:
		if simpleKeyRe.MatchString(key) {
			return "." + key
		}
		return `."` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(key) + `"`
	case StyleArray:
		if simpleKeyRe.MatchString(key) {
			return key
		}
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(key) + `"`
	}
	return key
}

// SQLText escapes text for inside a single-quoted SQL literal.
func SQLText(s string) string { return strings.ReplaceAll(s, "'", "''") }

// Detail describes a key for the completion menu, e.g. "string · 6/6 rows".
func Detail(k Key) string {
	d := strings.Join(k.Types, "|")
	if k.Sampled > 0 {
		d += fmt.Sprintf(" · %d/%d sampled", k.Count, k.Sampled)
	}
	return d
}

// --- cache ----------------------------------------------------------------

// Cache holds sampled keys. Lookups wait a short time for a fetch and leave
// slow fetches running in the background, so completion never blocks long.
type Cache struct {
	TTL time.Duration

	mu sync.Mutex
	m  map[string]*entry
}

type entry struct {
	done chan struct{}
	keys []Key
	err  error
	at   time.Time
}

// Get returns cached keys for id, fetching them if needed. ready is false
// when the fetch is still running after wait.
func (c *Cache) Get(id string, wait time.Duration, fetch func(context.Context) ([]Key, error)) (keys []Key, ready bool, err error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*entry{}
	}
	e := c.m[id]
	if e != nil {
		select {
		case <-e.done:
			if c.TTL > 0 && time.Since(e.at) > c.TTL {
				e = nil // expired: refetch
			}
		default: // in flight
		}
	}
	if e == nil {
		e = &entry{done: make(chan struct{})}
		c.m[id] = e
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			e.keys, e.err = fetch(ctx)
			e.at = time.Now()
			close(e.done)
		}()
	}
	c.mu.Unlock()

	select {
	case <-e.done:
		if e.err != nil { // don't cache failures
			c.mu.Lock()
			if c.m[id] == e {
				delete(c.m, id)
			}
			c.mu.Unlock()
		}
		return e.keys, true, e.err
	case <-time.After(wait):
		return nil, false, nil
	}
}

// Clear drops everything (connection switch, schema refresh).
func (c *Cache) Clear() {
	c.mu.Lock()
	c.m = nil
	c.mu.Unlock()
}

// ArrayHint explains, for PostgreSQL operator styles, why an array-element
// key cannot be read directly and what to write instead. ok is false when
// the key is usable as is (including SQL/JSON paths, whose lax mode steps
// into arrays by itself).
func ArrayHint(jc *Context, k Key) (doc string, ok bool) {
	if !k.ArrayOnly() || jc.Style == StyleJSONPath {
		return "", false
	}
	col := jc.Column
	if !simpleKeyRe.MatchString(col) {
		col = `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
	}
	if jc.Qualifier != "" {
		col = jc.Qualifier + "." + col
	}
	parent := col
	path := "$"
	for _, seg := range jc.Path {
		if isInt(seg) {
			parent += "->" + seg
			path += "[" + seg + "]"
		} else {
			parent += "->'" + SQLText(seg) + "'"
			path += InsertText(StyleJSONPath, seg)
		}
	}
	key := SQLText(k.Name)
	name := "the value"
	if n := len(jc.Path); n > 0 {
		name = "`" + jc.Path[n-1] + "`"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s is an **array**: `%s` is a key of its elements, so `%s->>'%s'` is always NULL.\n\n",
		name, k.Name, parent, key)
	fmt.Fprintf(&b, "- one element: `%s->0->>'%s'`\n", parent, key)
	fmt.Fprintf(&b, "- all values: `jsonb_path_query_array(%s, '%s[*]%s')`\n", col, SQLText(path), SQLText(InsertText(StyleJSONPath, k.Name)))
	fmt.Fprintf(&b, "- a row per element: `left join lateral jsonb_array_elements(%s) as e(item) on true` → `e.item->>'%s'`\n", parent, key)
	return b.String(), true
}
