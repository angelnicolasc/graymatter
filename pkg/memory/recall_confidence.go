package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// recallPolicy is immutable request state. It owns copied option values so a
// request never changes the store configuration or another query's policy.
type recallPolicy struct {
	collectReceipts  bool
	citationIdentity bool // core keeps independently cited versions distinct
	omitGraphHints   bool // compact results require canonical namespace identities
	preview          bool // inspection suppresses every retrieval hook and durable side effect
	minimum          *string
	weight           float64
	metadata         bool
	embedding        *confidenceEmbeddingCache
	vectorFallback   *string
}

// A single pipeline may fall back from the compact index to a canonical scan
// after an overlapping mutation. It reuses the same embedding, never embeds
// twice or silently switches an alias-expanded query within one request.
type confidenceEmbeddingCache struct {
	loaded bool
	query  string
	value  []float32
	err    error
}

var errConfidenceQueryChanged = errors.New("confidence recall effective query changed during retrieval; retry the query")

func (s *Store) embedConfidenceQuery(ctx context.Context, query string, p recallPolicy) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.embedder == nil || s.embedder.Dimensions() == 0 {
		return nil, nil
	}
	if p.embedding == nil {
		return s.embedder.Embed(ctx, query)
	}
	if p.embedding.loaded {
		if p.embedding.query != query {
			return nil, errConfidenceQueryChanged
		}
		return p.embedding.value, p.embedding.err
	}
	p.embedding.loaded, p.embedding.query = true, query
	p.embedding.value, p.embedding.err = s.embedder.Embed(ctx, query)
	return p.embedding.value, p.embedding.err
}

// DefaultConfidenceWeight exposes the configured request default to optional
// adapters so direct and negotiated RPC calls resolve the same policy once.
func (s *Store) DefaultConfidenceWeight() float64 { return s.cfg.ConfidenceWeight }

func (s *Store) resolveRecallPolicy(opts RecallOptions) (recallPolicy, error) {
	if err := opts.Validate(); err != nil {
		return recallPolicy{}, err
	}
	p := recallPolicy{weight: s.cfg.ConfidenceWeight, metadata: opts.Requested()}
	if opts.ConfidenceWeight != nil {
		p.weight = *opts.ConfidenceWeight
	}
	if opts.MinConfidence != nil {
		v := *opts.MinConfidence
		p.minimum = &v
	}
	if p.weight != 0 {
		p.metadata = true
	}
	return p, nil
}

func (p recallPolicy) filtered() bool { return p.minimum != nil }
func (p recallPolicy) eligible(raw string) bool {
	return p.minimum == nil || ConfidenceLevel(raw) >= ConfidenceLevel(*p.minimum)
}
func (p recallPolicy) factor(raw string) float64 {
	return 1 + p.weight*float64(ConfidenceLevel(raw)-1)
}
func (p recallPolicy) noteVectorFallback(reason string) {
	if reason != "" && p.vectorFallback != nil && *p.vectorFallback == "" {
		*p.vectorFallback = reason
	}
}

func (p recallPolicy) retrieval() *RetrievalMetadata {
	var vectorFallback string
	if p.vectorFallback != nil {
		vectorFallback = *p.vectorFallback
	}
	if !p.metadata && vectorFallback == "" {
		return nil
	}
	kg := "hints_without_confidence_receipts"
	if p.filtered() {
		kg = "suppressed_min_confidence"
	} else if p.omitGraphHints {
		kg = "suppressed_namespace_scope"
	}
	return &RetrievalMetadata{MinConfidence: p.minimum, ConfidenceWeight: p.weight, Policy: ConfidencePolicy, KG: kg, VectorFallback: vectorFallback}
}

func recallTopK(topK int) int {
	if topK <= 0 {
		return 8
	}
	return topK
}

func vectorBudget(topK int) int {
	maxInt := int(^uint(0) >> 1)
	if topK > maxInt/2 {
		return maxInt
	}
	return topK * 2
}

// RecallWithOptions applies confidence filtering before every retrieval signal
// and a bounded preference to the fused score. Metadata is also returned when
// the eligible corpus is empty.
func (s *Store) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts RecallOptions) (RecallResult, error) {
	p, err := s.resolveRecallPolicy(opts)
	if err != nil {
		return RecallResult{}, err
	}
	var vectorFallback string
	p.vectorFallback = &vectorFallback
	facts, feedback, activity, err := s.recallDetailedCollectWithPolicy(ctx, agentID, query, recallTopK(topK), p, touchImmediately)
	return RecallResult{Facts: facts, Feedback: feedback, Retrieval: p.retrieval(), ReviewNotices: s.recalledReviewNotices(ctx, activity.facts)}, err
}

// RecallExplainWithOptions reads out the same ranking that selects plain facts.
func (s *Store) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts RecallOptions) (RecallExplainResult, error) {
	p, err := s.resolveRecallPolicy(opts)
	if err != nil {
		return RecallExplainResult{}, err
	}
	var vectorFallback string
	p.vectorFallback = &vectorFallback
	receipts, err := s.recallExplainWithPolicy(ctx, agentID, query, recallTopK(topK), p)
	return RecallExplainResult{Receipts: receipts, Retrieval: p.retrieval()}, err
}

func (s *Store) RecallSharedWithOptions(ctx context.Context, query string, topK int, opts RecallOptions) (RecallResult, error) {
	return s.RecallWithOptions(ctx, SharedAgentID, query, topK, opts)
}

// RecallAllWithOptions preserves the namespace RRF merge policy after applying
// exactly one effective policy to each input ranking.
func (s *Store) RecallAllWithOptions(ctx context.Context, agentID, query string, topK int, opts RecallOptions) (RecallResult, error) {
	p, err := s.resolveRecallPolicy(opts)
	if err != nil {
		return RecallResult{}, err
	}
	var vectorFallback string
	p.vectorFallback = &vectorFallback
	return s.recallAllWithPolicy(ctx, agentID, query, recallTopK(topK), p)
}

// Both public All methods share collection and bookkeeping. The older method
// supplies its original raw topK semantics; option-aware calls normalize it.
func (s *Store) recallAllWithPolicy(ctx context.Context, agentID, query string, topK int, p recallPolicy) (RecallResult, error) {
	result, _, err := s.recallAllCollectWithPolicy(ctx, agentID, query, topK, p, touchImmediately)
	return result, err
}

func (s *Store) recallAllCollectWithPolicy(ctx context.Context, agentID, query string, topK int, p recallPolicy, mode recallTouchMode) (RecallResult, recallActivity, error) {
	agent, af, agentActivity, err := s.recallDetailedCollectWithPolicy(ctx, agentID, query, topK, p, deferRecallActivity)
	if err != nil {
		return RecallResult{}, recallActivity{}, fmt.Errorf("recall agent: %w", err)
	}
	shared, sf, sharedActivity, err := s.recallDetailedCollectWithPolicy(ctx, SharedAgentID, query, topK, p, deferRecallActivity)
	if err != nil {
		return RecallResult{}, recallActivity{}, fmt.Errorf("recall shared: %w", err)
	}
	feedback := af
	if sf != "" && sf != af {
		if feedback != "" {
			feedback += "\n"
		}
		feedback += sf
	}
	facts := fuseRecallResults(agent, shared, topK)
	selectedByText := make(map[string]Fact, len(agentActivity.facts)+len(sharedActivity.facts))
	// Namespace fusion gives agent facts the stable preference on a text
	// duplicate. Bookkeeping follows that same canonical identity rather than
	// touching every duplicate that contributed to the merged ranking.
	for _, f := range agentActivity.facts {
		selectedByText[f.Text] = f
	}
	for _, f := range sharedActivity.facts {
		if _, exists := selectedByText[f.Text]; !exists {
			selectedByText[f.Text] = f
		}
	}
	touched := make([]Fact, 0, len(facts))
	for _, text := range facts {
		if f, exists := selectedByText[text]; exists {
			touched = append(touched, f)
		}
	}
	if p.citationIdentity {
		facts, touched = fuseCitationFacts(agentActivity.facts, sharedActivity.facts, topK, p)
	}
	activity := recallActivity{
		corpusSize: agentActivity.corpusSize + sharedActivity.corpusSize,
		facts:      touched,
		aliases:    append(agentActivity.aliases, sharedActivity.aliases...),
	}
	if p.collectReceipts {
		activity.receipts = make(map[string]RecallReceipt, len(touched))
		for _, f := range touched {
			key := f.AgentID + "\x00" + f.ID
			if r, ok := agentActivity.receipts[key]; ok {
				activity.receipts[key] = r
			} else if r, ok := sharedActivity.receipts[key]; ok {
				activity.receipts[key] = r
			}
		}
	}
	s.recordRecallActivity(activity, mode)
	return RecallResult{Facts: facts, Feedback: feedback, Retrieval: p.retrieval(), ReviewNotices: s.recalledReviewNotices(ctx, touched)}, activity, nil
}

// Untouched namespaces preserve the existing vector query contract. Revised
// namespaces exclude retired IDs before the backend's result budget is cut.
func (s *Store) vectorSearchWithPolicy(ctx context.Context, agentID, query string, n int, eligible map[string]int, p recallPolicy, revised bool) ([]VectorResult, error) {
	if !p.filtered() && !revised {
		var embedding []float32
		var err error
		if p.embedding == nil {
			embedding, err = s.embedder.Embed(ctx, query)
			if err != nil {
				return nil, nil
			}
		} else {
			embedding, err = s.embedConfidenceQuery(ctx, query, p)
		}
		if err != nil || len(embedding) == 0 {
			return nil, err
		}
		results, err := s.vectors.Query(ctx, agentID, embedding, n)
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			if _, live := eligible[result.ID]; !live {
				// An older store can retain external vectors after their
				// canonical tombstones were pruned. The limited result set
				// cannot establish which live vectors they displaced.
				return s.searchCanonicalVectors(ctx, agentID, embedding, n, eligible)
			}
		}
		return results, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.embedder == nil || s.embedder.Dimensions() == 0 {
		return nil, nil
	}
	emb, err := s.embedConfidenceQuery(ctx, query, p)
	if err != nil {
		return nil, fmt.Errorf("filtered vector embedding: %w", err)
	}
	if len(emb) == 0 {
		if !p.filtered() {
			return nil, nil
		}
		return nil, fmt.Errorf("filtered vector embedding is empty")
	}
	restricted, restrictedOK := s.vectors.(EligibleVectorStore)
	exhaustive, exhaustiveOK := s.vectors.(ExhaustiveVectorStore)
	if !restrictedOK && !exhaustiveOK {
		if revised {
			return s.searchCanonicalVectors(ctx, agentID, emb, n, eligible)
		}
		return nil, fmt.Errorf("filtered vector retrieval requires eligible-query or explicit-exhaustion capability")
	}
	ids := make(map[string]bool, len(eligible))
	for id := range eligible {
		ids[id] = true
	}
	if restrictedOK {
		results, exhausted, err := restricted.QueryEligible(ctx, agentID, emb, n, ids)
		if err != nil {
			return nil, err
		}
		if err := validateConfidenceVectors(results); err != nil {
			if revised {
				return s.searchCanonicalVectors(ctx, agentID, emb, n, eligible)
			}
			return nil, err
		}
		ordered := orderedEligibleVectors(results, ids)
		if revised && (len(results) > n || len(ordered) != len(results) ||
			len(ordered) < min(n, len(eligible)) || len(ordered) < n && !exhausted) {
			return s.searchCanonicalVectors(ctx, agentID, emb, n, eligible)
		}
		results = ordered
		if len(results) < n && !exhausted {
			return nil, fmt.Errorf("filtered vector query incomplete: exhaustion was not established")
		}
		if len(results) > n {
			results = results[:n]
		}
		return results, ctx.Err()
	}
	requested := n
	seen := make(map[string]VectorResult)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results, exhausted, err := exhaustive.QueryExhaustive(ctx, agentID, emb, requested)
		if err != nil {
			return nil, err
		}
		progress := false
		prefix := make(map[string]bool, len(results))
		for _, r := range results {
			if math.IsNaN(float64(r.Similarity)) || math.IsInf(float64(r.Similarity), 0) {
				return nil, fmt.Errorf("filtered vector backend returned non-finite similarity")
			}
			if _, exists := seen[r.ID]; !exists {
				progress = true
				seen[r.ID] = r
			} else if seen[r.ID].Similarity != r.Similarity {
				return nil, fmt.Errorf("filtered vector query incomplete: backend changed an established prefix similarity")
			}
			prefix[r.ID] = true
		}
		for id := range seen {
			if !prefix[id] {
				return nil, fmt.Errorf("filtered vector query incomplete: backend did not extend its established prefix")
			}
		}
		all := make([]VectorResult, 0, len(seen))
		for _, r := range seen {
			all = append(all, r)
		}
		all = orderedEligibleVectors(all, ids)
		if len(all) >= n {
			return all[:n], nil
		}
		if exhausted {
			if revised && len(all) < min(n, len(eligible)) {
				return s.searchCanonicalVectors(ctx, agentID, emb, n, eligible)
			}
			return all, nil
		}
		if !progress {
			return nil, fmt.Errorf("filtered vector query incomplete: backend made no progress without exhaustion")
		}
		maxInt := int(^uint(0) >> 1)
		if requested >= maxInt {
			return nil, fmt.Errorf("filtered vector query incomplete: neighbor budget overflow")
		}
		if requested > maxInt/2 {
			requested = maxInt
		} else {
			requested *= 2
		}
	}
}

func validateConfidenceVectors(results []VectorResult) error {
	for _, r := range results {
		if math.IsNaN(float64(r.Similarity)) || math.IsInf(float64(r.Similarity), 0) {
			return fmt.Errorf("filtered vector backend returned non-finite similarity")
		}
	}
	return nil
}
