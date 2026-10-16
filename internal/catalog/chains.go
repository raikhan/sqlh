package catalog

import (
	"sort"
	"strings"

	"github.com/raikhan/sqlh/internal/control"
)

// chainBudget bounds the chain enumeration (steps of the depth-first walk):
// counts past it are reported as "more".
const chainBudget = 200000

func chainKey(table string) string {
	return strings.ToUpper(strings.ReplaceAll(table, `"`, ""))
}

// chainGraph: the joinable relationships of the tables reachable from the
// statement's, loaded on demand.
type chainGraph struct {
	src   Source
	opt   Options
	maybe bool
	edges map[string][]chainEdge
	names map[string]string // key → the table as typed
	kinds map[string]string
}

type chainEdge struct {
	to  string // key
	hop control.JoinHop
}

func (g *chainGraph) out(k string) []chainEdge {
	if e, ok := g.edges[k]; ok {
		return e
	}
	var out []chainEdge
	if rel, err := Relations(g.src, g.names[k], g.opt, g.maybe); err == nil {
		for _, r := range rel.Relations {
			if len(r.Cols) == 0 {
				continue // not joinable (a view using it)
			}
			rk := chainKey(r.Insert)
			if _, ok := g.names[rk]; !ok {
				g.names[rk], g.kinds[rk] = r.Insert, r.Kind
			}
			out = append(out, chainEdge{to: rk, hop: control.JoinHop{Table: r.Insert, Cols: r.Cols}})
		}
	}
	g.edges[k] = out
	return out
}

// Chains searches the relationship graph from the statement's tables (from,
// as typed) for chains of joins of at most maxDepth joins; only joinable
// relationships (with columns). A chain never visits a table twice, nor
// goes through another statement table (from there it starts itself).
//
// Without target: every reachable table, nearest first, with its shortest
// length and how many chains reach it (counted up to limit; More past it).
// With target: the chains to it (at most limit), shortest first, then in the
// from tables' and the relationships' order.
func Chains(src Source, from []string, target string, maxDepth, limit int, opt Options, maybe bool) (*control.ChainsResult, error) {
	if maxDepth <= 0 {
		maxDepth = 4
	}
	if limit <= 0 {
		limit = 10
	}
	g := &chainGraph{src: src, opt: opt, maybe: maybe, edges: map[string][]chainEdge{}, names: map[string]string{}, kinds: map[string]string{}}
	stmt := map[string]bool{}
	var anchors []string
	for _, f := range from {
		k := chainKey(f)
		if !stmt[k] {
			stmt[k] = true
			anchors = append(anchors, k)
			g.names[k] = f
		}
	}
	res := &control.ChainsResult{Targets: []control.ChainTarget{}, Chains: []control.Chain{}}
	tk := chainKey(target)
	if target != "" && stmt[tk] {
		return res, nil
	}

	// breadth-first from the statement: each table's nearest distance (and
	// the order tables are found in)
	dist := map[string]int{}
	var order []string
	frontier := anchors
	for _, a := range anchors {
		dist[a] = 0
	}
	for d := 1; d <= maxDepth && len(frontier) > 0; d++ {
		var next []string
		for _, k := range frontier {
			for _, e := range g.out(k) {
				if _, seen := dist[e.to]; !seen {
					dist[e.to] = d
					order = append(order, e.to)
					next = append(next, e.to)
				}
			}
		}
		frontier = next
	}

	// with a target: its distance to every table (over the edges seen,
	// both ways: a lower bound), to prune the walk
	toTarget := map[string]int{}
	if target != "" {
		if _, ok := dist[tk]; !ok {
			return res, nil
		}
		back := map[string][]string{}
		for k, es := range g.edges {
			for _, e := range es {
				back[e.to] = append(back[e.to], k)
				back[k] = append(back[k], e.to)
			}
		}
		toTarget[tk] = 0
		q := []string{tk}
		for len(q) > 0 {
			k := q[0]
			q = q[1:]
			for _, n := range back[k] {
				if _, ok := toTarget[n]; !ok {
					toTarget[n] = toTarget[k] + 1
					q = append(q, n)
				}
			}
		}
	}

	// depth-first: every simple chain
	counts := map[string]int{}
	more := map[string]bool{}
	var chains []control.Chain
	steps := 0
	onPath := map[string]bool{}
	var walk func(anchor, k string, hops []control.JoinHop)
	walk = func(anchor, k string, hops []control.JoinHop) {
		if len(hops) == maxDepth {
			return
		}
		for _, e := range g.out(k) {
			if onPath[e.to] || stmt[e.to] {
				continue
			}
			steps++
			if steps > chainBudget {
				for t := range dist {
					more[t] = true
				}
				return
			}
			if target != "" {
				d, ok := toTarget[e.to]
				if !ok || len(hops)+1+d > maxDepth {
					continue
				}
			}
			next := append(append([]control.JoinHop(nil), hops...), e.hop)
			if target == "" {
				if counts[e.to] < limit {
					counts[e.to]++
				} else {
					more[e.to] = true
				}
			} else if e.to == tk {
				chains = append(chains, control.Chain{Anchor: g.names[anchor], Hops: next})
				continue // a chain ends at its target
			}
			onPath[e.to] = true
			walk(anchor, e.to, next)
			onPath[e.to] = false
		}
	}
	for _, a := range anchors {
		onPath[a] = true
		walk(a, a, nil)
		onPath[a] = false
	}

	if target == "" {
		sort.SliceStable(order, func(i, j int) bool { return dist[order[i]] < dist[order[j]] })
		for _, k := range order {
			res.Targets = append(res.Targets, control.ChainTarget{Table: g.names[k], Kind: g.kinds[k],
				Joins: dist[k], Chains: counts[k], More: more[k]})
		}
		return res, nil
	}
	sort.SliceStable(chains, func(i, j int) bool { return len(chains[i].Hops) < len(chains[j].Hops) })
	if len(chains) > limit {
		chains, res.More = chains[:limit], true
	}
	res.Chains = chains
	return res, nil
}
