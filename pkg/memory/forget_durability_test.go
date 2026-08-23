package memory

import (
	"context"
	"testing"
	"time"
)

// openForgetStore returns a store on a fixed directory so a test can close and
// reopen it, which is how these tests wait out the background writebacks.
func openForgetStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: dir, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// TestUpdateFact_DoesNotResurrectADeletedFact is the deterministic half of the
// bug CI caught on ubuntu/go1.23.
//
// Recall bumps the access counter of every fact it returns from a detached
// goroutine. bolt's Put does not care whether the key still exists, so a
// writeback that landed after a Delete brought the fact back — the store had
// already told the caller it was gone.
func TestUpdateFact_DoesNotResurrectADeletedFact(t *testing.T) {
	dir := t.TempDir()
	s := openForgetStore(t, dir)
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.Put(ctx, "alice", "The capital of France is Paris."); err != nil {
		t.Fatalf("put: %v", err)
	}
	facts, err := s.List("alice")
	if err != nil || len(facts) != 1 {
		t.Fatalf("List = %v, %v", facts, err)
	}
	stale := facts[0] // what a writeback goroutine would be holding

	if err := s.Delete("alice", stale.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The writeback arrives late, carrying a copy read before the delete.
	stale.AccessCount++
	stale.AccessedAt = time.Now().UTC()
	if err := s.UpdateFact("alice", stale); err != nil {
		t.Fatalf("UpdateFact after delete: %v", err)
	}

	got, err := s.List("alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a late writeback resurrected a deleted fact: %v", got[0].Text)
	}
}

// TestUpdateFact_StillUpdatesLiveFacts guards the other direction: the guard
// must not turn UpdateFact into a no-op for the consolidation and decay paths
// that depend on it.
func TestUpdateFact_StillUpdatesLiveFacts(t *testing.T) {
	dir := t.TempDir()
	s := openForgetStore(t, dir)
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.Put(ctx, "alice", "a fact worth decaying"); err != nil {
		t.Fatalf("put: %v", err)
	}
	facts, err := s.List("alice")
	if err != nil || len(facts) != 1 {
		t.Fatalf("List = %v, %v", facts, err)
	}

	f := facts[0]
	f.Weight = 0.25
	f.AccessCount = 42
	if err := s.UpdateFact("alice", f); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	got, err := s.List("alice")
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v", got, err)
	}
	if got[0].Weight != 0.25 || got[0].AccessCount != 42 {
		t.Errorf("update did not land: weight=%v accessCount=%d", got[0].Weight, got[0].AccessCount)
	}
}

// TestForget_SurvivesTheRecallWriteback drives the real race rather than
// simulating it: recall (which spawns the writeback), then delete, then close
// the store — Close waits on the goroutines — and reopen to see what persisted.
//
// Before the fix this failed most of the time; it is what turned a green local
// run into a red ubuntu/go1.23 job.
func TestForget_SurvivesTheRecallWriteback(t *testing.T) {
	dir := t.TempDir()
	s := openForgetStore(t, dir)

	ctx := context.Background()
	const text = "The capital of France is Paris."
	if err := s.Put(ctx, "alice", text); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Several recalls, so several writebacks are in flight for the same fact.
	for i := 0; i < 8; i++ {
		if _, err := s.Recall(ctx, "alice", "France", 1); err != nil {
			t.Fatalf("recall: %v", err)
		}
	}

	facts, err := s.List("alice")
	if err != nil || len(facts) != 1 {
		t.Fatalf("List = %v, %v", facts, err)
	}
	if err := s.Delete("alice", facts[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Close drains the writeback goroutines, so whatever is on disk after this
	// is the final answer rather than a race we happened to win.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openForgetStore(t, dir)
	t.Cleanup(func() { _ = reopened.Close() })

	got, err := reopened.List("alice")
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("forget did not stick: %d fact(s) survived, first = %q", len(got), got[0].Text)
	}
}
