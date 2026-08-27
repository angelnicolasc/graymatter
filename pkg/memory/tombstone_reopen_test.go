package memory

import (
	"context"
	"testing"
)

// TestTombstoneSurvivesReopen reproduces the agent-lifecycle finding at the
// library level: a fact tombstoned in one store session must still be
// filtered by Recall after the store is closed and reopened — the lifecycle
// simulation observes the tombstone lost across process death.
func TestTombstoneSurvivesReopen(t *testing.T) {
	dir := t.TempDir()

	// session 1: store v1
	s1, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	ctx := context.Background()
	if err := s1.Put(ctx, "a", "The API base URL is https://api.example.com/v1"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close1: %v", err)
	}

	// session 2: supersede it
	s2, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	facts, err := s2.List("a")
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("list2: got %d facts, want 1", len(facts))
	}
	facts[0].SupersededBy = "replacement-id"
	if err := s2.UpdateFact("a", facts[0]); err != nil {
		t.Fatalf("update: %v", err)
	}
	// in-process it must already be filtered
	got, err := s2.Recall(ctx, "a", "api base url", 8)
	if err != nil {
		t.Fatalf("recall2: %v", err)
	}
	for _, g := range got {
		if g == "The API base URL is https://api.example.com/v1" {
			t.Fatalf("in-process recall returned the tombstoned fact: %v", got)
		}
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close2: %v", err)
	}

	// session 3: reopen — the tombstone must have been persisted
	s3, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("open3: %v", err)
	}
	defer s3.Close()
	reopened, err := s3.List("a")
	if err != nil {
		t.Fatalf("list3: %v", err)
	}
	if len(reopened) != 1 || !reopened[0].IsSuperseded() {
		t.Fatalf("after reopen: fact isSuperseded=%v, want true (tombstone must persist)", reopened[0].IsSuperseded())
	}
	got3, err := s3.Recall(ctx, "a", "api base url", 8)
	if err != nil {
		t.Fatalf("recall3: %v", err)
	}
	for _, g := range got3 {
		if g == "The API base URL is https://api.example.com/v1" {
			t.Fatalf("cross-reopen recall returned the tombstoned fact: %v", got3)
		}
	}
	t.Log("tombstone survived the reopen and recall filtered it")
}
