package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// Recall also fires the OnRecall observability hook if configured.

// recallPipeline is the intermediate ranking state one recall pass produces.
// Both Recall (bare texts) and RecallExplain (receipts) consume it, so the two
// can never disagree about what ranked where: there is exactly one pipeline,
// and explain is a read-out of it, not a second implementation.
type recallPipeline struct {
	facts      []Fact // live facts, tombstones filtered out
	factByID   map[string]*Fact
	ranked     []scored // full fused ranking, best first, post MinRelevance cut
	vectorRank map[string]int
	kwRank     map[string]int
	recRank    map[string]int
	k          float64 // RRF constant in force
	nowT       time.Time
	// supersededTexts holds the tombstoned texts the filter dropped, so the
	// KG enrichment step can keep a superseded fact's text out of the result.
	supersededTexts map[string]bool
}

// Recall performs hybrid retrieval for agentID given a query string.
// It fuses three signals via Reciprocal Rank Fusion (RRF):
//  1. Vector similarity (cosine, pluggable VectorStore) — when embeddings available
//  2. Keyword relevance (TF-IDF approximation over bbolt facts)
//  3. Recency score (exponential decay from CreatedAt)
//
// Returns the top-k fact texts, ready to inject into a system prompt.
func (s *Store) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	start := time.Now()
	p, err := s.runRecallPipeline(ctx, agentID, query, topK)
	if err != nil || p == nil {
		return nil, err
	}

	// Collect top-k, deduplicated by text (the documented contract), updating
	// access metadata along the way. The dedup pass walks the full ranking
	// rather than slicing first: with N identical texts inside the top-k, the
	// slice would spend budget on duplicates and return fewer distinct facts
	// than the caller asked for. Duplicates arise whenever a caller re-stores
	// the same sentence across sessions — the store is append-only by design.
	result := make([]string, 0, topK)
	seen := make(map[string]bool, topK)
	touched := make([]Fact, 0, topK)
	for _, sc := range p.ranked {
		if len(result) >= topK {
			break
		}
		f, ok := p.factByID[sc.id]
		if !ok {
			continue
		}
		if seen[f.Text] {
			continue
		}
		result = append(result, f.Text)
		seen[f.Text] = true
		// Access metadata rides on one batched transaction after the loop.
		// The previous shape spawned one goroutine holding one bbolt write
		// transaction PER RETURNED FACT - up to eight write txns and eight
		// goroutines per recall, contending with writers under load for
		// bookkeeping. One txn for the batch carries the same information.
		f.AccessCount++
		f.AccessedAt = p.nowT.UTC()
		touched = append(touched, *f)
	}
	s.touchFacts(touched)

	// Enrich with knowledge graph neighbors (optional; graph may be nil).
	s.mu.RLock()
	graph := s.graph
	extractor := s.extractor
	s.mu.RUnlock()
	if graph != nil && extractor != nil && len(result) > 0 {
		// Extract entity IDs from the top-ranked fact and surface neighbors.
		//
		// Budget (ADR-003 condition 2): at most kgMaxNeighbors entries are
		// appended in total. Enrichment is a hint, not a second result set —
		// an uncapped append would grow the prompt without bound on hub
		// entities and defeat the token discipline the rest of the system
		// enforces.
		ids, _ := extractor.ExtractIDs(result[0])
		appended := 0
		for _, id := range ids {
			if appended >= kgMaxNeighbors {
				break
			}
			neighborTexts, gErr := graph.NeighborTexts(id, 1)
			if gErr != nil {
				break
			}
			for _, nt := range neighborTexts {
				if appended >= kgMaxNeighbors {
					break
				}
				// The graph stores node labels, not fact IDs, so a superseded
				// fact's text can still be reachable as a neighbour. Skip it:
				// the tombstone has to hold on every path into the result.
				if !seen[nt] && !p.supersededTexts[nt] {
					seen[nt] = true
					result = append(result, nt)
					appended++
				}
			}
		}
	}

	if s.cfg.OnRecall != nil {
		s.cfg.OnRecall(agentID, query, len(result), time.Since(start))
	}
	return result, nil
}

// RecallExplain performs the same hybrid retrieval as Recall and returns one
// RecallReceipt per returned fact instead of the bare texts (see explain.go).
func (s *Store) RecallExplain(ctx context.Context, agentID, query string, topK int) ([]RecallReceipt, error) {
	start := time.Now()
	p, err := s.runRecallPipeline(ctx, agentID, query, topK)
	if err != nil || p == nil {
		return nil, err
	}

	// Identical walk to Recall's collection loop — same dedup contract, same
	// access-metadata side effects — accumulating receipts instead of texts,
	// read straight out of the pipeline's ranking state. No second pass.
	receipts := make([]RecallReceipt, 0, topK)
	seen := make(map[string]bool, topK)
	touched := make([]Fact, 0, topK)
	for _, sc := range p.ranked {
		if len(receipts) >= topK {
			break
		}
		f, ok := p.factByID[sc.id]
		if !ok {
			continue
		}
		if seen[f.Text] {
			continue
		}
		seen[f.Text] = true
		receipts = append(receipts, s.newReceipt(f, p, sc.score))
		f.AccessCount++
		f.AccessedAt = p.nowT.UTC()
		touched = append(touched, *f)
	}
	s.touchFacts(touched)

	if s.cfg.OnRecall != nil {
		s.cfg.OnRecall(agentID, query, len(receipts), time.Since(start))
	}
	return receipts, nil
}

// runRecallPipeline performs every scoring pass of one recall: list, tombstone
// filter, the three signal rankings, RRF fusion, the debugRanking seam and the
// optional MinRelevance cut. Both Recall and RecallExplain consume the result.
//
// Returns (nil, nil) when the agent has nothing to rank — either no stored
// facts at all or no live facts after the tombstone filter. An empty result is
// not an error; the OnRecall hook still fires on the live-but-all-tombstoned
// path so observability sees the recall happened and returned nothing.
func (s *Store) runRecallPipeline(ctx context.Context, agentID, query string, topK int) (*recallPipeline, error) {
	start := time.Now()
	stored, err := s.listLite(agentID)
	if err != nil || len(stored) == 0 {
		return nil, err
	}

	// Drop superseded facts before anything is scored. Filtering here rather
	// than at the end means a tombstoned fact cannot displace a live one from
	// the top-k, and the three signals below rank only what is still true.
	//
	// The vector index is deliberately not filtered: it can still return a
	// superseded ID, but the fusion loop iterates over facts, so such an ID
	// only ever contributes a rank nobody reads.
	facts := make([]Fact, 0, len(stored))
	var supersededTexts map[string]bool
	for _, f := range stored {
		if f.IsSuperseded() {
			if supersededTexts == nil {
				supersededTexts = make(map[string]bool)
			}
			supersededTexts[f.Text] = true
			continue
		}
		facts = append(facts, f)
	}
	if len(facts) == 0 {
		if s.cfg.OnRecall != nil {
			s.cfg.OnRecall(agentID, query, 0, time.Since(start))
		}
		return nil, nil
	}

	factByID := make(map[string]*Fact, len(facts))
	for i := range facts {
		factByID[facts[i].ID] = &facts[i]
	}

	// rankBefore is the total order every ranking below uses: score
	// descending, then oldest first, then fact ID.
	//
	// Each of the three signal rankings turns scores into ranks, and those
	// ranks are what the RRF fusion actually reads. Sorting them with a
	// comparator that only looks at the score leaves the order of equal
	// scores unspecified — sort.Slice is not stable — so tied facts received
	// arbitrary ranks, arbitrary ranks produced different fused scores, and
	// the same query against the same store returned different facts from one
	// call to the next. With six facts written in the same instant, all six
	// rotations of the result were observable.
	//
	// CreatedAt comes before ID because it is meaningful: when two facts score
	// the same, the older one has been in the store longer and is the more
	// established of the two. ID is the final fallback, so the order is total
	// even for facts created in the same instant.
	rankBefore := func(aID string, aScore float64, bID string, bScore float64) bool {
		if aScore != bScore {
			return aScore > bScore
		}
		if fa, fb := factByID[aID], factByID[bID]; fa != nil && fb != nil && !fa.CreatedAt.Equal(fb.CreatedAt) {
			return fa.CreatedAt.Before(fb.CreatedAt)
		}
		return aID < bID
	}

	// --- Signal 1: vector similarity ---
	vectorRank := make(map[string]int, topK*2) // factID → rank (1-based)
	vecResults, _ := s.vectorSearch(ctx, agentID, query, topK*2)
	// Impose the same total order rankBefore uses everywhere else — score
	// descending, then ID ascending — before turning results into ranks. The
	// backend's own ordering is unspecified for equal similarities (chromem
	// iterates an internal map, so tied vectors came back in a different
	// order on every call), and an arbitrary order here fed arbitrary ranks
	// into the fusion: the same query returned different facts from one call
	// to the next whenever two embeddings tied.
	sort.SliceStable(vecResults, func(i, j int) bool {
		if vecResults[i].Similarity != vecResults[j].Similarity {
			return vecResults[i].Similarity > vecResults[j].Similarity
		}
		return vecResults[i].ID < vecResults[j].ID
	})
	for i, r := range vecResults {
		vectorRank[r.ID] = i + 1
	}

	// --- Signal 2: keyword relevance ---
	kwScores := keywordScore(query, facts)
	type kwEntry struct {
		id    string
		score float64
	}
	kwSorted := make([]kwEntry, 0, len(kwScores))
	for id, sc := range kwScores {
		kwSorted = append(kwSorted, kwEntry{id, sc})
	}
	sort.Slice(kwSorted, func(i, j int) bool {
		return rankBefore(kwSorted[i].id, kwSorted[i].score, kwSorted[j].id, kwSorted[j].score)
	})
	kwRank := make(map[string]int, len(kwSorted))
	for i, e := range kwSorted {
		kwRank[e.id] = i + 1
	}

	// --- Signal 3: recency score ---
	//
	// The recency score is exp(-λ·age) with λ = ln2 / DecayHalfLife (30-day
	// default) — strictly monotonic in CreatedAt, so a fact's recency rank is
	// its position in the newest-first list, and listLite already returns
	// exactly that order (CreatedAt desc, with bbolt's ID-asc key order
	// surviving the stable sort for equal stamps). That is precisely
	// rankBefore's recency order, so this signal needs no sort of its own —
	// and equal-CreatedAt facts (same clock tick; Windows ticks at ~15.6 ms)
	// resolve by ID deterministically instead of by sort.Slice's unspecified
	// tie order. One less O(n log n) pass per recall, on top of the
	// determinism.
	recRank := make(map[string]int, len(facts))
	for i := range facts {
		recRank[facts[i].ID] = i + 1
	}
	nowT := s.now()

	// --- RRF fusion ---
	//
	// k=60 is the constant from Cormack, Clarke & Buettcher (SIGIR 2009). It
	// stays fixed: it damps the difference between adjacent ranks, and with a
	// single signal enabled it cannot change the ordering at all, so making it
	// configurable would add a knob with no reachable effect.
	const k = 60.0
	w := s.cfg.SignalWeights
	if w == nil {
		d := DefaultSignalWeights()
		w = &d
	}
	candidates := make(map[string]float64, len(facts))
	for _, f := range facts {
		rrf := 0.0
		if r, ok := vectorRank[f.ID]; ok {
			rrf += w.Vector / (k + float64(r))
		}
		if r, ok := kwRank[f.ID]; ok {
			rrf += w.Keyword / (k + float64(r))
		}
		if r, ok := recRank[f.ID]; ok {
			rrf += w.Recency / (k + float64(r))
		}
		candidates[f.ID] = rrf
	}

	allScored := make([]scored, 0, len(candidates))
	for id, sc := range candidates {
		allScored = append(allScored, scored{id, sc})
	}
	sort.Slice(allScored, func(i, j int) bool {
		return rankBefore(allScored[i].id, allScored[i].score, allScored[j].id, allScored[j].score)
	})

	// Test-only seam. Nil in production; the golden fixture sets it to freeze
	// the fused scores themselves rather than only their argmax.
	//
	// Recording just the returned order proved far too weak: mutation testing
	// showed a golden built on order alone passing with the default recency
	// weight changed from 0.5 to 0.6, and with the RRF constant k changed from
	// 60 to 50. Both alter every score; neither reordered the head of this
	// corpus. A gate on ranking knobs has to observe the ranking arithmetic.
	if s.debugRanking != nil {
		snapshot := make([]scored, len(allScored))
		copy(snapshot, allScored)
		s.debugRanking(query, snapshot)
	}

	// Optional relevance floor, relative to the best score in this result set.
	// At the default of 0 every score clears the bar and the slice is
	// untouched, which is the pre-v0.10.0 contract of returning exactly topK.
	if s.cfg.MinRelevance > 0 && len(allScored) > 0 {
		floor := s.cfg.MinRelevance * allScored[0].score
		cut := len(allScored)
		for i, sc := range allScored {
			if sc.score < floor {
				cut = i
				break
			}
		}
		allScored = allScored[:cut]
	}

	return &recallPipeline{
		facts:           facts,
		factByID:        factByID,
		ranked:          allScored,
		vectorRank:      vectorRank,
		kwRank:          kwRank,
		recRank:         recRank,
		k:               k,
		nowT:            nowT,
		supersededTexts: supersededTexts,
	}, nil
}

// keywordScore returns a TF-IDF-like score for each fact against the query.
// It uses simple term frequency over token overlap — no external deps.
//
// One tokenize pass per fact: each fact's term frequencies are computed once
// and reused for both the document-frequency counts and the scoring pass. The
// two-pass version re-tokenized every fact (10k facts ≈ 11 ms per pass, which
// showed up directly in the hook latency gate), with byte-identical scores.
func keywordScore(query string, facts []Fact) map[string]float64 {
	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return nil
	}

	type factTerms struct {
		terms []string
		tf    map[string]int
	}
	perFact := make([]factTerms, len(facts))
	df := make(map[string]int, len(queryTerms)*8)
	for i, f := range facts {
		terms := tokenize(f.Text)
		tf := make(map[string]int, len(terms))
		for _, t := range terms {
			tf[t]++
		}
		// DF: how many facts contain each term — exactly the set of unique
		// terms in the fact, which the tf map already holds.
		for t := range tf {
			df[t]++
		}
		perFact[i] = factTerms{terms: terms, tf: tf}
	}

	n := float64(len(facts))
	scores := make(map[string]float64, len(facts))
	for i, f := range facts {
		var score float64
		for _, qt := range queryTerms {
			if count, ok := perFact[i].tf[qt]; ok {
				idf := math.Log((n + 1) / (float64(df[qt]) + 1))
				score += float64(count) * idf
			}
		}
		if score > 0 {
			scores[f.ID] = score / float64(len(perFact[i].terms)+1)
		}
	}
	return scores
}

// stopWordSet is allocated once at package init time and shared across all calls.
var stopWordSet = func() map[string]bool {
	words := []string{
		"a", "an", "the", "is", "it", "in", "on", "at", "to", "for",
		"of", "and", "or", "but", "not", "with", "this", "that", "was",
		"are", "be", "by", "as", "from", "up", "has", "had", "have",
		"its", "my", "me", "we", "he", "she", "they", "you", "i",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()

// tokenize splits text into lowercase tokens, removing stop words.
func tokenize(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9')
	})
	result := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) > 1 && !stopWordSet[w] {
			result = append(result, w)
		}
	}
	return result
}

// scored is one fact's fused RRF score. Package-level so the debugRanking
// seam can hand it to a test without exporting anything.
type scored struct {
	id    string
	score float64
}

// SignalWeights sets the relative contribution of each retrieval signal to the
// fused ranking. Zero disables a signal entirely.
//
// The defaults are the values that were compile-time constants before v0.10.0.
// Recency is deliberately weighted below the other two: it is a tie-breaker
// that keeps fresh context ahead of equally-relevant stale context, not a
// ranking criterion of its own. Weighted at parity it drowns relevance out on
// any store with a steady write rate.
type SignalWeights struct {
	// Vector weights cosine similarity from the vector store. Ignored when no
	// embedder is configured, since there are no vector ranks to weight.
	Vector float64
	// Keyword weights the TF-IDF approximation over stored text.
	Keyword float64
	// Recency weights the exponential decay from CreatedAt.
	Recency float64
}

// DefaultSignalWeights returns the weights Recall uses when StoreConfig leaves
// SignalWeights nil: vector 1.0, keyword 1.0, recency 0.5.
func DefaultSignalWeights() SignalWeights {
	return SignalWeights{Vector: 1.0, Keyword: 1.0, Recency: 0.5}
}

// kgMaxNeighbors caps how many knowledge-graph neighbour labels Recall may
// append after the ranked facts (ADR-003 condition 2). A constant, not a
// setting: enrichment is a hint with a fixed budget.
const kgMaxNeighbors = 3
