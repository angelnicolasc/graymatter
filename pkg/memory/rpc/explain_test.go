package rpc

import (
	"context"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// TestRoundTrip_RecallExplain proves the receipts survive the wire: the
// client calls RecallExplain through the real JSON codec against a real
// store, and every receipt field that the explain surface promises — text,
// ranks, fused score, k, weight, provenance — arrives intact and consistent
// with what the same store returns over the plain Recall method.
func TestRoundTrip_RecallExplain(t *testing.T) {
	dataDir, _ := startServer(t, mustToken(t))
	c := dialT(t, dataDir)
	ctx := context.Background()

	for _, text := range []string{
		"deployments are signed with the team gpg key before publishing",
		"the staging cluster restarts every night",
		"release notes are drafted from merged pull requests",
	} {
		if err := c.Put(ctx, "agent-a", text); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	recalled, err := c.Recall(ctx, "agent-a", "gpg signing deployments", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	receipts, err := c.RecallExplain(ctx, "agent-a", "gpg signing deployments", 5)
	if err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}

	if len(receipts) != len(recalled) {
		t.Fatalf("RecallExplain returned %d receipts, Recall returned %d facts", len(receipts), len(recalled))
	}
	for i, r := range receipts {
		if r.Text != recalled[i] {
			t.Errorf("receipt %d text = %q, want Recall's %q", i, r.Text, recalled[i])
		}
		if r.Ranks.K != 60 {
			t.Errorf("receipt %d: k = %v, want 60", i, r.Ranks.K)
		}
		if r.Ranks.RecencyRank <= 0 {
			t.Errorf("receipt %d: recency rank %d, want >= 1", i, r.Ranks.RecencyRank)
		}
		if r.Provenance.FactID == "" {
			t.Errorf("receipt %d: empty fact_id over the wire", i)
		}
		if r.Provenance.WrittenAt.IsZero() {
			t.Errorf("receipt %d: zero written_at over the wire", i)
		}
	}

	// The fact_id must resolve against the store's own List — receipts cite
	// real facts, not reconstructed ones.
	facts, err := c.List("agent-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := make(map[string]memory.Fact, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	for _, r := range receipts {
		f, ok := byID[r.Provenance.FactID]
		if !ok {
			t.Errorf("receipt fact_id %q not present in the store", r.Provenance.FactID)
			continue
		}
		if f.Weight != r.Weight {
			t.Errorf("receipt %q weight %v != stored %v", r.Provenance.FactID, r.Weight, f.Weight)
		}
	}
}
