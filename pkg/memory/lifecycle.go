package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrFactChanged means a required source disappeared or changed before a
// derived write could commit. Callers may obtain a fresh snapshot and retry.
var ErrFactChanged = errors.New("fact changed before mutation")

// errFactUnchanged rolls back a no-op patch: committing even an unchanged
// bbolt write transaction would still write freelist and metadata pages.
var errFactUnchanged = errors.New("fact unchanged")

func factInTx(tx *bolt.Tx, agentID, id string) (Fact, error) {
	parent := tx.Bucket(bucketFacts)
	if parent == nil {
		return Fact{}, fmt.Errorf("%w: fact %s does not exist", ErrFactChanged, id)
	}
	b := parent.Bucket([]byte(agentID))
	if b == nil || b.Get([]byte(id)) == nil {
		return Fact{}, fmt.Errorf("%w: fact %s does not exist", ErrFactChanged, id)
	}
	f, err := unmarshalFact(b.Get([]byte(id)))
	if err != nil {
		return Fact{}, err
	}
	if f.AgentID != agentID || f.ID != id {
		return Fact{}, fmt.Errorf("%w: fact identity mismatch", ErrFactChanged)
	}
	if err := hydrateLineageTx(tx, &f); err != nil {
		return Fact{}, err
	}
	return f, nil
}

// patchFact reads fresh state under the write lock. Its callback changes only
// the intended fields, preserving confidence and all unrelated metadata.
// Returning errFactUnchanged without modifying the fact skips persistence and
// returns the current fact successfully after rolling back the transaction.
func (s *Store) patchFact(agentID, id string, patch func(*Fact) error) (Fact, error) {
	if s.readOnly {
		return Fact{}, ErrStoreReadOnly
	}
	var updated Fact
	err := s.updateSubjectReadView(agentID, true, func(tx *bolt.Tx) error {
		current, err := factInTx(tx, agentID, id)
		if err != nil {
			return err
		}
		wasLive := !current.IsSuperseded()
		if err := patch(&current); err != nil {
			if err == errFactUnchanged {
				updated = current
			}
			return err
		}
		if err := s.persistFactTx(tx, tx.Bucket(bucketFacts).Bucket([]byte(agentID)), agentID, current); err != nil {
			return err
		}
		updated = current
		if wasLive && current.IsSuperseded() && lineageHeadKey(tx, agentID, id) != "" {
			return collectLineageTx(tx, agentID, s.now().UTC())
		}
		return nil
	})
	if err == errFactUnchanged {
		err = nil
	}
	return updated, err
}

func (s *Store) retireFact(agentID string, victim Fact, replacementID string, requireUnpinned bool) error {
	return s.retireSnapshotFact(agentID, victim, replacementID, requireUnpinned, false)
}

// retireDerivedFact keeps the validated confidence snapshot through the
// retirement phase. A replacement may already be committed when the source
// changes, so the caller must surface that partial outcome with its identity.
func (s *Store) retireDerivedFact(agentID string, victim Fact, replacementID string, requireUnpinned bool) error {
	return s.retireSnapshotFact(agentID, victim, replacementID, requireUnpinned, true)
}

func (s *Store) retireSnapshotFact(agentID string, victim Fact, replacementID string, requireUnpinned, requireConfidence bool) error {
	if victim.AgentID != agentID {
		return fmt.Errorf("retire: fact belongs to another agent")
	}
	_, err := s.patchFact(agentID, victim.ID, func(current *Fact) error {
		if current.IsSuperseded() || current.Text != victim.Text || current.Kind != victim.Kind || (requireUnpinned && current.Pinned) || (requireConfidence && current.Confidence != victim.Confidence) {
			return fmt.Errorf("%w: cannot retire fact %s", ErrFactChanged, victim.ID)
		}
		current.SupersededBy = replacementID
		return nil
	})
	return err
}

// SetPinned changes pin metadata on exact live IDs without replaying a stale
// confidence, access counter, weight or tombstone from the caller's snapshot.
func (s *Store) SetPinned(agentID string, pinned bool, victims ...Fact) error {
	for _, v := range victims {
		if v.AgentID != agentID || (pinned && v.IsSuperseded()) {
			return fmt.Errorf("pin: invalid or superseded fact %s", v.ID)
		}
	}
	var errs []error
	at := s.now().UTC()
	for _, v := range victims {
		_, err := s.patchFact(agentID, v.ID, func(current *Fact) error {
			if (pinned && current.IsSuperseded()) || current.Text != v.Text || current.Kind != v.Kind {
				return fmt.Errorf("%w: cannot pin fact %s", ErrFactChanged, v.ID)
			}
			current.Pinned = pinned
			current.PinnedAt = time.Time{}
			if pinned {
				current.PinnedAt = at
			}
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// agingExempt preserves pins and live authored vocabulary, including aliases
// written before AliasSource existed. Retired aliases age like other rows.
func (f Fact) agingExempt() bool {
	return f.Pinned || (f.IsAlias() && !f.IsSuperseded() && f.AliasSource == "")
}

const minimumFactWeight = 0.01

func weightAtAge(accessedAt, now time.Time, lambda float64) float64 {
	return math.Exp(-lambda * now.Sub(accessedAt).Hours())
}

// Alias aging follows its source-specific policy. The age guard protects ordinary live facts.
func pruneEligible(f Fact, now time.Time, lambda float64) bool {
	return !f.agingExempt() && f.Weight < minimumFactWeight &&
		(f.IsSuperseded() || f.IsAlias() || weightAtAge(f.AccessedAt, now, lambda) < minimumFactWeight)
}

const maintenanceDecayBatchSize = 256

// decayBatch rereads each selected identity under the writer lock. A batch
// whose durable values are already current rolls back without a metadata write.
func (s *Store) decayBatch(ctx context.Context, namespace string, selected []Fact, now time.Time, lambda float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.updateSubjectReadMetadata(func(tx *bolt.Tx) error {
		rows := lineageRowBucket(tx, namespace)
		if rows == nil {
			return errFactUnchanged
		}
		state, indexed := idxReadState(tx, namespace)
		keepPostings := s.cfg.CandidateRetrieval && indexed && state.Version == indexVersion &&
			state.Stemmed == s.cfg.StemKeywords && s.idxCounted(namespace)
		changed := false
		for _, entry := range selected {
			if rows.Get([]byte(entry.ID)) == nil {
				continue // a concurrent delete already collected the row
			}
			current, err := factInTx(tx, namespace, entry.ID)
			if err != nil {
				return fmt.Errorf("decay fact %s: %w", entry.ID, err)
			}
			if current.agingExempt() {
				continue
			}
			weight := math.Min(current.Weight, weightAtAge(current.AccessedAt, now, lambda))
			if weight == current.Weight {
				continue
			}
			current.Weight = weight
			if keepPostings {
				err = s.persistDecayedFactTx(tx, rows, namespace, current)
			} else {
				err = s.persistFactTx(tx, rows, namespace, current)
			}
			if err != nil {
				return fmt.Errorf("decay fact %s: %w", entry.ID, err)
			}
			changed = true
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !changed {
			return errFactUnchanged
		}
		return nil
	})
	if err == errFactUnchanged {
		return nil
	}
	return err
}

// persistDecayedFactTx changes only Weight, which affects neither postings
// nor the recency spine. Preserve citation validation and the index write
// stamp used by cached readers.
func (s *Store) persistDecayedFactTx(tx *bolt.Tx, rows *bolt.Bucket, namespace string, fact Fact) error {
	if err := hydrateCitationTx(tx, &fact); err != nil {
		return err
	}
	data, err := fact.marshal()
	if err != nil {
		return err
	}
	if err := rows.Put([]byte(fact.ID), data); err != nil {
		return err
	}
	return idxBumpCount(tx, namespace, 0, s.cfg.StemKeywords)
}

func (s *Store) pruneFact(agentID, id string, now time.Time, lambda float64) error {
	if s.readOnly {
		return ErrStoreReadOnly
	}
	return s.updateSubjectReadView(agentID, true, func(tx *bolt.Tx) error {
		current, err := factInTx(tx, agentID, id)
		if errors.Is(err, ErrFactChanged) {
			return nil // a concurrent delete already collected it
		}
		if err != nil {
			return err
		}
		if !pruneEligible(current, now, lambda) {
			return nil
		}
		if pending, err := pendingLegacyEvidenceTx(tx, current); err != nil || pending {
			return err
		}
		return s.deleteFactTx(tx, agentID, id)
	})
}

// sourceMatches checks fields used to derive content and confidence. Access
// statistics and decay may advance without invalidating a content snapshot.
func sourceMatches(snapshot, current Fact, requireUnpinned bool) bool {
	return current.AgentID == snapshot.AgentID && current.ID == snapshot.ID &&
		current.Text == snapshot.Text && current.Kind == snapshot.Kind &&
		current.Confidence == snapshot.Confidence && !current.IsSuperseded() &&
		(!requireUnpinned || !current.Pinned)
}

func derivedConfidence(facts []Fact) string {
	confidence := "inferred"
	for _, f := range facts {
		if ConfidenceLevel(f.Confidence) < ConfidenceLevel(confidence) {
			confidence = EffectiveConfidence(f.Confidence)
		}
	}
	return confidence
}

func validateSourcesTx(tx *bolt.Tx, agentID string, sources []Fact, requireUnpinned bool) error {
	for _, source := range sources {
		current, err := factInTx(tx, agentID, source.ID)
		if err != nil {
			return err
		}
		if !sourceMatches(source, current, requireUnpinned) {
			return fmt.Errorf("%w: source %s", ErrFactChanged, source.ID)
		}
	}
	return nil
}
