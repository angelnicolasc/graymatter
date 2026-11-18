package memory

import (
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
	return f, nil
}

// patchFact reads fresh state under the write lock. Its callback changes only
// the intended fields, preserving confidence and all unrelated metadata.
// Returning errFactUnchanged without modifying the fact skips persistence and
// returns the current fact successfully after rolling back the transaction.
func (s *Store) patchFact(agentID, id string, patch func(*Fact) error) (Fact, error) {
	fact, _, err := s.patchFactWithReceipt(agentID, id, patch, nil)
	return fact, err
}

func (s *Store) patchFactWithReceipt(agentID, id string, patch func(*Fact) error, update commitUpdate) (Fact, commitReceipt, error) {
	if s.readOnly {
		return Fact{}, notCommitted, ErrStoreReadOnly
	}
	var updated Fact
	receipt, err := s.trackCommit(update, func(tx *bolt.Tx) error {
		current, err := factInTx(tx, agentID, id)
		if err != nil {
			return err
		}
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
		return nil
	})
	if err == errFactUnchanged {
		err = nil
	}
	return updated, receipt, err
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
	_, err := s.retireSnapshotFactWithReceipt(agentID, victim, replacementID, requireUnpinned, requireConfidence, nil)
	return err
}

func (s *Store) retireSnapshotFactWithReceipt(agentID string, victim Fact, replacementID string, requireUnpinned, requireConfidence bool, update commitUpdate) (commitReceipt, error) {
	if victim.AgentID != agentID {
		return notCommitted, fmt.Errorf("retire: fact belongs to another agent")
	}
	_, receipt, err := s.patchFactWithReceipt(agentID, victim.ID, func(current *Fact) error {
		if current.IsSuperseded() || current.Text != victim.Text || current.Kind != victim.Kind || (requireUnpinned && current.Pinned) || (requireConfidence && current.Confidence != victim.Confidence) {
			return fmt.Errorf("%w: cannot retire fact %s", ErrFactChanged, victim.ID)
		}
		current.SupersededBy = replacementID
		return nil
	}, update)
	return receipt, err
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

func (s *Store) decayFact(agentID, id string, now time.Time, lambda float64) (Fact, error) {
	return s.patchFact(agentID, id, func(current *Fact) error {
		if current.Pinned {
			return errFactUnchanged
		}
		hours := now.Sub(current.AccessedAt).Hours()
		current.Weight = math.Min(current.Weight, math.Exp(-lambda*hours))
		return nil
	})
}

func (s *Store) pruneFact(agentID, id string) error {
	if s.readOnly {
		return ErrStoreReadOnly
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		current, err := factInTx(tx, agentID, id)
		if errors.Is(err, ErrFactChanged) {
			return nil // a concurrent delete already collected it
		}
		if err != nil {
			return err
		}
		if current.Pinned || current.Weight >= 0.01 {
			return nil
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
