package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

// connectFresh spawns a daemon for a temp dir and returns a connected client.
func connectFresh(t *testing.T) *Client {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	withBuiltDaemon(t)
	dir := t.TempDir()
	c, err := Connect(dir)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(); _ = c.Close() })
	return c
}

// TestHostService_CoreSurface drives the full client surface against a real
// daemon: the memory operations plus every host-service method (checkpoints,
// sessions, KG, audit, tokens). This is the contract the TUI, MCP server, and
// CLI all depend on.
func TestHostService_CoreSurface(t *testing.T) {
	c := connectFresh(t)
	ctx := context.Background()

	// --- memory surface ---
	if err := c.Remember(ctx, "agent-a", "alpha fact"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := c.PutShared(ctx, "shared fact"); err != nil {
		t.Fatalf("PutShared: %v", err)
	}
	if got, err := c.RecallDefault(ctx, "agent-a", "alpha"); err != nil || len(got) == 0 {
		t.Fatalf("RecallDefault = %v, %v", got, err)
	}
	if got, err := c.RecallShared(ctx, "shared", 5); err != nil || len(got) == 0 {
		t.Fatalf("RecallShared = %v, %v", got, err)
	}
	if got, err := c.RecallAll(ctx, "agent-a", "fact", 0); err != nil || len(got) == 0 {
		t.Fatalf("RecallAll = %v, %v", got, err)
	}
	agents, err := c.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) == 0 {
		t.Fatal("ListAgents empty")
	}
	facts, err := c.List("agent-a")
	if err != nil || len(facts) != 1 {
		t.Fatalf("List = %v, %v", facts, err)
	}
	if st, err := c.Stats("agent-a"); err != nil || st.FactCount != 1 {
		t.Fatalf("Stats = %+v, %v", st, err)
	}
	// UpdateFact (weight to 0), then Delete.
	f := facts[0]
	f.Weight = 0.5
	if err := c.UpdateFact("agent-a", f); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}
	if err := c.Delete("agent-a", f.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n, err := c.PendingVectorCount(); err != nil || n != 0 {
		t.Fatalf("PendingVectorCount = %d, %v", n, err)
	}

	// --- checkpoints ---
	saved, err := c.CheckpointSave(session.Checkpoint{
		AgentID:  "agent-a",
		State:    map[string]any{"k": "v"},
		Messages: []session.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil || saved.ID == "" {
		t.Fatalf("CheckpointSave = %+v, %v", saved, err)
	}
	if got, err := c.CheckpointResume("agent-a"); err != nil || got.ID != saved.ID {
		t.Fatalf("CheckpointResume = %+v, %v", got, err)
	}
	if got, err := c.CheckpointLoad("agent-a", saved.ID); err != nil || got.ID != saved.ID {
		t.Fatalf("CheckpointLoad = %+v, %v", got, err)
	}
	if cps, err := c.CheckpointList("agent-a"); err != nil || len(cps) != 1 {
		t.Fatalf("CheckpointList = %v, %v", cps, err)
	}

	// --- sessions ---
	hs := harness.HarnessSession{ID: "sess-1", AgentID: "agent-a", AgentFile: "a.md", Status: "done"}
	if err := c.SessionSave(hs); err != nil {
		t.Fatalf("SessionSave: %v", err)
	}
	sessions, err := c.SessionsList()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("SessionsList = %v, %v", sessions, err)
	}
	if id, err := c.SessionResolve("agent-a", "latest"); err != nil || id != "sess-1" {
		t.Fatalf("SessionResolve = %q, %v", id, err)
	}
	// Kill a non-running session: must error clearly, not panic.
	if err := c.SessionKill("sess-1"); err == nil {
		t.Error("SessionKill on a non-running session should error")
	}

	// --- knowledge graph ---
	if err := c.KGUpsert("node-a", "Node A", "concept"); err != nil {
		t.Fatalf("KGUpsert a: %v", err)
	}
	if err := c.KGUpsert("node-b", "Node B", "concept"); err != nil {
		t.Fatalf("KGUpsert b: %v", err)
	}
	if err := c.KGLink("node-a", "node-b", "relates_to"); err != nil {
		t.Fatalf("KGLink: %v", err)
	}
	nodes, err := c.KGNodes()
	if err != nil {
		t.Fatalf("KGNodes: %v", err)
	}
	if len(nodes) < 2 {
		t.Fatalf("KGNodes = %d, want >= 2", len(nodes))
	}

	// --- audit ---
	if err := c.AuditWrite(audit.Entry{Action: "forget", Agent: "agent-a", NewText: "x", Source: "test"}); err != nil {
		t.Fatalf("AuditWrite: %v", err)
	}

	// --- token ledger ---
	if err := c.TokenRecord("agent-a", "claude-sonnet-4-6", 100, 50, 10, 5); err != nil {
		t.Fatalf("TokenRecord: %v", err)
	}
	sum, err := c.TokenSummary(30)
	if err != nil {
		t.Fatalf("TokenSummary: %v", err)
	}
	if !sum.Loaded || sum.Requests != 1 {
		t.Fatalf("TokenSummary = %+v, want Loaded with 1 request", sum)
	}
}

// TestConnect_NoSpawnWhenAbsent verifies ConnectNoSpawn does not start a
// daemon and reports a clear error.
func TestConnect_NoSpawnWhenAbsent(t *testing.T) {
	_, err := ConnectNoSpawn(t.TempDir())
	if err == nil {
		t.Fatal("ConnectNoSpawn should fail when no daemon is running")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("error %q should say the daemon is not reachable", err)
	}
}
