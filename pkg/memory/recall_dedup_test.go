package memory

import (
	"context"
	"testing"
)

// TestRecall_DeduplicatesIdenticalStoredText pins the documented contract
// ("returns top-K, deduplicated by text"): storing the same sentence twice
// must not yield it twice in one recall result. Discovered during the
// agent-lifecycle simulation, where repeated session templates stored
// identical facts across sessions.
func TestRecall_DeduplicatesIdenticalStoredText(t *testing.T) {
	cfg := StoreConfig{DataDir: t.TempDir()}
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	const fact = "Rate limit on the public API is 100 requests per minute per key"
	for i := 0; i < 3; i++ {
		if err := s.Put(ctx, "dup", fact); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := s.Put(ctx, "dup", "An unrelated stored fact about caching headers"); err != nil {
		t.Fatalf("put filler: %v", err)
	}

	got, err := s.Recall(ctx, "dup", "rate limit public api", 8)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	count := 0
	for _, g := range got {
		if g == fact {
			count++
		}
	}
	if count != 1 {
		t.Errorf("identical fact returned %d times, want exactly 1 (doc: deduplicated by text)\nresults: %v", count, got)
	}
}
