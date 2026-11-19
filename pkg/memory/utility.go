package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrUtilityUnsupported = errors.New("compact memory operations unsupported: update and restart the store daemon")

// WriteFact is one member of an atomic write batch. Options remain optional.
type WriteFact struct {
	Text    string       `json:"text"`
	Options WriteOptions `json:"options,omitempty"`
}

type SearchFact struct {
	Subjects     []SubjectProjection `json:"subjects,omitempty"`
	ShortID      string              `json:"short_id,omitempty"`
	AgeDays      float64             `json:"age_days"`
	Evidence     *Citation           `json:"evidence,omitempty"`
	ContentHash  string              `json:"content_hash,omitempty"`
	ID           string              `json:"id,omitempty"`
	Namespace    string              `json:"namespace,omitempty"`
	Text         string              `json:"text"`
	CreatedAt    time.Time           `json:"created_at,omitempty"`
	Receipt      *RecallReceipt      `json:"receipt,omitempty"`
	ReceiptQuery int                 `json:"receipt_query,omitempty"`
}

type ScopedSearchResult struct {
	Facts         []SearchFact       `json:"facts"`
	PerQuery      [][]int            `json:"per_query,omitempty"`
	Feedback      string             `json:"feedback,omitempty"`
	Retrieval     *RetrievalMetadata `json:"retrieval,omitempty"`
	ReviewNotices []ReviewNotice     `json:"review_notices,omitempty"`
	GraphHints    string             `json:"graph_hints,omitempty"`
	SubjectIssues *SubjectIssues     `json:"subject_issues,omitempty"`
	// HasUnshownFacts reports whether the compact result omitted live facts
	// eligible for ranking in the searched scope.
	HasUnshownFacts bool `json:"has_unshown_facts,omitempty"`
}

type UtilityStore interface {
	SearchScoped(context.Context, string, []string, int, bool, RecallOptions) (ScopedSearchResult, error)
	PutManyReturningFacts(context.Context, string, []WriteFact) ([]Fact, error)
}

func ValidateWriteFacts(agentID string, facts []WriteFact) error {
	if strings.TrimSpace(agentID) == "" {
		return errors.New("agent_id is required")
	}
	if len(facts) == 0 {
		return errors.New("facts must contain at least one object")
	}
	subjects := map[string]bool{}
	for i, f := range facts {
		if strings.TrimSpace(f.Text) == "" {
			return fmt.Errorf("facts[%d].text must be non-empty", i)
		}
		if err := f.Options.Validate(); err != nil {
			return fmt.Errorf("facts[%d]: %w", i, err)
		}
		if f.Options.Subject != nil {
			key, _ := NormalizeSubject(*f.Options.Subject)
			if subjects[key] {
				return fmt.Errorf("batch writes the same normalized subject more than once")
			}
			subjects[key] = true
		}
	}
	return nil
}

// SearchScoped retains identities from the ranked snapshot, before any later
// writer can change the canonical rows. Namespace and query merges use the
// existing ordering; text is represented once and query results use indices.
func (s *Store) SearchScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts RecallOptions) (ScopedSearchResult, error) {
	queries = append([]string(nil), queries...)
	if strings.TrimSpace(agentID) == "" {
		return ScopedSearchResult{}, errors.New("agent_id is required")
	}
	if len(queries) == 0 {
		return ScopedSearchResult{}, errors.New("queries must not be empty")
	}
	for _, q := range queries {
		if strings.TrimSpace(q) == "" {
			return ScopedSearchResult{}, errors.New("queries must contain non-empty strings")
		}
	}
	policy, err := s.resolveRecallPolicy(opts)
	if err != nil {
		return ScopedSearchResult{}, err
	}
	policy.collectReceipts = true
	policy.citationIdentity = true
	policy.omitGraphHints = true
	var vectorFallback string
	policy.vectorFallback = &vectorFallback
	namespaces := []string{agentID}
	if all && agentID != SharedAgentID {
		namespaces = append(namespaces, SharedAgentID)
	}
	subjectFence, err := s.subjectReadFence(ctx, namespaces)
	if err != nil {
		return ScopedSearchResult{}, err
	}
	topK = recallTopK(topK)
	rows := make([]BatchResult, 0, len(queries))
	byKey := map[string]Fact{}
	versions := map[string]string{}
	receipts := map[string]RecallReceipt{}
	receiptQueries := map[string]int{}
	var activity recallActivity
	var corpusSize int
	var notices []ReviewNotice
	var feedback []string
	for queryIndex, q := range queries {
		var result RecallResult
		var collected recallActivity
		if all && agentID != SharedAgentID {
			result, collected, err = s.recallAllCollectWithPolicy(ctx, agentID, q, topK, policy, deferRecallActivity)
		} else {
			result.Facts, result.Feedback, collected, err = s.recallDetailedCollectWithPolicy(ctx, agentID, q, topK, policy, deferRecallActivity)
			if err == nil {
				result.ReviewNotices = s.recalledReviewNotices(ctx, collected.facts)
			}
		}
		if err != nil {
			return ScopedSearchResult{}, err
		}
		row := BatchResult{Query: q}
		for _, f := range collected.facts {
			key := f.AgentID + "\x00" + f.ID
			version := NewEvidenceSnapshot(f).ContentHash
			if previous, seen := versions[key]; seen && previous != version {
				return ScopedSearchResult{}, errors.New("scoped search content changed during retrieval; retry the query")
			}
			versions[key] = version
			mergeKey := policy.factKey(f)
			row.Facts = append(row.Facts, mergeKey)
			if prior, ok := byKey[mergeKey]; !ok || prior.AgentID == SharedAgentID && f.AgentID == agentID {
				byKey[mergeKey] = f
			}
		}
		rows = append(rows, row)
		// Every query searches the same scope. Count it once for the merged result.
		corpusSize = max(corpusSize, collected.corpusSize)
		activity.aliases = append(activity.aliases, collected.aliases...)
		for id, r := range collected.receipts {
			if _, exists := receipts[id]; !exists {
				receipts[id] = r
				receiptQueries[id] = queryIndex
			}
		}
		notices = append(notices, result.ReviewNotices...)
		if result.Feedback != "" && !containsString(feedback, result.Feedback) {
			feedback = append(feedback, result.Feedback)
		}
	}
	merged := MergedFacts(rows)
	out := ScopedSearchResult{
		Facts:           make([]SearchFact, 0, len(merged)),
		Retrieval:       policy.retrieval(),
		ReviewNotices:   MergeReviewNotices(notices),
		Feedback:        strings.Join(feedback, "\n"),
		HasUnshownFacts: len(merged) < corpusSize,
	}
	// Graph labels and extracted links lack a canonical namespace identity.
	// The compact protocol cannot safely represent them as scoped fact rows.
	s.mu.RLock()
	graphHintsConfigured := s.graph != nil && s.extractor != nil
	s.mu.RUnlock()
	if graphHintsConfigured && !policy.filtered() {
		out.GraphHints = "Graph hints omitted: no namespace identity."
	}
	positions := map[string]int{}
	for _, key := range merged {
		positions[key] = len(out.Facts)
		item := SearchFact{}
		if f, ok := byKey[key]; ok {
			item.Text = f.Text
			item.ID, item.Namespace, item.CreatedAt = f.ID, f.AgentID, f.CreatedAt
			if f.Evidence != nil {
				item.Evidence = f.Evidence
				item.ContentHash = NewEvidenceSnapshot(f).ContentHash
			}
			activity.facts = append(activity.facts, f)
			if r, ok := receipts[f.AgentID+"\x00"+f.ID]; ok {
				item.Receipt = &r
				item.ReceiptQuery = receiptQueries[f.AgentID+"\x00"+f.ID]
			}
		} else {
			return ScopedSearchResult{}, errors.New("scoped search returned a fact without canonical identity")
		}
		out.Facts = append(out.Facts, item)
	}
	// Match RecallExplain's durable lineage projection without another recall.
	// The receipt's version hash keeps this read bound to the selected content.
	for _, namespace := range namespaces {
		var selected []RecallReceipt
		var indices []int
		for i, fact := range out.Facts {
			if fact.Namespace == namespace && fact.Receipt != nil {
				selected = append(selected, *fact.Receipt)
				indices = append(indices, i)
			}
		}
		if err := s.annotateLineageReceipts(ctx, selected, namespace); err != nil {
			return ScopedSearchResult{}, err
		}
		for i, index := range indices {
			out.Facts[index].Receipt = &selected[i]
		}
	}
	if len(queries) > 1 {
		for _, row := range rows {
			indices := make([]int, 0, len(row.Facts))
			for _, text := range row.Facts {
				indices = append(indices, positions[text])
			}
			out.PerQuery = append(out.PerQuery, indices)
		}
	}
	if err := ctx.Err(); err != nil {
		return ScopedSearchResult{}, err
	}
	if err := s.projectSubjectSearch(ctx, subjectFence, namespaces, &out, activity.facts); err != nil {
		return ScopedSearchResult{}, err
	}
	s.touchRecallActivity(activity)
	return out, nil
}

func containsString(values []string, value string) bool {
	for _, s := range values {
		if s == value {
			return true
		}
	}
	return false
}

// PutManyReturningFacts commits the entire batch or nothing. A successful
// return identifies the committed rows. Retrying an external call is a new
// operation; transports must not replay a write after losing its response.
func (s *Store) PutManyReturningFacts(ctx context.Context, agentID string, inputs []WriteFact) ([]Fact, error) {
	receipts, err := s.putManyWithReceipts(ctx, agentID, inputs, false)
	if err != nil {
		return nil, err
	}
	facts := make([]Fact, len(receipts))
	for i, r := range receipts {
		facts[i] = r.Fact
	}
	return facts, nil
}

func (s *Store) PutManyReturningReceipts(ctx context.Context, agentID string, inputs []WriteFact) ([]WriteReceipt, error) {
	return s.putManyWithReceipts(ctx, agentID, inputs, true)
}

func (s *Store) putManyWithReceipts(ctx context.Context, agentID string, inputs []WriteFact, suggest bool) ([]WriteReceipt, error) {
	// Snapshot caller-owned options before validation and embedding callbacks.
	// Validation must describe the same values that eventually commit.
	cloned := make([]WriteFact, len(inputs))
	copy(cloned, inputs)
	for i := range cloned {
		cloned[i].Options = cloned[i].Options.Clone()
	}
	inputs = cloned
	if err := ValidateWriteFacts(agentID, inputs); err != nil {
		return nil, err
	}
	for i, input := range inputs {
		if err := s.validateCitationOptions(input.Options); err != nil {
			return nil, fmt.Errorf("facts[%d]: %w", i, err)
		}
	}
	if s.readOnly {
		return nil, ErrStoreReadOnly
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start := time.Now()
	facts := make([]Fact, len(inputs))
	receipts := make([]WriteReceipt, len(inputs))
	embedErrors := make([]error, len(inputs))
	for i, input := range inputs {
		var emb []float32
		if s.embedder != nil {
			emb, embedErrors[i] = s.embedder.Embed(ctx, input.Text)
			if embedErrors[i] != nil {
				emb = nil
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		facts[i] = newFact(agentID, input.Text, emb, s.now())
		facts[i].Kind = KindFact
		if input.Options.Confidence != nil {
			facts[i].Confidence = *input.Options.Confidence
		}
	}
	if err := s.updateSubjectReadView(agentID, true, func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		plans, err := subjectWritePlansTx(tx, agentID, inputs, suggest)
		if err != nil {
			return err
		}
		b, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte(agentID))
		if err != nil {
			return err
		}
		var evidenceAdded int64
		collect, revised := false, false
		for i := range facts {
			f := &facts[i]
			receipt, dropped, err := s.applySubjectWriteTx(tx, f, inputs[i].Options, plans[i], &evidenceAdded)
			if err != nil {
				return err
			}
			receipts[i] = receipt
			collect = collect || dropped
			revised = revised || len(plans[i].Sources) > 0
			data, err := f.marshal()
			if err != nil {
				return err
			}
			if err = b.Put([]byte(f.ID), data); err != nil {
				return err
			}
			if s.cfg.CandidateRetrieval {
				if err = idxAddFact(tx, *f, s.cfg.StemKeywords); err != nil {
					return err
				}
				if err = idxBumpCount(tx, agentID, 1, s.cfg.StemKeywords); err != nil {
					return err
				}
			} else if err = idxInvalidate(tx, agentID); err != nil {
				return err
			}
			if len(f.Embedding) > 0 {
				pb, err := tx.Bucket(bucketPendingVector).CreateBucketIfNotExists([]byte(agentID))
				if err != nil {
					return err
				}
				if err = pb.Put([]byte(f.ID), []byte{1}); err != nil {
					return err
				}
			}
		}
		if collect {
			if err := collectLineageTx(tx, agentID, s.now().UTC()); err != nil {
				return err
			}
		}
		if revised {
			if err := s.noteRevisionVectorTx(tx, agentID); err != nil {
				return err
			}
		}
		if evidenceAdded > 0 {
			if err := checkEvidenceQuotaTx(tx, agentID, evidenceAdded); err != nil {
				return err
			}
		}
		if err = tx.Bucket(bucketAgents).Put([]byte(agentID), []byte("1")); err != nil {
			return err
		}
		return ctx.Err()
	}); err != nil {
		return nil, fmt.Errorf("put facts: %w", err)
	}
	for i, f := range facts {
		if len(f.Embedding) > 0 {
			s.recordEmbedDimensions(len(f.Embedding))
			if err := s.addToVector(ctx, agentID, f); err != nil {
				if s.cfg.OnVectorIndexError != nil {
					s.cfg.OnVectorIndexError(agentID, f.ID, err)
				}
			} else {
				s.clearPendingVector(agentID, f.ID)
			}
		} else if embedErrors[i] != nil {
			recordEmbedDegraded(s.db, embedErrors[i])
		}
		if s.cfg.OnPut != nil {
			s.cfg.OnPut(agentID, f.ID, time.Since(start))
		}
	}
	return receipts, nil
}
