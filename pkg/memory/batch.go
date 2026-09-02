package memory

import (
	"context"
	"runtime"
	"sort"
	"sync"
)

// BatchRecall answers many queries in one call, fanning them out across
// goroutines.
//
// The reason this exists is a measured one, and it is about turns rather than
// milliseconds. An agent holding several open questions had to issue one recall
// per question, and each recall is a round trip through the model: the wall
// clock is spent in the conversation, not in the store. Measured against a
// flat memory file the agent could grep in a single call, the recall-per-query
// shape lost on turns while winning on tokens — the engine was never the
// bottleneck, the interface was.
//
// Ranking is untouched. Each query is scored exactly as Recall would score it
// alone, so a batch of one is indistinguishable from a plain Recall. What the
// batch adds is the fan-out and a merged view (see MergedFacts) for callers
// that want one deduplicated block instead of k separate lists.
type BatchResult struct {
	Query string   `json:"query"`
	Facts []string `json:"facts"`
	// Err carries a per-query failure. One bad query does not fail the batch:
	// an agent asking six questions should get the five answers that worked,
	// not a single error for all of them.
	Err error `json:"-"`
}

// BatchRecall runs each query concurrently and returns one result per query,
// in the order the queries were given. The order is deterministic and matches
// the input regardless of which goroutine finishes first, because a caller
// pairing answers to questions positionally must not depend on scheduling.
//
// Concurrency is bounded by GOMAXPROCS: the store's own read path is already
// parallel-safe, but an unbounded fan-out on a 200-query batch would create
// 200 simultaneous bbolt read transactions and vector searches for no gain.
func (s *Store) BatchRecall(ctx context.Context, agentID string, queries []string, topK int) ([]BatchResult, error) {
	if len(queries) == 0 {
		return nil, nil
	}
	out := make([]BatchResult, len(queries))
	limit := runtime.GOMAXPROCS(0)
	if limit > len(queries) {
		limit = len(queries)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = BatchResult{Query: q}
			facts, err := s.Recall(ctx, agentID, q, topK)
			if err != nil {
				out[i].Err = err
				return
			}
			out[i].Facts = facts
		}(i, q)
	}
	wg.Wait()
	return out, ctx.Err()
}

// MergedFacts flattens a batch into one deduplicated block, best-first.
//
// A fact that answers three of the queries is one fact, not three, and paying
// for it three times is exactly the waste the fixed token budget exists to
// avoid. Ordering is by best rank achieved across the queries that returned
// it, with the number of queries it satisfied breaking ties: a fact several
// questions agree on is more likely to be what the caller needed than one that
// topped a single list.
func MergedFacts(results []BatchResult) []string {
	type agg struct {
		best  int // best (lowest) rank across queries, 1-based
		hits  int // how many queries returned it
		first int // first appearance, for a stable final order
	}
	seen := make(map[string]*agg)
	order := 0
	for _, r := range results {
		for i, f := range r.Facts {
			rank := i + 1
			a, ok := seen[f]
			if !ok {
				seen[f] = &agg{best: rank, hits: 1, first: order}
				order++
				continue
			}
			a.hits++
			if rank < a.best {
				a.best = rank
			}
		}
	}
	merged := make([]string, 0, len(seen))
	for f := range seen {
		merged = append(merged, f)
	}
	sort.Slice(merged, func(i, j int) bool {
		a, b := seen[merged[i]], seen[merged[j]]
		if a.best != b.best {
			return a.best < b.best
		}
		if a.hits != b.hits {
			return a.hits > b.hits
		}
		return a.first < b.first
	})
	return merged
}
