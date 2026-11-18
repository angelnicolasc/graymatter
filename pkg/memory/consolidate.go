package memory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Deprecated: Automatic generative compaction is disabled. This sentinel is
// retained for callers that used it with older versions.
var ErrConsolidateLLMUnsupported = errors.New(
	"consolidation LLM \"ollama\" is implemented since v0.14.0; this sentinel " +
		"is retained only for API compatibility and is never returned")

// Deprecated: Consolidate no longer requests or applies generative proposals.
var ErrInvalidProposal = errors.New("invalid consolidation proposal")

// Deprecated: Consolidate no longer commits generative summaries or retires
// their sources. This sentinel is retained for API compatibility.
var ErrConsolidationRetirement = errors.New("consolidation summary committed but source retirement failed")

// ConsolidateConfig supplies the threshold and decay policy for maintenance.
// Provider getters are retained for compatibility with existing implementations.
type ConsolidateConfig interface {
	GetAnthropicAPIKey() string
	GetConsolidateLLM() string
	GetConsolidateModel() string
	GetConsolidateThreshold() int
	GetDecayHalfLife() time.Duration
	GetOllamaURL() string
	GetOllamaConsolidateModel() string
}

// LaunchAsyncConsolidate starts MaybeConsolidate in a tracked, bounded goroutine.
// Non-blocking: if the semaphore is at capacity the trigger is silently dropped
// rather than blocking the caller.
func (s *Store) LaunchAsyncConsolidate(agentID string, cfg ConsolidateConfig) {
	select {
	case s.sema <- struct{}{}: // acquired slot
	default:
		return // at capacity; skip this consolidation cycle
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.sema }()
		// Close drains accepted background maintenance before releasing the store.
		if err := s.MaybeConsolidate(context.WithoutCancel(s.shutdownCtx), agentID, cfg); err != nil {
			if s.cfg.OnConsolidateError != nil {
				s.cfg.OnConsolidateError(agentID, err)
			}
		}
	}()
}

// MaybeConsolidate runs maintenance when the namespace meets its fact-count
// threshold. Safe to call concurrently.
func (s *Store) MaybeConsolidate(ctx context.Context, agentID string, cfg ConsolidateConfig) error {
	facts, err := s.List(agentID)
	if err != nil {
		return err
	}
	if len(facts) < cfg.GetConsolidateThreshold() {
		return nil
	}
	return s.Consolidate(ctx, agentID, cfg)
}

// Consolidate runs decay, pruning, and configured knowledge-graph maintenance.
// It does not request summaries, publish generative facts, or retire sources.
func (s *Store) Consolidate(ctx context.Context, agentID string, cfg ConsolidateConfig) error {
	if s.readOnly {
		return ErrStoreReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	nowT := s.now().UTC()
	migrationErr := s.db.Update(func(tx *bolt.Tx) error {
		var added int64
		if err := captureLegacyLineageTx(tx, agentID, nowT, nil, &added); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if added == 0 {
			return errFactUnchanged
		}
		return checkEvidenceQuotaTx(tx, agentID, added)
	})
	if migrationErr == errFactUnchanged {
		migrationErr = nil
	}
	if migrationErr != nil && !errors.Is(migrationErr, ErrEvidenceQuota) {
		return migrationErr
	}
	s.mu.RLock()
	extractor, graph, maintenance := s.extractor, s.graph, s.graphMaintenance
	s.mu.RUnlock()
	provenance, _ := graph.(GraphProvenanceWriter)
	if migrator, ok := maintenance.(graphProvenanceMigrator); ok {
		// Inventory legacy aliases before canonical pruning can remove them.
		if err := migrator.MigrateGraphProvenance(); err != nil {
			return fmt.Errorf("inventory graph provenance: %w", err)
		}
	}

	facts, err := s.List(agentID)
	if err != nil {
		return err
	}
	if len(facts) == 0 && graph == nil && maintenance == nil {
		return errors.Join(migrationErr, s.sweepLineage(agentID, nowT))
	}

	halfLife := cfg.GetDecayHalfLife()
	if halfLife == 0 {
		halfLife = 720 * time.Hour
	}
	lambda := math.Log(2) / halfLife.Hours()

	// Step 1: commit bounded decay batches in order. A failed batch rolls back.
	//
	// Weight is recomputed from staleness, not multiplied into. Multiplying
	// re-applied the entire elapsed period on every run — nothing recorded
	// that a fact had already been decayed — so weight halved once per
	// consolidation cycle rather than once per half-life. Five cycles in the
	// same millisecond took a one-half-life-stale fact from 0.5 to 0.03, and
	// with AsyncConsolidate on, a busy agent could prune a month-old fact in
	// minutes. The half-life was per run, not per 30 days.
	//
	// min() rather than plain assignment, for two reasons: decay must never
	// hand weight back, and a fact whose weight was deliberately zeroed —
	// a supersede tombstone (ADR-007) — must stay collectable by pruning
	// instead of being resurrected by its own recent access time.
	for batch := range slices.Chunk(facts, maintenanceDecayBatchSize) {
		if err := s.decayBatch(ctx, agentID, batch, nowT, lambda); err != nil {
			return errors.Join(migrationErr, err)
		}
	}

	// Step 2: prune low-weight facts, including retired rows, when aging applies.
	facts, err = s.List(agentID)
	if err != nil {
		return err
	}
	var maintenanceErrs []error
	for _, f := range facts {
		if err := ctx.Err(); err != nil {
			return errors.Join(migrationErr, errors.Join(maintenanceErrs...), err)
		}
		if !pruneEligible(f, nowT, lambda) {
			continue
		}
		if err := s.pruneFact(agentID, f.ID, nowT, lambda); err != nil {
			maintenanceErrs = append(maintenanceErrs, fmt.Errorf("prune fact %s: %w", f.ID, err))
		}
	}

	if err := ctx.Err(); err != nil {
		return errors.Join(migrationErr, err)
	}
	if err := s.sweepLineage(agentID, nowT); err != nil {
		return err
	}

	// Step 3: run entity extraction on surviving facts and upsert into graph.
	//
	// A text-signature watermark (A7) makes the pass incremental: a fact is
	// extracted only when its text changed since the last successful pass.
	// Extraction is idempotent, so the old re-extract-everything behaviour was
	// correct but O(facts) per cycle for no information.
	//
	// Two paths, chosen by capability:
	//   - TypedEntityExtractor + EdgeWriter: nodes keep the extractor's label
	//     and type, and co-mentioned pairs become edges. This is what makes
	//     recall enrichment traversable (issue #24).
	//   - Legacy: ID-only upserts, no edges — preserved verbatim so older
	//     extractor implementations behave exactly as before.
	if extractor != nil && graph != nil {
		typed, typedOK := extractor.(TypedEntityExtractor)
		writer, writeOK := graph.(EdgeWriter)
		var target string
		if typedOK && provenance != nil {
			target, err = s.graphTarget(provenance)
			if err != nil {
				maintenanceErrs = append(maintenanceErrs, fmt.Errorf("identify graph for extraction: %w", err))
			}
		}

		facts, err = s.List(agentID)
		if err != nil {
			return errors.Join(append(maintenanceErrs, err)...)
		}
		for _, f := range facts {
			if err := ctx.Err(); err != nil {
				return errors.Join(migrationErr, errors.Join(maintenanceErrs...), err)
			}
			if !f.Extractable() {
				continue
			}
			if typedOK && provenance != nil {
				if target != "" {
					if err := s.extractGraphRevision(ctx, typed, provenance, f, target); err != nil && !errors.Is(err, ErrFactChanged) {
						maintenanceErrs = append(maintenanceErrs, fmt.Errorf("extract graph revision %s: %w", f.ID, err))
					}
				}
				continue
			}
			sig := textSignature(f.Text)
			if prev, ok := s.extractedSignature(agentID, f.ID); ok && prev == sig {
				continue
			}
			clean := true
			if typedOK && writeOK {
				refs, links, extractErr := typed.ExtractTyped(f.Text)
				if extractErr != nil {
					continue
				}
				for _, ref := range refs {
					if upsertErr := graph.UpsertNode(ref.ID, ref.Label, ref.EntityType); upsertErr != nil {
						clean = false
						if s.cfg.OnConsolidateError != nil {
							s.cfg.OnConsolidateError(agentID, fmt.Errorf("upsert node %s: %w", ref.ID, upsertErr))
						}
					}
				}
				for i := 0; i < len(links); i++ {
					if linkErr := writer.LinkEdges([]EntityLink{links[i]}, f.ID); linkErr != nil {
						clean = false
						if s.cfg.OnConsolidateError != nil {
							s.cfg.OnConsolidateError(agentID, fmt.Errorf("link edge: %w", linkErr))
						}
					}
				}
				if clean {
					_ = s.markExtracted(agentID, f.ID, sig)
				}
				continue
			}

			ids, extractErr := extractor.ExtractIDs(f.Text)
			if extractErr != nil {
				continue
			}
			for _, id := range ids {
				if upsertErr := graph.UpsertNode(id, id, "concept"); upsertErr != nil {
					clean = false
					if s.cfg.OnConsolidateError != nil {
						s.cfg.OnConsolidateError(agentID, fmt.Errorf("upsert node %s: %w", id, upsertErr))
					}
				}
			}
			if clean {
				_ = s.markExtracted(agentID, f.ID, sig)
			}
		}
	}

	// Step 4: decay graph weights. Optional capability - existing
	// GraphAccessor implementations keep compiling unchanged.
	//
	// The graph mirrors recallable memory; without this pass its weights
	// only ever grew (upsert takes max), so a hub entity from a finished
	// project stayed prominent forever. Same recompute-from-staleness rule
	// as Step 1, for the same reason: multiplying re-applied whole elapsed
	// periods per cycle and compounded forgetting.
	if writer, ok := maintenance.(GraphProvenanceWriter); ok {
		if err := s.drainGraphCleanup(writer); err != nil {
			maintenanceErrs = append(maintenanceErrs, fmt.Errorf("deliver graph cleanup: %w", err))
		}
	}
	if decayer, ok := maintenance.(GraphDecayer); ok {
		if err := decayer.DecayGraph(halfLife); err != nil {
			if s.cfg.OnConsolidateError != nil {
				s.cfg.OnConsolidateError(agentID, fmt.Errorf("graph decay: %w", err))
			}
		}
	}

	return errors.Join(append(maintenanceErrs, migrationErr)...)
}

// GraphDecayer is an optional GraphAccessor capability: lifecycle-driven
// weight decay over graph state, invoked once per consolidation cycle.
// Implementations must be idempotent - recomputing from staleness rather
// than multiplying into weight - so running more often never forgets faster.
type GraphDecayer interface {
	DecayGraph(halfLife time.Duration) error
}

// textSignature fingerprints a fact's text for the extraction watermark.
func textSignature(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum)
}

// extractedSignature returns the text signature recorded the last time this
// fact was successfully extracted into the knowledge graph.
func (s *Store) extractedSignature(agentID, factID string) (string, bool) {
	var sig string
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKGExtracted)
		if b == nil {
			return nil // read-only store against an old DB: treat as unextracted
		}
		if v := b.Get([]byte(agentID + "\x00" + factID)); v != nil {
			sig, found = string(v), true
		}
		return nil
	})
	return sig, found
}

// markExtracted records the text signature of a fact whose extraction fully
// succeeded. No-op on read-only stores (consolidation cannot run there
// anyway, and a failed write must not fail the pass).
func (s *Store) markExtracted(agentID, factID, sig string) error {
	if s.readOnly {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKGExtracted)
		if b == nil {
			return fmt.Errorf("kg_extracted bucket missing")
		}
		return b.Put([]byte(agentID+"\x00"+factID), []byte(sig))
	})
}

const (
	metaKeyConsolidations    = "consolidations"
	metaKeyFactsConsolidated = "facts_consolidated"
)

// ReadConsolidationCounters reports historical summary cycles and source counts.
// Maintenance leaves these persisted counters unchanged.
func ReadConsolidationCounters(db *bolt.DB) (cycles, facts int) {
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(metaKeyConsolidations)); v != nil {
			cycles, _ = strconv.Atoi(string(v))
		}
		if v := b.Get([]byte(metaKeyFactsConsolidated)); v != nil {
			facts, _ = strconv.Atoi(string(v))
		}
		return nil
	})
	return cycles, facts
}

// ConsolidationCounters reads the counters from this store.
func (s *Store) ConsolidationCounters() (cycles, facts int) {
	return ReadConsolidationCounters(s.db)
}
