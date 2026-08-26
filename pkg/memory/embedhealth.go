package memory

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Embedding health is the store-observed truth about the vector channel:
// not what the configuration intends, but what actually happened to facts
// as they were written. The embeddings chain degrades silently by design —
// a fact whose embedder failed is stored keyword-only and Put still returns
// nil — so without a durable record, a broken backend looks identical to an
// empty store. These counters are that record.

const (
	metaKeyVectorDegraded     = "vector_degraded"
	metaKeyVectorDegradedLast = "vector_degraded_last"
	maxDegradedErrBytes       = 256
)

// EmbeddingHealth reports the store's observed embedding state.
type EmbeddingHealth struct {
	// EmbedDims is the dimension declared by the first provider that opened
	// this store for writing (recorded eagerly at Open). 0 means every open
	// so far used a provider without declared dimensions — keyword-only.
	// Declared intent, not proof of indexing: use CountEmbeddings for what
	// actually landed.
	EmbedDims int `json:"embed_dims"`
	// DegradedFacts counts lifetime writes where the embedder returned an
	// error. Keyword providers that return (nil, nil) do not count: that is
	// supported configuration, not failure.
	DegradedFacts int `json:"degraded_facts"`
	// LastDegradError is the most recent embedder error, truncated. Empty
	// when nothing has degraded.
	LastDegradError string `json:"last_degrade_error,omitempty"`
	// PendingVectors is the current size of the retry queue for facts whose
	// bbolt write succeeded but whose vector upsert has not landed yet.
	PendingVectors int `json:"pending_vectors"`
}

// recordEmbedDegraded bumps the degradation counter and stores the last
// error for doctor --embeddings. A diagnostic must never be able to fail
// the write it is describing; callers ignore the error deliberately.
func recordEmbedDegraded(db *bolt.DB, embedErr error) {
	if embedErr == nil {
		return
	}
	msg := embedErr.Error()
	if len(msg) > maxDegradedErrBytes {
		msg = msg[:maxDegradedErrBytes]
	}
	_ = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return nil // read-only or pre-bucketing open: nothing to record into
		}
		cur := 0
		if v := b.Get([]byte(metaKeyVectorDegraded)); v != nil {
			n, err := strconv.Atoi(string(v))
			if err != nil {
				return fmt.Errorf("meta key %q: %w", metaKeyVectorDegraded, err)
			}
			cur = n
		}
		if err := b.Put([]byte(metaKeyVectorDegraded), []byte(strconv.Itoa(cur+1))); err != nil {
			return err
		}
		return b.Put([]byte(metaKeyVectorDegradedLast), []byte(
			strconv.Itoa(int(time.Now().Unix()))+"\x00"+msg))
	})
}

// ReadEmbeddingHealth reads the recorded embedding state from a database.
// Exported alongside ReadConsolidationCounters for the same reason: every
// surface that displays it reads the same numbers without owning the write
// logic.
func ReadEmbeddingHealth(db *bolt.DB) (EmbeddingHealth, error) {
	h := EmbeddingHealth{}
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte("embed_dims")); v != nil {
			_ = json.Unmarshal(v, &h.EmbedDims)
		}
		if v := b.Get([]byte(metaKeyVectorDegraded)); v != nil {
			n, err := strconv.Atoi(string(v))
			if err == nil {
				h.DegradedFacts = n
			}
		}
		if v := b.Get([]byte(metaKeyVectorDegradedLast)); v != nil {
			h.LastDegradError = lastDegradedMessage(v)
		}
		return nil
	})
	if err != nil {
		return EmbeddingHealth{}, err
	}
	return h, nil
}

// lastDegradedMessage strips the timestamp prefix written by
// recordEmbedDegraded and returns the message part only.
func lastDegradedMessage(raw []byte) string {
	for i, c := range raw {
		if c == 0 {
			return string(raw[i+1:])
		}
	}
	return string(raw)
}

// EmbeddingHealth reads this store's observed embedding state.
func (s *Store) EmbeddingHealth() (EmbeddingHealth, error) {
	h, err := ReadEmbeddingHealth(s.db)
	if err != nil {
		return EmbeddingHealth{}, err
	}
	h.PendingVectors = s.PendingVectorCount()
	return h, nil
}

// CountEmbeddings returns the number of live facts carrying a vector,
// against the number of live facts total, across all agents. Tombstones are
// excluded on both sides: retired facts say nothing about the channel's
// present state.
func (s *Store) CountEmbeddings() (withVector, total int, err error) {
	agents, err := s.ListAgents()
	if err != nil {
		return 0, 0, err
	}
	for _, a := range agents {
		facts, err := s.List(a)
		if err != nil {
			return 0, 0, err
		}
		for _, f := range facts {
			if f.IsSuperseded() {
				continue
			}
			total++
			if len(f.Embedding) > 0 {
				withVector++
			}
		}
	}
	return withVector, total, nil
}
