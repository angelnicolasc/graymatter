package memory

import (
	"context"
	"testing"
	"time"
)

// TestNamespaceIsolation_RecallCrossAgent is the minimal core-level
// reproduction of the cross-agent leak observed through the MCP layer: a fact
// stored under a1 must never surface in a2's recall, whatever the signal mix.
func TestNamespaceIsolation_RecallCrossAgent(t *testing.T) {
	cfg := StoreConfig{
		DataDir:  t.TempDir(),
		Embedder: nil, // keyword-only, deterministic
		// No background loops: the assertion targets Recall's own filtering,
		// not a race with consolidation.
		VectorReconcileInterval: time.Hour,
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.Put(ctx, "a1", "precedence probe"); err != nil {
		t.Fatalf("put a1: %v", err)
	}
	if err := s.Put(ctx, "a2", "unrelated filler fact about databases"); err != nil {
		t.Fatalf("put a2: %v", err)
	}

	got, err := s.Recall(ctx, "a2", "precedence probe", 8)
	if err != nil {
		t.Fatalf("recall a2: %v", err)
	}
	for _, g := range got {
		if g == "precedence probe" {
			t.Fatalf("cross-agent leak: a1's fact returned by a2's recall: %v", got)
		}
	}
	t.Logf("a2 recall = %v", got)

	got1, err := s.Recall(ctx, "a1", "precedence probe", 8)
	if err != nil {
		t.Fatalf("recall a1: %v", err)
	}
	if len(got1) == 0 || got1[0] != "precedence probe" {
		t.Fatalf("a1 should recall its own fact first, got %v", got1)
	}
}
