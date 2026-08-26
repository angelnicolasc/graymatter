package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// RecallAll promises Reciprocal Rank Fusion over the agent and shared
// rankings; it used to concatenate agent-first and truncate, which starved
// the shared namespace whenever the agent list filled its topK. These tests
// pin the fusion, its determinism, and the starvation fix.

func openRecallAllStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      nil,
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRecallAllSharedNamespaceNoLongerStarves(t *testing.T) {
	s := openRecallAllStore(t)
	ctx := context.Background()

	// Eight agent facts matching the query fill the per-namespace topK.
	for i := 0; i < 8; i++ {
		if err := s.Put(ctx, "agent-a", fmt.Sprintf("deploy rollback procedure step %d for release", i)); err != nil {
			t.Fatalf("Put agent: %v", err)
		}
	}
	// One shared fact that matches best of all.
	if err := s.PutShared(ctx, "the deploy rollback runbook lives in ops/wiki"); err != nil {
		t.Fatalf("Put shared: %v", err)
	}

	got, err := s.RecallAll(ctx, "agent-a", "deploy rollback", 8)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}

	found := false
	for _, g := range got {
		if g == "the deploy rollback runbook lives in ops/wiki" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("shared fact starved out of fused results: %v", got)
	}
}

func TestRecallAllDeduplicatesTextAcrossNamespaces(t *testing.T) {
	s := openRecallAllStore(t)
	ctx := context.Background()

	const dup = "billing runs through Polar"
	if err := s.Put(ctx, "agent-a", dup); err != nil {
		t.Fatalf("Put agent: %v", err)
	}
	if err := s.PutShared(ctx, dup); err != nil {
		t.Fatalf("Put shared: %v", err)
	}

	got, err := s.RecallAll(ctx, "agent-a", "billing", 8)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}
	count := 0
	for _, g := range got {
		if g == dup {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicated text appeared %d times, want exactly 1 (both contributions fuse)", count)
	}
	if len(got) == 0 || got[0] != dup {
		t.Errorf("text present in both lists should fuse to rank 1, got %v", got)
	}
}

func TestRecallAllRespectsTopK(t *testing.T) {
	s := openRecallAllStore(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		if err := s.Put(ctx, "agent-a", fmt.Sprintf("agent fact about invoices number %d", i)); err != nil {
			t.Fatalf("Put agent: %v", err)
		}
		if err := s.PutShared(ctx, fmt.Sprintf("shared fact about invoices number %d", i)); err != nil {
			t.Fatalf("Put shared: %v", err)
		}
	}

	got, err := s.RecallAll(ctx, "agent-a", "invoices", 5)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}
	if len(got) > 5 {
		t.Errorf("len = %d, want at most topK=5", len(got))
	}
}

// Pure-function determinism: equal fused scores must resolve to a stable
// total order regardless of map iteration order inside the fusion.
func TestFuseRecallResultsTiebreakIsTotal(t *testing.T) {
	agent := []string{"alpha", "beta"}
	shared := []string{"gamma"}

	first := fuseRecallResults(agent, shared, 8)
	for i := 0; i < 50; i++ {
		next := fuseRecallResults(agent, shared, 8)
		for j := range first {
			if first[j] != next[j] {
				t.Fatalf("order drifted on iteration %d:\nfirst: %v\nnext:  %v", i, first, next)
			}
		}
	}
	if len(first) != 3 {
		t.Fatalf("len = %d, want 3", len(first))
	}
	// Scores: alpha = 1/(60+1), gamma = 1/(60+1) — tied at rank 1 of their
	// lists; beta = 1/(60+2) is strictly lower. The alpha/gamma exact tie
	// resolves to the agent-listed text first, so the total order is
	// [alpha gamma beta].
	if first[0] != "alpha" || first[1] != "gamma" || first[2] != "beta" {
		t.Errorf("order = %v, want [alpha gamma beta]", first)
	}
}

func TestFuseRecallResultsAgentWinsExactTie(t *testing.T) {
	// Same rank in each list (both rank 1): identical fused scores. The
	// agent-scoped text is the more specific of the two and wins the tie.
	agent := []string{"from-agent"}
	shared := []string{"from-shared"}
	got := fuseRecallResults(agent, shared, 8)
	if got[0] != "from-agent" {
		t.Errorf("exact tie resolved to %q, want the agent-scoped text first", got[0])
	}
}
