package catalog

import (
	"strings"

	"github.com/raikhan/sqlh/internal/control"
)

// chainNode: a table reached by the breadth-first search, with every way
// of reaching it at its distance.
type chainNode struct {
	insert, kind string
	dist         int
	paths        int         // shortest chains reaching it
	from         []chainEdge // the tables one join closer (in discovery order)
}

type chainEdge struct {
	prev string // key of the previous table ("" = none: a statement table)
	hop  control.JoinHop
}

func chainKey(table string) string {
	return strings.ToUpper(strings.ReplaceAll(table, `"`, ""))
}

// Chains searches the relationship graph from the statement's tables (from,
// as typed) for the tables a chain of joins reaches, at most maxDepth joins
// away; only joinable relationships (with columns). A chain never goes
// through another statement table (from there it would be shorter).
//
// Without target: every reachable table, nearest first, with its shortest
// chain. With target: the shortest chains to it (at most limit), in a stable
// order — the from tables' order, then the relationships' order.
func Chains(src Source, from []string, target string, maxDepth, limit int, opt Options, maybe bool) (*control.ChainsResult, error) {
	if maxDepth <= 0 {
		maxDepth = 4
	}
	nodes := map[string]*chainNode{}
	var order []string // discovery order
	var frontier []string
	for _, f := range from {
		k := chainKey(f)
		if _, ok := nodes[k]; ok {
			continue
		}
		nodes[k] = &chainNode{insert: f, paths: 1}
		frontier = append(frontier, k)
	}
	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, k := range frontier {
			n := nodes[k]
			rel, err := Relations(src, n.insert, opt, maybe)
			if err != nil {
				continue
			}
			for _, r := range rel.Relations {
				if len(r.Cols) == 0 {
					continue // not joinable (a view using it)
				}
				rk := chainKey(r.Insert)
				m, seen := nodes[rk]
				if !seen {
					m = &chainNode{insert: r.Insert, kind: r.Kind, dist: depth}
					nodes[rk] = m
					order = append(order, rk)
					next = append(next, rk)
				}
				if m.dist != depth {
					continue // reached before, by a shorter chain
				}
				m.from = append(m.from, chainEdge{prev: k, hop: control.JoinHop{Table: r.Insert, Cols: r.Cols}})
				m.paths += n.paths
			}
		}
		frontier = next
	}
	res := &control.ChainsResult{Targets: []control.ChainTarget{}, Chains: []control.Chain{}}
	if target == "" {
		for _, k := range order {
			n := nodes[k]
			c := chainsTo(nodes, k, 1)
			t := control.ChainTarget{Table: n.insert, Kind: n.kind, Joins: n.dist, Chains: n.paths}
			if len(c) > 0 {
				t.Via = append([]string{c[0].Anchor}, hopTables(c[0].Hops)...)
			}
			res.Targets = append(res.Targets, t)
		}
		return res, nil
	}
	if n, ok := nodes[chainKey(target)]; ok && n.dist > 0 {
		res.Chains = chainsTo(nodes, chainKey(target), limit)
	}
	return res, nil
}

func hopTables(hops []control.JoinHop) []string {
	out := make([]string, len(hops))
	for i, h := range hops {
		out[i] = h.Table
	}
	return out
}

// chainsTo: up to limit shortest chains to the table k (depth-first over
// the recorded predecessors, in their order).
func chainsTo(nodes map[string]*chainNode, k string, limit int) []control.Chain {
	if limit <= 0 {
		limit = 5
	}
	var out []control.Chain
	var walk func(k string, hops []control.JoinHop)
	walk = func(k string, hops []control.JoinHop) {
		if len(out) >= limit {
			return
		}
		n := nodes[k]
		if n.dist == 0 {
			out = append(out, control.Chain{Anchor: n.insert, Hops: append([]control.JoinHop(nil), hops...)})
			return
		}
		for _, e := range n.from {
			walk(e.prev, append([]control.JoinHop{e.hop}, hops...))
		}
	}
	walk(k, nil)
	return out
}
