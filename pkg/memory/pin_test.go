package memory

// W1 of the hardening playbook: pinned facts (ADR-010). Invariant I-1: a
// pinned fact is exempt from decay (consolidation step 1), summarisation
// (step 2) and pruning (step 3). Consolidation must not collect what the
// user declared permanent — and pinned facts are precisely the rarely-
// accessed ones the weight sort would surface first.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// pinTestStore returns a store whose clock the test advances manually, so
// decay behaves deterministically without sleeping.
func pinTestStore(t *testing.T) (*Store, func(time.Duration)) {
	t.Helper()
	s, cleanup := openTestStore(t)
	t.Cleanup(cleanup)
	current := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return current }
	return s, func(d time.Duration) { current = current.Add(d) }
}

func mustPin(t *testing.T, s *Store, agentID string, f Fact, at time.Time) {
	t.Helper()
	f.Pinned = true
	f.PinnedAt = at
	if err := s.UpdateFact(agentID, f); err != nil {
		t.Fatalf("pin fact: %v", err)
	}
}

// A pinned fact must survive consolidation cycles that prune every unpinned
// neighbour, byte-identical, across arbitrarily many cycles.
func TestPinnedSurvivesConsolidationCycles(t *testing.T) {
	s, advance := pinTestStore(t)
	ctx := context.Background()

	var pinnedJSON []byte
	var pinnedID string
	for i := 0; i < 5; i++ {
		_ = s.Put(ctx, "dormant", fmt.Sprintf("ephemeral observation %d", i))
	}
	_ = s.Put(ctx, "dormant", "ARCHITECTURE DECISION: the write path is single-writer by design.")
	facts, _ := s.List("dormant")
	for _, f := range facts {
		if f.Text == "ARCHITECTURE DECISION: the write path is single-writer by design." {
			mustPin(t, s, "dormant", f, f.CreatedAt)
		}
	}
	// Snapshot AFTER pinning, re-listed from the store: the baseline the
	// cycles must not mutate.
	facts, _ = s.List("dormant")
	for _, f := range facts {
		if f.Pinned {
			pinnedID = f.ID
			pinnedJSON, _ = f.marshal()
		}
	}
	if pinnedID == "" {
		t.Fatal("pinned fact not found after pinning")
	}

	cfg := &testConsolidateCfg{threshold: 100, halfLife: 24 * time.Hour}
	for cycle := 0; cycle < 60; cycle++ {
		advance(48 * time.Hour) // 120 simulated days: every unpinned fact decays past 0.01
		if err := s.Consolidate(ctx, "dormant", cfg); err != nil {
			t.Fatalf("consolidate cycle %d: %v", cycle, err)
		}
	}

	remaining, err := s.List("dormant")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected only the pinned fact to survive 120 simulated days, got %d facts: %v", len(remaining), remaining)
	}
	got := remaining[0]
	if got.ID != pinnedID {
		t.Errorf("survivor is %q, want the pinned fact %q", got.ID, pinnedID)
	}
	if !got.Pinned {
		t.Error("surviving fact lost its pinned flag")
	}
	if got.Weight != 1.0 {
		t.Errorf("pinned weight drifted: %v, want 1.0", got.Weight)
	}
	afterJSON, _ := got.marshal()
	if string(pinnedJSON) != string(afterJSON) {
		t.Errorf("pinned fact mutated across cycles:\n before: %s\n after:  %s", pinnedJSON, afterJSON)
	}
}

// The summariser consumes the weakest half of the unpinned facts; a pinned
// fact is never eligible, whatever its weight.
func TestSummarisationBatchExcludesPinned(t *testing.T) {
	facts := []Fact{
		{ID: "pinned-heavy", Text: "pinned", Weight: 0.9, Pinned: true},
		{ID: "weak-1", Text: "w1", Weight: 0.1},
		{ID: "weak-2", Text: "w2", Weight: 0.2},
		{ID: "strong", Text: "s", Weight: 0.8},
	}
	batch := summarisationBatch(facts)
	// Weakest half of the 3 unpinned facts = 1 fact (floor semantics, same
	// as the pre-pin implementation's len/2).
	if len(batch) != 1 {
		t.Fatalf("expected weakest half of unpinned (1), got %d: %v", len(batch), batch)
	}
	for _, f := range batch {
		if f.Pinned {
			t.Errorf("pinned fact %q in summarisation batch", f.ID)
		}
		if f.ID != "weak-1" {
			t.Errorf("unexpected batch member %q (weight %.2f)", f.ID, f.Weight)
		}
	}

	allPinned := []Fact{{ID: "a", Pinned: true}, {ID: "b", Pinned: true}}
	if batch := summarisationBatch(allPinned); batch != nil {
		t.Errorf("all-pinned store must yield an empty batch, got %v", batch)
	}
}

// Unpinning restores normal decay. While pinned, elapsed time does not move
// the weight; once unpinned, the fact decays again from its current weight.
// (Decay recomputes from last access, so a fact pinned for a year then
// unpinned legitimately inherits that staleness — the test keeps elapsed
// time short to isolate the resume behaviour.)
func TestUnpinRestoresDecay(t *testing.T) {
	s, advance := pinTestStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "u", "A decision that was permanent, then revised to be ephemeral.")
	facts, _ := s.List("u")
	f := facts[0]
	mustPin(t, s, "u", f, f.CreatedAt)

	cfg := &testConsolidateCfg{threshold: 100, halfLife: 24 * time.Hour}
	for i := 0; i < 2; i++ {
		advance(1 * time.Hour)
		_ = s.Consolidate(ctx, "u", cfg)
	}
	frozen, _ := s.List("u")
	if frozen[0].Weight != 1.0 {
		t.Fatalf("pinned weight moved while pinned: %v", frozen[0].Weight)
	}

	frozen[0].Pinned = false
	frozen[0].PinnedAt = time.Time{}
	if err := s.UpdateFact("u", frozen[0]); err != nil {
		t.Fatal(err)
	}
	// One half-life of elapsed time since creation: weight must land near
	// 0.5 — decaying again, far from the prune floor.
	advance(24 * time.Hour)
	_ = s.Consolidate(ctx, "u", cfg)

	after, _ := s.List("u")
	if len(after) != 1 {
		t.Fatalf("unpinned fact pruned after a single half-life: %+v", after)
	}
	if after[0].Weight >= 1.0 || after[0].Weight < 0.4 {
		t.Errorf("unpinned fact did not resume decay sensibly: weight %v", after[0].Weight)
	}
}

// The pin survives close/reopen: it is store state, not process state.
func TestPinPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s1, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Put(ctx, "persist", "Security policy: API keys never live in config files.")
	facts, _ := s1.List("persist")
	mustPin(t, s1, "persist", facts[0], facts[0].CreatedAt)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	after, _ := s2.List("persist")
	if len(after) != 1 || !after[0].Pinned {
		t.Errorf("pin lost across reopen: %+v", after)
	}
}
