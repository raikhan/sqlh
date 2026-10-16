package tui

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fuzzyScore matches pattern as a case-insensitive subsequence of s. It
// returns the score (higher is better), the matched rune positions, and
// whether it matched. Matches at word starts, consecutive runs and a prefix
// score higher; shorter candidates win ties.
func fuzzyScore(pattern, s string) (int, []int, bool) {
	if pattern == "" {
		return 0, nil, true
	}
	p := []rune(strings.ToLower(pattern))
	r := []rune(s)
	lower := []rune(strings.ToLower(s))
	if len(lower) != len(r) { // case folding changed the length: compare as is
		lower = r
	}
	m, n := len(p), len(lower)
	if m > n {
		return 0, nil, false
	}
	const none = -1 << 30
	// score[i][j]: best score with p[i] matched at j; from[i][j]: where p[i-1] was
	score := make([][]int, m)
	from := make([][]int, m)
	for i := range score {
		score[i] = make([]int, n)
		from[i] = make([]int, n)
		for j := range score[i] {
			score[i][j] = none
		}
	}
	bonus := func(j int) int {
		switch {
		case j == 0:
			return 32
		case wordStart(r, j):
			return 24
		}
		return 0
	}
	for j := 0; j < n; j++ {
		if lower[j] == p[0] {
			score[0][j] = 16 + bonus(j) - min(j, 8) // later starts cost a little
		}
	}
	for i := 1; i < m; i++ {
		for j := i; j < n; j++ {
			if lower[j] != p[i] {
				continue
			}
			for k := i - 1; k < j; k++ {
				if score[i-1][k] == none {
					continue
				}
				v := score[i-1][k] + 16 + bonus(j)
				if k == j-1 {
					v += 16
				} else {
					v -= min(j-k-1, 10)
				}
				if v > score[i][j] {
					score[i][j], from[i][j] = v, k
				}
			}
		}
	}
	best, at := none, -1
	for j := 0; j < n; j++ {
		if score[m-1][j] > best {
			best, at = score[m-1][j], j
		}
	}
	if at < 0 {
		return 0, nil, false
	}
	pos := make([]int, m)
	for i := m - 1; i >= 0; i-- {
		pos[i] = at
		at = from[i][at]
	}
	return best*4 - utf8.RuneCountInString(s), pos, true
}

func wordStart(r []rune, i int) bool {
	prev := r[i-1]
	if prev == '_' || prev == '.' || prev == ' ' || prev == '$' || prev == '#' || prev == '-' {
		return true
	}
	return unicode.IsLower(prev) && unicode.IsUpper(r[i])
}

// match is a ranked search result.
type match struct {
	index int
	score int
	pos   []int
}

// fuzzyRank ranks candidates for pattern (all of them in order when it is
// empty).
func fuzzyRank(pattern string, candidates []string) []match {
	var out []match
	for i, c := range candidates {
		if score, pos, ok := fuzzyScore(pattern, c); ok {
			out = append(out, match{index: i, score: score, pos: pos})
		}
	}
	if pattern != "" {
		sort.SliceStable(out, func(a, b int) bool { return out[a].score > out[b].score })
	}
	return out
}
