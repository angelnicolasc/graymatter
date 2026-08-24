package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// seedGrayDB creates a real gray.db in dir (bbolt creates on open) so the
// "database exists" branches of checkStore/checkKG run.
func seedGrayDB(t *testing.T, dir string, facts int) {
	t.Helper()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < facts; i++ {
		if err := store.Put(ctx, "doc-agent", strings.Repeat("fact ", 10)); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCheckStore_EmptyDBYoungProjectGetsRestartHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	seedGrayDB(t, dir, 0) // db exists: the old code went silent here

	c := checkStore(dir)
	if c.Status != "ok" && c.Status != "info" {
		t.Fatalf("status = %s (%s)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Hint, "restart your MCP client") {
		t.Errorf("empty db with no facts must carry the restart hint, got hint=%q", c.Hint)
	}
}

func TestCheckStore_FactsPresentNoHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	seedGrayDB(t, dir, 2)

	c := checkStore(dir)
	if c.Status != "ok" {
		t.Fatalf("status = %s (%s)", c.Status, c.Detail)
	}
	if c.Hint != "" {
		t.Errorf("a used store should not nag: %q", c.Hint)
	}
}

func TestCheckStore_StaleEmptyKeepsRestartAndAddsGuidance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	seedGrayDB(t, dir, 0)

	memoryMD := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(memoryMD, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(memoryMD, old, old); err != nil {
		t.Fatal(err)
	}

	c := checkStore(dir)
	if c.Status != "warn" {
		t.Fatalf("status = %s, want warn for a stale empty project (%s)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Hint, "restart your MCP client") ||
		!strings.Contains(c.Hint, "CLAUDE.md") {
		t.Errorf("stale warning must keep restart first then add guidance, got %q", c.Hint)
	}
}

func TestCheckKG_DecisionTable(t *testing.T) {
	t.Run("off by default is benign info with the enable command", func(t *testing.T) {
		dir := t.TempDir()
		c := checkKG(dir)
		if c.Status != "info" {
			t.Errorf("off state should be info (benign), got %s: %s", c.Status, c.Detail)
		}
		if !strings.Contains(c.Hint, "init --kg") {
			t.Errorf("off state should advertise init --kg, got %q", c.Hint)
		}
	})

	t.Run("sentinel on without db reports enabled and empty", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(daemon.KGSentinelPath(dir), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		c := checkKG(dir)
		if !strings.Contains(c.Detail, "on") {
			t.Errorf("detail should say auto-population is on: %s", c.Detail)
		}
		if !strings.Contains(strings.ToLower(c.Detail+c.Hint), "consolidation") || c.Status == "fail" {
			t.Errorf("enabled-empty state should explain the threshold: status=%s detail=%s hint=%s",
				c.Status, c.Detail, c.Hint)
		}
	})

	t.Run("env var alone enables too", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("GRAYMATTER_KG", "1")
		c := checkKG(dir)
		if !strings.Contains(c.Detail, "on") {
			t.Errorf("env-enabled state not detected: %s", c.Detail)
		}
	})

	t.Run("explicitly linked graph without auto stays non-warn", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("GRAYMATTER_NO_DAEMON", "1")
		cfg := memory.StoreConfig{DataDir: dir}
		store, err := memory.Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		g, err := kg.Open(store.DB())
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Upsert(kg.Node{ID: "a", Label: "A", EntityType: "person"}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		c := checkKG(dir)
		if c.Status == "warn" || c.Status == "fail" {
			t.Errorf("explicit links are legitimate; doctor must not scold: %s / %s", c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "1 nodes") {
			t.Errorf("node count missing from detail: %s", c.Detail)
		}
	})
}
