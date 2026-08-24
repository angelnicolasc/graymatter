package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKGAutoEnabled_DecisionTable pins the OR-semantics in one place: flag,
// environment, or the init --kg sentinel each enable auto-population; none
// means off. This is the function that keeps a hand-run daemon and one
// spawned by an MCP client in agreement.
func TestKGAutoEnabled_DecisionTable(t *testing.T) {
	dir := t.TempDir()

	if kgAutoEnabled(dir, false) {
		t.Error("nothing opted in: auto-population must be off")
	}

	t.Setenv("GRAYMATTER_KG", "1")
	if !kgAutoEnabled(dir, false) {
		t.Error("GRAYMATTER_KG=1 must enable regardless of sentinel")
	}
	os.Unsetenv("GRAYMATTER_KG")

	if err := os.WriteFile(KGSentinelPath(dir), nil, 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if !kgAutoEnabled(dir, false) {
		t.Error("sentinel must enable auto-population")
	}
	if got, want := KGSentinelPath(dir), filepath.Join(dir, "kg.auto"); got != want {
		t.Errorf("KGSentinelPath = %q, want %q", got, want)
	}
}

// TestDaemonSpawnHonorsInitKGSentinel is the acceptance test for the whole
// mechanism: `graymatter init --kg` writes the marker; later, an MCP client
// spawns a daemon WITHOUT any flag or environment — and KGState must report
// auto-population on. Uses a real built binary and the real spawn path.
func TestDaemonSpawnHonorsInitKGSentinel(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	withBuiltDaemon(t)

	dir := t.TempDir()
	if err := os.WriteFile(KGSentinelPath(dir), nil, 0o644); err != nil {
		t.Fatalf("simulate init --kg: %v", err)
	}

	c, err := Connect(dir) // spawns exactly like an MCP client would: no args, no env
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Shutdown(); _ = c.Close() }()

	state, err := c.KGState()
	if err != nil {
		t.Fatalf("KGState: %v", err)
	}
	if !state.AutoPopulate {
		t.Fatal("spawned daemon ignored the kg.auto sentinel")
	}
}

// TestDaemonSpawnWithoutSentinelStaysOff guards the other half of the
// contract: without any opt-in, a spawned daemon must not wire extraction.
func TestDaemonSpawnWithoutSentinelStaysOff(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	withBuiltDaemon(t)
	t.Setenv("GRAYMATTER_KG", "")

	dir := t.TempDir()
	c, err := Connect(dir)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Shutdown(); _ = c.Close() }()

	state, err := c.KGState()
	if err != nil {
		t.Fatalf("KGState: %v", err)
	}
	if state.AutoPopulate {
		t.Fatal("daemon wired extraction with no flag, env, or sentinel")
	}
}
