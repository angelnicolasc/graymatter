package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestCheckDataDir(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		c := checkDataDir(filepath.Join(t.TempDir(), "nope"))
		if c.Status != "warn" {
			t.Errorf("status = %s, want warn", c.Status)
		}
		if !strings.Contains(c.Hint, "graymatter init") {
			t.Errorf("hint should point at init, got %q", c.Hint)
		}
	})
	t.Run("writable", func(t *testing.T) {
		c := checkDataDir(t.TempDir())
		if c.Status != "ok" {
			t.Errorf("status = %s, want ok (%s)", c.Status, c.Detail)
		}
	})
	t.Run("file not dir", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := checkDataDir(path)
		if c.Status != "fail" {
			t.Errorf("status = %s, want fail", c.Status)
		}
	})
}

func TestCheckStore(t *testing.T) {
	t.Run("no db yet", func(t *testing.T) {
		c := checkStore(t.TempDir())
		if c.Status != "info" {
			t.Errorf("status = %s, want info (%s)", c.Status, c.Detail)
		}
	})

	t.Run("healthy with facts", func(t *testing.T) {
		dir := t.TempDir()
		store, err := memory.Open(memory.StoreConfig{DataDir: dir})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		if err := store.Put(context.Background(), "agent-x", "fact one"); err != nil {
			t.Fatalf("put: %v", err)
		}
		if err := store.Put(context.Background(), "agent-x", "fact two"); err != nil {
			t.Fatalf("put: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		c := checkStore(dir)
		if c.Status != "ok" {
			t.Fatalf("status = %s, want ok (%s)", c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "2 fact(s)") || !strings.Contains(c.Detail, "1 agent(s)") {
			t.Errorf("detail %q should report 2 facts / 1 agent", c.Detail)
		}
	})

	t.Run("locked by another process reports warn", func(t *testing.T) {
		dir := t.TempDir()
		// Hold the write lock in-process; the doctor probe is a separate
		// bolt.Open on the same file, which contends the same flock.
		store, err := memory.Open(memory.StoreConfig{DataDir: dir})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer func() { _ = store.Close() }()

		c := checkStore(dir)
		if c.Status != "warn" {
			t.Fatalf("status = %s, want warn (%s)", c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "non-daemon process") {
			t.Errorf("detail %q should explain the single-writer lock", c.Detail)
		}
	})
}

func TestCheckMCPWiring(t *testing.T) {
	// Pin codex home lookups away from the real user profile.
	testHomeOverride = t.TempDir()
	defer func() { testHomeOverride = "" }()

	t.Run("nothing wired", func(t *testing.T) {
		c := checkMCPWiring(t.TempDir())
		if c.Status != "warn" {
			t.Errorf("status = %s, want warn", c.Status)
		}
	})

	t.Run("claude code wired", func(t *testing.T) {
		dir := t.TempDir()
		mcpJSON := `{"mcpServers":{"graymatter":{"command":"graymatter","args":["mcp","serve"]}}}`
		if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcpJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		c := checkMCPWiring(dir)
		if c.Status != "ok" {
			t.Fatalf("status = %s, want ok (%s)", c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "Claude Code") {
			t.Errorf("detail %q should name Claude Code", c.Detail)
		}
	})
}

func TestCheckInstructions(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		c := checkInstructions(t.TempDir())
		if c.Status != "warn" {
			t.Errorf("status = %s, want warn", c.Status)
		}
	})
	t.Run("present after init writer", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := upsertInstructionsBlock(filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		c := checkInstructions(dir)
		if c.Status != "ok" {
			t.Fatalf("status = %s, want ok (%s)", c.Status, c.Detail)
		}
	})
}
