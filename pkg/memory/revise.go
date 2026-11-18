package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// AtomicRevisionSupported reports that this store commits explicit revisions
// and their source retirements in one transaction.
func (*Store) AtomicRevisionSupported() bool { return true }

// Revise writes newText and retires all selected facts atomically. Each
// source retains a tombstone that points to the committed replacement.
// The returned ID identifies that replacement.
func (s *Store) Revise(ctx context.Context, agentID, newText string, victims ...Fact) (string, error) {
	f, err := s.ReviseFactsWithOptions(ctx, agentID, newText, WriteOptions{}, victims...)
	return f.ID, err
}

// ReviseWithOptions revises every live exact-text match captured by this call.
// The replacement's confidence is explicit or conservatively derived from the
// validated sources. The replacement never inherits a pin.
func (s *Store) ReviseWithOptions(ctx context.Context, agentID, target, newText string, options WriteOptions) (Fact, error) {
	if err := options.Validate(); err != nil {
		return Fact{}, err
	}
	facts, err := s.List(agentID)
	if err != nil {
		return Fact{}, err
	}
	var victims []Fact
	for _, f := range facts {
		if f.Text == target && !f.IsSuperseded() {
			victims = append(victims, f)
		}
	}
	if len(victims) == 0 {
		return Fact{}, fmt.Errorf("revise: no live target matches %q", target)
	}
	return s.ReviseFactsWithOptions(ctx, agentID, newText, options, victims...)
}

// ReviseFactsWithOptions revises the exact selected sources. The replacement,
// source tombstones and persisted indexes commit together. A failed revision
// returns no replacement. Once committed, the receipt is returned even if the
// caller cancels while post-commit work runs.
func (s *Store) ReviseFactsWithOptions(ctx context.Context, agentID, newText string, options WriteOptions, victims ...Fact) (Fact, error) {
	if err := options.Validate(); err != nil {
		return Fact{}, err
	}
	if strings.TrimSpace(newText) == "" {
		return Fact{}, fmt.Errorf("revise: the replacement text is required")
	}
	if len(victims) == 0 {
		return Fact{}, fmt.Errorf("revise: at least one live source is required")
	}
	if err := ctx.Err(); err != nil {
		return Fact{}, err
	}
	if s.readOnly {
		return Fact{}, ErrStoreReadOnly
	}
	for _, v := range victims {
		if v.AgentID != agentID || v.IsSuperseded() {
			return Fact{}, fmt.Errorf("revise: %q is invalid or already superseded", v.Text)
		}
	}
	sources := make([]Fact, 0, len(victims))
	seen := make(map[string]bool, len(victims))
	if err := s.db.View(func(tx *bolt.Tx) error {
		for _, v := range victims {
			if seen[v.ID] {
				continue
			}
			seen[v.ID] = true
			current, err := factInTx(tx, agentID, v.ID)
			if err != nil {
				return err
			}
			if !sourceMatches(v, current, false) {
				return fmt.Errorf("%w: source %s", ErrFactChanged, v.ID)
			}
			if len(sources) > 0 && current.Kind != sources[0].Kind {
				return fmt.Errorf("revise: all sources must have the same kind")
			}
			sources = append(sources, current)
		}
		return nil
	}); err != nil {
		return Fact{}, err
	}

	return s.reviseSources(ctx, agentID, newText, options, sources)
}

// reviseSources is shared by explicit multi-source revisions and ID-selected
// curation. The caller chooses the sources before embedding. Re-reading them
// here preserves access and decay updates without accepting changed content,
// kind, confidence, pin state or authorship.
func (s *Store) reviseSources(ctx context.Context, agentID, text string, options WriteOptions, sources []Fact) (Fact, error) {
	replacement, err := s.putReturningFactPrepared(ctx, agentID, text, sources[0].Kind, "", options, func(tx *bolt.Tx, replacement *Fact) error {
		if options.Confidence == nil {
			replacement.Confidence = derivedConfidence(sources)
		}
		var added int64
		var unknown []string
		for _, source := range sources {
			if lineageHeadKey(tx, agentID, source.ID) == "" {
				unknown = append(unknown, source.ID)
			}
		}
		if len(unknown) != 0 {
			if err := captureLegacyLineageTx(tx, agentID, s.now().UTC(), unknown, &added); err != nil {
				return err
			}
		}
		validated := make([]Fact, 0, len(sources))
		for _, source := range sources {
			current, err := factInTx(tx, agentID, source.ID)
			if err != nil {
				return err
			}
			if !sourceVersionMatches(source, current) {
				return fmt.Errorf("%w: source %s", ErrFactChanged, source.ID)
			}
			validated = append(validated, current)
		}
		if err := recordRevisionLineageTx(tx, replacement, validated, &added); err != nil {
			return err
		}
		if err := checkEvidenceQuotaTx(tx, agentID, added); err != nil {
			return err
		}
		b := tx.Bucket(bucketFacts).Bucket([]byte(agentID))
		droppedHistoricalRoot := false
		for _, current := range validated {
			droppedHistoricalRoot = droppedHistoricalRoot || current.LineageStatus == LineageContentChanged
			current.SupersededBy = replacement.ID
			if err := s.persistFactTx(tx, b, agentID, current); err != nil {
				return fmt.Errorf("retire source %s: %w", current.ID, err)
			}
		}
		if droppedHistoricalRoot {
			// The replacement row is inserted by the caller later in this
			// transaction, so its exact version is already a staged root.
			if err := collectLineageTx(tx, agentID, s.now().UTC(), NewEvidenceSnapshot(*replacement).key()); err != nil {
				return err
			}
		}
		return s.noteRevisionVectorTx(tx, agentID)
	})
	if err != nil {
		return Fact{}, fmt.Errorf("revise: %w", err)
	}
	return replacement, nil
}

// Retire tombstones facts that have nothing to replace them. Recall stops
// returning them at once; the facts themselves stay in the store with a
// receipt recording that an agent dropped them.
func (s *Store) Retire(agentID string, victims ...Fact) error {
	for _, v := range victims {
		if v.AgentID != agentID || v.IsSuperseded() {
			return fmt.Errorf("retire: %q is already superseded", v.Text)
		}
	}
	var errs []error
	seen := make(map[string]bool, len(victims))
	for _, v := range victims {
		if seen[v.ID] {
			continue
		}
		seen[v.ID] = true
		if err := s.retireFact(agentID, v, SupersededByAgent, false); err != nil {
			errs = append(errs, fmt.Errorf("retire %q: %w", v.Text, err))
		}
	}
	return errors.Join(errs...)
}
