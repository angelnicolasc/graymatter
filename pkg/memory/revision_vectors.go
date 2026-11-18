package memory

import (
	"context"
	"errors"
	"math"
	"sort"

	bolt "go.etcd.io/bbolt"
)

const (
	vectorIndexPending     = "pending_vector_index"
	vectorIndexChanged     = "vector_index_changed"
	vectorIndexUnavailable = "vector_index_unavailable"
)

// The queue's namespace sequence records each canonical revision, including
// those without an embedding. It survives a complete enqueue/drain cycle
// while a query is outside bbolt calling the embedding or vector provider.
type recallVectorState struct {
	revision uint64
	pending  bool
}

func (s *Store) noteRevisionVectorTx(tx *bolt.Tx, agentID string) error {
	root := tx.Bucket(bucketPendingVector)
	if root == nil {
		return errors.New("pending vector bucket missing")
	}
	b, err := root.CreateBucketIfNotExists([]byte(agentID))
	if err != nil {
		return err
	}
	_, err = b.NextSequence()
	return err
}

func recallVectorStateTx(tx *bolt.Tx, agentID string) recallVectorState {
	root := tx.Bucket(bucketPendingVector)
	if root == nil {
		return recallVectorState{}
	}
	b := root.Bucket([]byte(agentID))
	if b == nil {
		return recallVectorState{}
	}
	first, _ := b.Cursor().First()
	return recallVectorState{revision: b.Sequence(), pending: first != nil}
}

// External results must describe the canonical population that was selected
// before the query. No bbolt transaction spans the provider round trip.
func (s *Store) searchSnapshotVectors(ctx context.Context, agentID, query string, n int, eligible map[string]int, policy recallPolicy, snapshot recallVectorState, hasTombstones bool) ([]VectorResult, string, error) {
	if snapshot.pending {
		return nil, vectorIndexPending, nil
	}
	if _, unavailable := s.vectors.(unavailableVectorStore); unavailable {
		return nil, vectorIndexUnavailable, nil
	}
	if s.embedder == nil || n <= 0 {
		return nil, "", nil
	}
	restrict := snapshot.revision != 0 || hasTombstones
	results, queryErr := s.vectorSearchWithPolicy(ctx, agentID, query, n, eligible, policy, restrict)
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	var current recallVectorState
	if err := s.db.View(func(tx *bolt.Tx) error {
		current = recallVectorStateTx(tx, agentID)
		return nil
	}); err != nil {
		return nil, "", err
	}
	if current.revision != snapshot.revision {
		return nil, vectorIndexChanged, nil
	}
	if current.pending {
		return nil, vectorIndexPending, nil
	}
	if queryErr != nil && restrict && !errors.Is(queryErr, errConfidenceQueryChanged) {
		return nil, vectorIndexUnavailable, nil
	}
	return results, "", queryErr
}

// Legacy VectorStore implementations cannot prove that stale documents
// did not crowd out live IDs. For revised namespaces, use the stored
// embeddings instead when the backend cannot restrict or exhaust its query.
func (s *Store) searchCanonicalVectors(ctx context.Context, agentID string, query []float32, n int, eligible map[string]int) ([]VectorResult, error) {
	queryNorm := 0.0
	for _, x := range query {
		queryNorm += float64(x) * float64(x)
	}
	if queryNorm == 0 || math.IsNaN(queryNorm) || math.IsInf(queryNorm, 0) {
		return nil, nil
	}

	var results []VectorResult
	err := s.db.View(func(tx *bolt.Tx) error {
		for id := range eligible {
			if err := ctx.Err(); err != nil {
				return err
			}
			fact, err := factInTx(tx, agentID, id)
			if errors.Is(err, ErrFactChanged) {
				continue
			}
			if err != nil {
				return err
			}
			if fact.IsSuperseded() || fact.IsAlias() || len(fact.Embedding) != len(query) {
				continue
			}
			dot, norm := 0.0, 0.0
			for i, x := range fact.Embedding {
				dot += float64(x) * float64(query[i])
				norm += float64(x) * float64(x)
			}
			if norm > 0 {
				results = append(results, VectorResult{ID: id, Similarity: float32(dot / math.Sqrt(norm*queryNorm))})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Similarity != results[j].Similarity {
			return results[i].Similarity > results[j].Similarity
		}
		return results[i].ID < results[j].ID
	})
	return results[:min(len(results), n)], ctx.Err()
}
