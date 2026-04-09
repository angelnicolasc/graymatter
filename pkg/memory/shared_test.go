package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

func openTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	return store, func() { _ = store.Close() }
}

func openTestBolt(t *testing.T, dir string) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRememberShared(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	if err := s.PutShared(ctx, "Global fact: always use metric units."); err != nil {
		t.Fatalf("PutShared: %v", err)
	}

	facts, err := s.List(SharedAgentID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 shared fact, got %d", len(facts))
	}
	if facts[0].Text != "Global fact: always use metric units." {
		t.Errorf("fact text = %q", facts[0].Text)
	}
}

func TestRecallShared(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.PutShared(ctx, "Shared preference: bullet points always.")
	_ = s.PutShared(ctx, "Shared rule: no jargon.")

	results, err := s.RecallShared(ctx, "preference", 8)
	if err != nil {
		t.Fatalf("RecallShared: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least 1 result from RecallShared")
	}
}

func TestRecallAll_MergesAndDeduplicates(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Agent-scoped fact.
	_ = s.Put(ctx, "sales-closer", "Maria Rodriguez is VP Sales at Acme Corp.")
	// Shared fact.
	_ = s.PutShared(ctx, "Global note: always CC the account manager.")
	// Same text in both — should be deduplicated.
	duplicate := "Always start emails with a personal touch."
	_ = s.Put(ctx, "sales-closer", duplicate)
	_ = s.PutShared(ctx, duplicate)

	results, err := s.RecallAll(ctx, "sales-closer", "email", 10)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}

	// Count occurrences of the duplicate.
	count := 0
	for _, r := range results {
		if r == duplicate {
			count++
		}
	}
	if count > 1 {
		t.Errorf("duplicate fact appears %d times in RecallAll results, want ≤1", count)
	}
}

func TestRecallAll_EmptyShared(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "agent-x", "Agent-specific fact about the project.")

	results, err := s.RecallAll(ctx, "agent-x", "project", 8)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected agent-scoped results when shared is empty")
	}
}

func TestRecallAll_EmptyAgent(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.PutShared(ctx, "Shared knowledge about the domain.")

	results, err := s.RecallAll(ctx, "brand-new-agent", "domain", 8)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected shared results when agent memory is empty")
	}
}

func TestSharedAgentID_Reserved(t *testing.T) {
	if SharedAgentID == "" {
		t.Error("SharedAgentID must not be empty")
	}
}
