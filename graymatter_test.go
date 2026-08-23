package graymatter

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// TestNew_HealthyOnSuccess verifies that a successful New() returns a Memory
// reporting Healthy()==true with operational status.
func TestNew_HealthyOnSuccess(t *testing.T) {
	dir := t.TempDir()
	mem := New(filepath.Join(dir, "mem"))
	defer mem.Close()

	if !mem.Healthy() {
		t.Fatalf("Healthy() = false on a successful open")
	}
	st := mem.Status()
	if !st.Healthy || st.Mode != "operational" {
		t.Fatalf("Status: got %+v, want healthy/operational", st)
	}
	if st.InitError != nil {
		t.Fatalf("Status.InitError = %v on healthy mem", st.InitError)
	}
}

// TestNew_NoOpModeIsObservable verifies that when New() falls back to no-op
// mode, callers can detect it via Healthy() and Status() rather than silently
// running with a broken store. This is the central guarantee added to address
// the "no-op mode is undetectable" audit finding.
func TestNew_NoOpModeIsObservable(t *testing.T) {
	// Force a failure by passing a path that contains a NUL byte. os.MkdirAll
	// rejects such paths on every supported platform.
	mem := New("invalid\x00path")
	defer mem.Close()

	if mem.Healthy() {
		t.Fatalf("Healthy() = true on a forced init failure")
	}
	st := mem.Status()
	if st.Healthy || st.Mode != "noop" {
		t.Fatalf("Status: got %+v, want unhealthy/noop", st)
	}
	if st.InitError == nil {
		t.Fatalf("Status.InitError is nil on a forced init failure — caller cannot diagnose")
	}

	// All operations must remain panic-free on a no-op handle.
	ctx := context.Background()
	if err := mem.Remember(ctx, "agent", "x"); err != nil {
		t.Fatalf("Remember on no-op: %v", err)
	}
	facts, err := mem.Recall(ctx, "agent", "x")
	if err != nil {
		t.Fatalf("Recall on no-op: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("Recall on no-op returned %d facts, want 0", len(facts))
	}
	if mem.Advanced() != nil {
		t.Fatalf("Advanced() on no-op should return nil")
	}
}

// TestDefaultConfig_ConsolidateThreshold verifies the default is low enough
// for early adopters and demos to actually exercise consolidation. The audit
// flagged 100 as too high; 20 is the new contract.
func TestDefaultConfig_ConsolidateThreshold(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ConsolidateThreshold != 20 {
		t.Fatalf("ConsolidateThreshold default: got %d, want 20", cfg.ConsolidateThreshold)
	}
}

// TestConfig_RankingFieldsReachTheStore covers the plumbing rather than the
// behaviour: a Config field that is accepted, documented, and then dropped on
// the way to StoreConfig is worse than not having it, because it fails
// silently. Both new fields are exercised through the public constructor.
func TestConfig_RankingFieldsReachTheStore(t *testing.T) {
	ctx := context.Background()

	// Recency-only weights make the effect unmistakable: the ranking must
	// ignore keyword relevance entirely and return the newest fact first.
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = EmbeddingKeyword
	cfg.SignalWeights = &memory.SignalWeights{Vector: 0, Keyword: 0, Recency: 1}

	mem, err := NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer mem.Close()

	if err := mem.Remember(ctx, "cfg", "kubernetes deployment pipeline notes"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := mem.Remember(ctx, "cfg", "the coffee machine takes whole beans"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	got, err := mem.Recall(ctx, "cfg", "kubernetes deployment pipeline")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Recall returned nothing")
	}
	if !strings.Contains(got[0], "coffee") {
		t.Errorf("Config.SignalWeights did not reach the store: recency-only ranking "+
			"put %q first, expected the newest fact", got[0])
	}
}

// TestConfig_MinRelevanceReachesTheStore does the same for the relevance floor.
func TestConfig_MinRelevanceReachesTheStore(t *testing.T) {
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = EmbeddingKeyword
	cfg.MinRelevance = 0.9

	mem, err := NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer mem.Close()

	if err := mem.Remember(ctx, "cfg", "kubernetes deployment pipeline notes"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	for _, filler := range []string{
		"the coffee machine takes whole beans",
		"parking permits renew through the portal",
		"lunch is catered on fridays",
	} {
		if err := mem.Remember(ctx, "cfg", filler); err != nil {
			t.Fatalf("Remember: %v", err)
		}
	}

	got, err := mem.Recall(ctx, "cfg", "kubernetes deployment pipeline")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("MinRelevance trimmed the best match")
	}
	if len(got) == 4 {
		t.Errorf("Config.MinRelevance did not reach the store: all %d facts were "+
			"returned despite a 0.9 floor: %v", len(got), got)
	}
}
