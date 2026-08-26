package mcp

import (
	"context"
	"fmt"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
)

// ADR-007 keeps tombstones so an audit can see what was retired and why.
// supersedeFact used to zero the retired fact's weight as well, which drops
// it below the prune floor (<0.01): the next consolidation cycle collected
// the receipt milliseconds after writing it. This pins the corrected
// contract — a forget receipt survives a consolidation cycle that runs
// immediately after the forget.

func newTombstoneTestServer(t *testing.T) (*Server, *graymatter.Memory) {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.AsyncConsolidate = false // deterministic: cycles only when asked
	cfg.ConsolidateLLM = ""      // decay+prune-only path; no network, no key
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return New(NewDirectBackend(mem, nil), "test"), mem
}

func TestForgetReceiptSurvivesConsolidationCycle(t *testing.T) {
	s, mem := newTombstoneTestServer(t)
	ctx := context.Background()

	const victim = "the fact the agent decided to forget"
	if err := mem.Remember(ctx, "a1", victim); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := mem.Remember(ctx, "a1", fmt.Sprintf("filler observation %d", i)); err != nil {
			t.Fatalf("Remember filler: %v", err)
		}
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "a1",
		"text":   victim,
	}))
	if err != nil || res.IsError {
		t.Fatalf("forget: %v / %s", err, resultText(t, res))
	}

	cfg := graymatter.DefaultConfig()
	cfg.ConsolidateLLM = ""
	if err := mem.Advanced().Consolidate(ctx, "a1", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	facts, err := mem.Advanced().List("a1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range facts {
		if f.Text != victim {
			continue
		}
		if !f.IsSuperseded() {
			t.Fatalf("receipt is listed but not tombstoned: %+v", f)
		}
		if f.Weight <= 0.01 {
			t.Errorf("receipt weight = %v at or under prune floor; the next cycle would destroy it", f.Weight)
		}
		return
	}
	t.Fatal("forget receipt was destroyed by the very next consolidation cycle - ADR-007 violated")
}

// The same invariant on the update path: the retired version must remain
// auditable after consolidation, still pointing at its replacement.
func TestUpdateReceiptSurvivesConsolidationCycle(t *testing.T) {
	s, mem := newTombstoneTestServer(t)
	ctx := context.Background()

	const oldFact = "billing runs through Stripe"
	if err := mem.Remember(ctx, "a1", oldFact); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update",
		"agent":  "a1",
		"target": oldFact,
		"text":   "billing runs through Polar",
	}))
	if err != nil || res.IsError {
		t.Fatalf("update: %v / %s", err, resultText(t, res))
	}

	cfg := graymatter.DefaultConfig()
	cfg.ConsolidateLLM = ""
	if err := mem.Advanced().Consolidate(ctx, "a1", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	facts, _ := mem.Advanced().List("a1")
	for _, f := range facts {
		if f.Text == oldFact {
			if !f.IsSuperseded() || f.Weight <= 0.01 {
				t.Fatalf("update receipt degraded: superseded=%v weight=%v", f.IsSuperseded(), f.Weight)
			}
			return
		}
	}
	t.Fatal("update receipt was destroyed by the next consolidation cycle - ADR-007 violated")
}
