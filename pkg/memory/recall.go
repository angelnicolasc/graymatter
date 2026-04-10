package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// Recall performs hybrid retrieval for agentID given a query string.
// It fuses three signals via Reciprocal Rank Fusion (RRF):
//  1. Vector similarity (cosine, via chromem-go) — when embeddings available
//  2. Keyword relevance (TF-IDF approximation over bbolt facts)
//  3. Recency score (exponential decay from CreatedAt)
//
// Returns the top-k fact texts, ready to inject into a system prompt.
func (s *Store) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	facts, err := s.List(agentID)
	if err != nil || len(facts) == 0 {
		return nil, err
	}

	// --- Signal 1: vector similarity ---
	vectorRank := make(map[string]int) // factID → rank (1-based)
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
	for _, f := range facts {
		ageDays := time.Since(f.CreatedAt).Hours()
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
	const k = 60.0
	type scored struct {
		id    string
		score float64
	}
	candidates := make(map[string]float64, len(facts))
	for _, f := range facts {
		rrf := 0.0
		if r, ok := vectorRank[f.ID]; ok {
			rrf += 1.0 / (k + float64(r))
		}
		if r, ok := kwRank[f.ID]; ok {
			rrf += 1.0 / (k + float64(r))
		}
		if r, ok := recRank[f.ID]; ok {
			rrf += 0.5 / (k + float64(r)) // recency gets half weight
		}
		candidates[f.ID] = rrf
	}

	allScored := make([]scored, 0, len(candidates))
	for id, sc := range candidates {
		allScored = append(allScored, scored{id, sc})
	}
	sort.Slice(allScored, func(i, j int) bool { return allScored[i].score > allScored[j].score })

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
		f.AccessedAt = time.Now().UTC()
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
				if !seen[nt] {
					seen[nt] = true
					result = append(result, nt)
				}
			}
		}
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

// tokenize splits text into lowercase tokens, removing stop words.
func tokenize(text string) []string {
	stop := stopWords()
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9')
	})
	result := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) > 1 && !stop[w] {
			result = append(result, w)
		}
	}
	return result
}

func stopWords() map[string]bool {
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
}
