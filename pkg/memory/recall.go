package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// Recall also fires the OnRecall observability hook if configured.

// Recall performs hybrid retrieval for agentID given a query string.
// It fuses three signals via Reciprocal Rank Fusion (RRF):
//  1. Vector similarity (cosine, pluggable VectorStore) — when embeddings available
//  2. Keyword relevance (TF-IDF approximation over bbolt facts)
//  3. Recency score (exponential decay from CreatedAt)
//
// Returns the top-k fact texts, ready to inject into a system prompt.
func (s *Store) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	start := time.Now()
	stored, err := s.List(agentID)
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

	// --- Signal 1: vector similarity ---
	vectorRank := make(map[string]int, topK*2) // factID → rank (1-based)
	vecResults, _ := s.vectorSearch(ctx, agentID, query, topK*2)
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
	sort.Slice(kwSorted, func(i, j int) bool { return kwSorted[i].score > kwSorted[j].score })
	kwRank := make(map[string]int, len(kwSorted))
	for i, e := range kwSorted {
		kwRank[e.id] = i + 1
	}

	// --- Signal 3: recency score ---
	halfLife := s.cfg.DecayHalfLife
	if halfLife == 0 {
		halfLife = 720 * time.Hour // 30 days default
	}
	lambda := math.Log(2) / halfLife.Hours()
	recencyScores := make(map[string]float64, len(facts))
	nowT := s.now()
	for _, f := range facts {
		ageDays := nowT.Sub(f.CreatedAt).Hours()
		recencyScores[f.ID] = math.Exp(-lambda * ageDays)
	}
	type recEntry struct {
		id    string
		score float64
	}
	recSorted := make([]recEntry, 0, len(recencyScores))
	for id, sc := range recencyScores {
		recSorted = append(recSorted, recEntry{id, sc})
	}
	sort.Slice(recSorted, func(i, j int) bool { return recSorted[i].score > recSorted[j].score })
	recRank := make(map[string]int, len(recSorted))
	for i, e := range recSorted {
		recRank[e.id] = i + 1
	}

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
	type scored struct {
		id    string
		score float64
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
	sort.Slice(allScored, func(i, j int) bool { return allScored[i].score > allScored[j].score })

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

	// Build fact lookup.
	factByID := make(map[string]*Fact, len(facts))
	for i := range facts {
		factByID[facts[i].ID] = &facts[i]
	}

	// Collect top-k, updating access metadata along the way.
	if topK > len(allScored) {
		topK = len(allScored)
	}
	result := make([]string, 0, topK)
	seen := make(map[string]bool, topK)
	for _, sc := range allScored[:topK] {
		f, ok := factByID[sc.id]
		if !ok {
			continue
		}
		result = append(result, f.Text)
		seen[f.Text] = true
		// Update access metadata (best-effort, non-blocking).
		f.AccessCount++
		f.AccessedAt = nowT.UTC()
		s.wg.Add(1)
		go func(fact Fact) {
			defer s.wg.Done()
			_ = s.UpdateFact(fact.AgentID, fact)
		}(*f)
	}

	// Enrich with knowledge graph neighbors (optional; graph may be nil).
	s.mu.RLock()
	graph := s.graph
	extractor := s.extractor
	s.mu.RUnlock()
	if graph != nil && extractor != nil && len(result) > 0 {
		// Extract entity IDs from the top-ranked fact and surface neighbors.
		ids, _ := extractor.ExtractIDs(result[0])
		for _, id := range ids {
			neighborTexts, gErr := graph.NeighborTexts(id, 1)
			if gErr != nil {
				break
			}
			for _, nt := range neighborTexts {
				// The graph stores node labels, not fact IDs, so a superseded
				// fact's text can still be reachable as a neighbour. Skip it:
				// the tombstone has to hold on every path into the result.
				if !seen[nt] && !supersededTexts[nt] {
					seen[nt] = true
					result = append(result, nt)
				}
			}
		}
	}

	if s.cfg.OnRecall != nil {
		s.cfg.OnRecall(agentID, query, len(result), time.Since(start))
	}
	return result, nil
}

// keywordScore returns a TF-IDF-like score for each fact against the query.
// It uses simple term frequency over token overlap — no external deps.
func keywordScore(query string, facts []Fact) map[string]float64 {
	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return nil
	}

	// DF: how many facts contain each term.
	df := make(map[string]int, len(queryTerms))
	for _, f := range facts {
		seen := make(map[string]bool)
		for _, t := range tokenize(f.Text) {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}

	n := float64(len(facts))
	scores := make(map[string]float64, len(facts))
	for _, f := range facts {
		terms := tokenize(f.Text)
		tf := make(map[string]int, len(terms))
		for _, t := range terms {
			tf[t]++
		}
		var score float64
		for _, qt := range queryTerms {
			if count, ok := tf[qt]; ok {
				idf := math.Log((n + 1) / (float64(df[qt]) + 1))
				score += float64(count) * idf
			}
		}
		if score > 0 {
			scores[f.ID] = score / float64(len(terms)+1)
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
