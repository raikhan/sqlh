// Package history records the statements sqlh runs, per workspace, in
// {workspace}/.sqlh/history.jsonl (gitignored with the results), and groups
// the runs of one query into versions: a run whose SQL is similar enough to
// the latest version of an earlier query of the same file continues it.
package history

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Entry is one executed statement.
type Entry struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"` // preview, export, dump, text, cli
	File      string    `json:"file,omitempty"`
	Line      int       `json:"line,omitempty"` // 1-based first line when run
	SQL       string    `json:"sql"`
	Conn      string    `json:"conn"`
	Database  string    `json:"db,omitempty"`
	Schema    string    `json:"schema,omitempty"`
	Env       string    `json:"env,omitempty"`
	ElapsedMS int64     `json:"elapsedMs"`
	Rows      int64     `json:"rows"`
	Affected  int64     `json:"affected"` // -1 when not DML
	Truncated bool      `json:"truncated,omitempty"`
	Status    string    `json:"status"` // ok, error, cancelled
	Error     string    `json:"error,omitempty"`
	Result    string    `json:"result,omitempty"` // CSV written
	Lineage   string    `json:"lineage"`          // the query this run is a version of
}

// Path is the history file of a workspace.
func Path(workspace string) string { return filepath.Join(workspace, ".sqlh", "history.jsonl") }

// Similar: the share of common tokens above which a run continues a query.
const Similar = 0.6

// scanBack: how many recent entries are considered for the lineage.
const scanBack = 2000

var mu sync.Mutex // one writer per process; appends are atomic lines across processes

// Append records e (ID, Time and Lineage filled in when empty).
func Append(workspace string, e Entry) (Entry, error) {
	mu.Lock()
	defer mu.Unlock()
	if workspace == "" {
		return e, nil
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.File = canonical(e.File)
	if e.Lineage == "" {
		e.Lineage = lineageFor(workspace, e)
	}
	dir := filepath.Dir(Path(workspace))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return e, err
	}
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); os.IsNotExist(err) {
		_ = os.WriteFile(gi, []byte("# created by sqlh: query results and history are not versioned\n*\n"), 0o644)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	f, err := os.OpenFile(Path(workspace), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return e, err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return e, err
}

// canonical resolves symlinks (macOS /tmp → /private/tmp), so a file is
// recorded and found under one name.
func canonical(path string) string {
	if path == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102150405") + "-" + hex.EncodeToString(b)
}

// lineageFor: the lineage of the most similar recent query of the same file
// (its latest version), else a new one.
func lineageFor(workspace string, e Entry) string {
	entries, _ := Load(workspace)
	if len(entries) > scanBack {
		entries = entries[len(entries)-scanBack:]
	}
	latest := map[string]Entry{} // lineage → its latest version
	for _, x := range entries {
		if x.File == e.File {
			latest[x.Lineage] = x
		}
	}
	toks := Tokens(e.SQL)
	best, bestScore := "", 0.0
	for lin, x := range latest {
		if x.SQL == e.SQL {
			return lin
		}
		if sc := Similarity(toks, Tokens(x.SQL)); sc > bestScore {
			best, bestScore = lin, sc
		}
	}
	if bestScore >= Similar {
		return best
	}
	return e.ID
}

// cache: the last file read, reused while its size and time are unchanged
// (the panel asks on every cursor move).
var cache struct {
	sync.Mutex
	path    string
	size    int64
	mod     time.Time
	entries []Entry
}

// Load reads a workspace's history, oldest first (unreadable lines skipped).
// The result is shared: don't modify it.
func Load(workspace string) ([]Entry, error) {
	path := Path(workspace)
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cache.Lock()
	defer cache.Unlock()
	if cache.path == path && cache.size == fi.Size() && cache.mod.Equal(fi.ModTime()) {
		return cache.entries, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			out = append(out, e)
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	cache.path, cache.size, cache.mod, cache.entries = path, fi.Size(), fi.ModTime(), out
	return out, nil
}

// Query filters entries, newest first.
type Query struct {
	File    string  // only this file
	Lineage string  // only this query's versions
	Like    string  // only versions of queries similar to this SQL (same file if File is set)
	Limit   int     // at most (0: all)
	MinSim  float64 // for Like (default Similar)
}

// Find returns the entries matching q, newest first.
func Find(workspace string, q Query) ([]Entry, error) {
	all, err := Load(workspace)
	if err != nil {
		return nil, err
	}
	q.File = canonical(q.File)
	keep := map[string]bool{}
	if q.Like != "" {
		minSim := q.MinSim
		if minSim == 0 {
			minSim = Similar
		}
		toks := Tokens(q.Like)
		for _, e := range all {
			if q.File != "" && e.File != q.File {
				continue
			}
			if keep[e.Lineage] {
				continue
			}
			if e.SQL == q.Like || Similarity(toks, Tokens(e.SQL)) >= minSim {
				keep[e.Lineage] = true
			}
		}
	}
	var out []Entry
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		switch {
		case q.File != "" && e.File != q.File:
			continue
		case q.Lineage != "" && e.Lineage != q.Lineage:
			continue
		case q.Like != "" && !keep[e.Lineage]:
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// Versions groups entries (newest first) by lineage, keeping distinct SQL
// texts: the versions of each query, newest first; queries ordered by their
// latest run.
func Versions(entries []Entry) [][]Entry {
	var order []string
	groups := map[string][]Entry{}
	seen := map[string]map[string]bool{}
	for _, e := range entries {
		if _, ok := groups[e.Lineage]; !ok {
			order = append(order, e.Lineage)
			seen[e.Lineage] = map[string]bool{}
		}
		if seen[e.Lineage][e.SQL] {
			continue
		}
		seen[e.Lineage][e.SQL] = true
		groups[e.Lineage] = append(groups[e.Lineage], e)
	}
	out := make([][]Entry, 0, len(order))
	for _, l := range order {
		out = append(out, groups[l])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][0].Time.After(out[j][0].Time) })
	return out
}

// Tokens normalises SQL for comparison: comments and whitespace dropped,
// keywords and identifiers folded to lower case, literals kept as written.
func Tokens(sql string) []string {
	var out []string
	r := []rune(sql)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '-' && i+1 < len(r) && r[i+1] == '-':
			for i < len(r) && r[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			i += 2
			for i+1 < len(r) && !(r[i] == '*' && r[i+1] == '/') {
				i++
			}
			i += 2
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(r) {
				if r[j] == c {
					if j+1 < len(r) && r[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(r))
			tok := string(r[i:end])
			if c == '"' {
				tok = strings.ToLower(tok)
			}
			out = append(out, tok)
			i = end
		case unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '$' || c == '#':
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || r[j] == '$' || r[j] == '#' || r[j] == '.') {
				j++
			}
			out = append(out, strings.ToLower(string(r[i:j])))
			i = j
		default:
			out = append(out, string(c))
			i++
		}
	}
	return out
}

// maxTokens bounds the comparison cost.
const maxTokens = 600

// Similarity of two token lists: 2·LCS / (len a + len b), 0…1.
func Similarity(a, b []string) float64 {
	if len(a) > maxTokens {
		a = a[:maxTokens]
	}
	if len(b) > maxTokens {
		b = b[:maxTokens]
	}
	if len(a)+len(b) == 0 {
		return 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return 2 * float64(prev[len(b)]) / float64(len(a)+len(b))
}
