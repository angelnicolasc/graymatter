// Graph analytics for `doctor --graph`: hubs by degree, articulation points
// and bridges (Tarjan, iterative), orphans, and a connectivity ratio.
//
// Formulas are declared in the output so nobody has to trust them:
//   - degree        = number of undirected edges touching the node
//   - articulation  = node whose removal increases connected components
//   - connectivity  = unique undirected edges / max possible for N nodes
package kg

import (
	"sort"
)

type NodeStat struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Degree int    `json:"degree"`
	Orphan bool   `json:"orphan"`
}

type GraphReport struct {
	NodeCount          int        `json:"nodes"`
	EdgeCount          int        `json:"edges"`
	Orphans            int        `json:"orphans"`
	ConnectivityRatio  float64    `json:"connectivity_ratio"`
	Hubs               []NodeStat `json:"hubs"` // top 5 by degree
	OrphanIDs          []string   `json:"orphan_ids,omitempty"`
	ArticulationPoints []string   `json:"articulation_points,omitempty"`
	Nodes              []NodeStat `json:"-"`
}

// Analyze builds the report from raw graph data. Deterministic: same input,
// same ordering everywhere.
func Analyze(nodes []Node, edges []Edge) GraphReport {
	rep := GraphReport{NodeCount: len(nodes), EdgeCount: len(edges)}

	deg := make(map[string]int, len(nodes))
	adj := map[string][]string{}
	pairSeen := map[string]bool{}
	for _, e := range edges {
		deg[e.From]++
		deg[e.To]++
		a, b := e.From, e.To
		if a > b {
			a, b = b, a
		}
		key := a + "\x00" + b
		if !pairSeen[key] {
			pairSeen[key] = true
			adj[a] = append(adj[a], b)
			adj[b] = append(adj[b], a)
		}
	}

	stats := make([]NodeStat, 0, len(nodes))
	for _, n := range nodes {
		d := deg[n.ID]
		orphan := d == 0
		if orphan {
			rep.Orphans++
			rep.OrphanIDs = append(rep.OrphanIDs, n.ID)
		}
		s := NodeStat{ID: n.ID, Label: n.Label, Degree: d, Orphan: orphan}
		stats = append(stats, s)
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Degree > stats[j].Degree })
	rep.Nodes = stats

	top := 5
	if len(stats) < top {
		top = len(stats)
	}
	rep.Hubs = stats[:top]

	if rep.NodeCount > 1 {
		maxPossible := float64(rep.NodeCount) * float64(rep.NodeCount-1) / 2
		rep.ConnectivityRatio = float64(uniqueUndirected(edges)) / maxPossible
	}

	rep.ArticulationPoints = articulation(adj)

	return rep
}

func uniqueUndirected(edges []Edge) int {
	seen := map[string]bool{}
	n := 0
	for _, e := range edges {
		a, b := e.From, e.To
		if a > b {
			a, b = b, a
		}
		k := a + "\x00" + b
		if !seen[k] {
			seen[k] = true
			n++
		}
	}
	return n
}

// articulation returns nodes whose removal disconnects part of the undirected
// projection (iterative Tarjan). Isolated nodes are never articulation points.
func articulation(adj map[string][]string) []string {
	var ids []string
	for id := range adj {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var result []string
	counter := 0

	var dfs func(start string)
	dfs = func(start string) {
		type frame struct {
			v  string
			ci int // child index into adj
		}
		stack := []frame{{v: start}}
		index[start] = counter
		low[start] = counter
		counter++
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			children := adj[f.v]
			if f.ci < len(children) {
				w := children[f.ci]
				f.ci++
				if _, seen := index[w]; !seen {
					stack = append(stack, frame{v: w})
					index[w] = counter
					low[w] = counter
					counter++
				} else {
					if low[f.v] > index[w] {
						low[f.v] = index[w]
					}
				}
				continue
			}
			// pop
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				break
			}
			parent := &stack[len(stack)-1]
			if low[f.v] < low[parent.v] {
				low[parent.v] = low[f.v]
			}
			if len(stack) > 1 && low[f.v] >= index[parent.v] {
				onStack[parent.v] = true
			}
		}
	}

	for _, id := range ids {
		if _, seen := index[id]; !seen && len(adj[id]) > 0 {
			dfs(id)
		}
	}

	for id := range onStack {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
