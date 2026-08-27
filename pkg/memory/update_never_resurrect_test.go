package memory

import (
	"context"
	"testing"
)

// TestUpdateFact_NeverResurrectsTombstone reproduces the agent-lifecycle
// finding deterministically. The race: a consolidation cycle reads its batch
// (fact live), the agent supersedes the fact mid-cycle, the cycle's decay
// pass then writes the stale snapshot back — clearing SupersededBy and
// bringing the dead fact back. The write must drop stale snapshots instead.
func TestUpdateFact_NeverResurrectsTombstone(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.Put(ctx, "a", "The API base URL is https://api.example.com/v1"); err != nil {
		t.Fatalf("put: %v", err)
	}

	// the consolidation cycle's snapshot: fact live
	batch, err := s.List("a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	stale := batch[0]

	// the agent supersedes the fact while the cycle is in flight
	supersede := stale
	supersede.SupersededBy = "replacement-id"
	if err := s.UpdateFact("a", supersede); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	// the cycle's decay writeback: the pre-tombstone snapshot, weight decayed,
	// access bumped — tombstone must survive the write
	stale.AccessCount = 99
	stale.Weight = 0.4
	if err := s.UpdateFact("a", stale); err != nil {
		t.Fatalf("stale decay write: %v", err)
	}

	got, err := s.List("a")
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	t.Logf("after stale writeback: superseded_by=%q access_count=%d", got[0].SupersededBy, got[0].AccessCount)
	if len(got) != 1 || !got[0].IsSuperseded() {
		t.Fatalf("tombstone was resurrected by the stale writeback: superseded=%v\n%+v", got[0].IsSuperseded(), got[0])
	}
	if got[0].AccessCount == 99 {
		t.Error("the stale snapshot's access metadata leaked through; the whole write should have been dropped")
	}
}

// TestTouchFacts_NeverResurrectsTombstone pins the same guard on the recall
// access-tracking writeback: Recall filters tombstones before returning, but
// a consolidation cycle can supersede a returned fact between the filter and
// the batched touch — the touch must not clear it.
func TestTouchFacts_NeverResurrectsTombstone(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.Put(ctx, "a", "fact that will be superseded mid-recall"); err != nil {
		t.Fatalf("put: %v", err)
	}

	batch, err := s.List("a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	stale := batch[0]
	stale.AccessCount = 7

	stale.SupersededBy = "gone"
	if err := s.UpdateFact("a", stale); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	// recall's writeback: the stale pre-tombstone snapshot, access bumped
	stale.SupersededBy = ""
	s.touchFacts([]Fact{stale})

	got, err := s.List("a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || !got[0].IsSuperseded() {
		t.Fatalf("touchFacts resurrected the tombstoned fact: superseded=%v", got[0].IsSuperseded())
	}
}
