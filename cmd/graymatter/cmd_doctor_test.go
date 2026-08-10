package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// TestCheckStore_FlagsLongIdleProject covers the state issue #14 was actually
// about: a project that is wired correctly, reports green everywhere, and has
// never stored a single fact. A fresh install looks identical, so the check has
// to key off how long the project has been sitting there.
func TestCheckStore_FlagsLongIdleProject(t *testing.T) {
	dir := t.TempDir()
	memoryMD := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(memoryMD, []byte("# GrayMatter Memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Just initialised and empty: nothing to complain about yet.
	if got := checkStore(dir); got.Status != "info" {
		t.Errorf("fresh project: status = %q, want info (%s)", got.Status, got.Detail)
	}

	// Two days on, still empty. That is the failure users kept reporting.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(memoryMD, old, old); err != nil {
		t.Fatal(err)
	}
	got := checkStore(dir)
	if got.Status != "warn" {
		t.Fatalf("idle project: status = %q, want warn (%s)", got.Status, got.Detail)
	}
	if got.Hint == "" {
		t.Error("idle warning must carry an actionable hint, that is the whole point")
	}
	if !strings.Contains(got.Detail, "no facts") {
		t.Errorf("detail should say nothing was stored, got %q", got.Detail)
	}
}

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
	// checkInstructions now resolves coverage per wired agent, and two of the
	// agents are home-scoped (Codex's config, Claude Code's and OpenCode's
	// global briefings). Without pinning both, this test reads the developer's
	// real home and its result depends on whose machine it runs on.
	testHomeOverride = t.TempDir()
	t.Cleanup(func() { testHomeOverride = "" })
	t.Setenv("XDG_CONFIG_HOME", "")

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

// TestCheckInstructions_GlobalCoverage is the check that used to contradict
// `graymatter init --global`, the command its own hint recommends. A global
// install puts the block where Claude Code and OpenCode read it in every
// project; doctor looked only at the project directory and reported that as
// missing.
//
// The inverse matters just as much. Cursor reads a project AGENTS.md and has no
// home-scoped file that `init --global` writes, so crediting it for a global
// block would hide a gap instead of a false alarm.
func TestCheckInstructions_GlobalCoverage(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })
	t.Setenv("XDG_CONFIG_HOME", "")

	if warns := installGlobalInstructions(true); len(warns) > 0 {
		t.Fatalf("global install warned: %v", warns)
	}

	// wireOnly writes an MCP config for exactly one agent, so checkInstructions
	// resolves coverage for that agent alone.
	wireOnly := func(t *testing.T, id string) string {
		t.Helper()
		dir := t.TempDir()
		for _, a := range knownAgents(dir) {
			if a.id != id {
				continue
			}
			if _, err := a.run(); err != nil {
				t.Fatalf("wiring %s: %v", id, err)
			}
		}
		return dir
	}

	t.Run("claude code is covered by the global file", func(t *testing.T) {
		c := checkInstructions(wireOnly(t, "claudecode"))
		if c.Status != "ok" {
			t.Errorf("status = %s, want ok (%s)", c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "global") {
			t.Errorf("detail should name the global file, got %q", c.Detail)
		}
	})

	t.Run("opencode is covered by the global file", func(t *testing.T) {
		c := checkInstructions(wireOnly(t, "opencode"))
		if c.Status != "ok" {
			t.Errorf("status = %s, want ok (%s)", c.Status, c.Detail)
		}
	})

	t.Run("cursor is not", func(t *testing.T) {
		c := checkInstructions(wireOnly(t, "cursor"))
		if c.Status != "warn" {
			t.Fatalf("status = %s, want warn — a global block does not reach Cursor (%s)", c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "Cursor") {
			t.Errorf("detail should name Cursor, got %q", c.Detail)
		}
	})
}

// TestCheckInstructions_StaleBlock: a project set up before v0.7.0 carries the
// briefing that told the model to search "when prior context might matter". Its
// markers are identical to today's, so the old check reported it as healthy and
// nobody was told to re-run init.
func TestCheckInstructions_StaleBlock(t *testing.T) {
	testHomeOverride = t.TempDir()
	t.Cleanup(func() { testHomeOverride = "" })
	t.Setenv("XDG_CONFIG_HOME", "")

	dir := t.TempDir()
	stale := instrBeginMarker + "\n## Memory (GrayMatter)\n\ncall memory_search when prior context might matter\n" + instrEndMarker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	c := checkInstructions(dir)
	if c.Status != "warn" {
		t.Fatalf("status = %s, want warn (%s)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "older version") {
		t.Errorf("detail should say the block is outdated, got %q", c.Detail)
	}

	// Re-running init is the documented remedy, so it has to actually clear it.
	if _, err := upsertInstructionsBlock(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if c := checkInstructions(dir); c.Status != "ok" {
		t.Errorf("status after re-running init = %s, want ok (%s)", c.Status, c.Detail)
	}
}
