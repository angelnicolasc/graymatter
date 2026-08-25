package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The host handlers run inside the spawned daemon process, so the E2E tests
// never see them in coverage — the client exercises them across a process
// boundary the profiler cannot cross. These tests call the same handlers
// directly, in-process, over a real store: identical code paths, measurable.

func newDirectHost(t *testing.T) *Host {
	t.Helper()
	dir := t.TempDir()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	cfg.StrictWrite = true
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	adv := mem.Advanced()
	db := adv.DB()
	g, gerr := kg.Open(db)
	if gerr != nil {
		t.Fatalf("kg open: %v", gerr)
	}
	adapter := kg.NewGraphAdapter(g)

	return &Host{
		mem:     mem,
		db:      db,
		graph:   g,
		adapter: adapter,
		kgAuto:  false,
		stop:    func() {},
	}
}

func TestHostHandlers_CheckpointLifecycle(t *testing.T) {
	h := newDirectHost(t)

	saved := CheckpointSaveResponse{}
	if err := h.CheckpointSave(&CheckpointSaveRequest{CP: session.Checkpoint{
		AgentID:  "cp-agent",
		State:    map[string]any{"step": float64(2)},
		Messages: []session.Message{{Role: "user", Content: "hi"}},
	}}, &saved); err != nil || saved.CP.ID == "" {
		t.Fatalf("CheckpointSave = %+v, %v", saved.CP, err)
	}

	var resumed CheckpointResumeResponse
	if err := h.CheckpointResume(&CheckpointResumeRequest{AgentID: "cp-agent"}, &resumed); err != nil || resumed.CP.ID != saved.CP.ID {
		t.Fatalf("CheckpointResume = %+v, %v", resumed.CP, err)
	}

	var loaded CheckpointLoadResponse
	if err := h.CheckpointLoad(&CheckpointLoadRequest{AgentID: "cp-agent", CheckpointID: saved.CP.ID}, &loaded); err != nil || loaded.CP.ID != saved.CP.ID {
		t.Fatalf("CheckpointLoad = %+v, %v", loaded.CP, err)
	}

	var listed CheckpointListResponse
	if err := h.CheckpointList(&CheckpointListRequest{AgentID: "cp-agent"}, &listed); err != nil || len(listed.CPs) != 1 {
		t.Fatalf("CheckpointList = %v, %v", listed.CPs, err)
	}

	var missing CheckpointLoadResponse
	if err := h.CheckpointLoad(&CheckpointLoadRequest{AgentID: "cp-agent", CheckpointID: "ghost"}, &missing); err == nil {
		t.Error("loading a ghost checkpoint returned no error")
	}
}

func TestHostHandlers_SessionsAndAudit(t *testing.T) {
	h := newDirectHost(t)

	hs := harness.HarnessSession{ID: "sess-x", AgentID: "agent-x", AgentFile: "x.md", Status: "done"}
	var saved SessionSaveResponse
	if err := h.SessionSave(&SessionSaveRequest{S: hs}, &saved); err != nil {
		t.Fatalf("SessionSave: %v", err)
	}
	var listed SessionListResponse
	if err := h.SessionList(&SessionListRequest{}, &listed); err != nil || len(listed.Sessions) != 1 {
		t.Fatalf("SessionList = %v, %v", listed.Sessions, err)
	}
	var resolved SessionResolveResponse
	if err := h.SessionResolve(&SessionResolveRequest{AgentID: "agent-x", SessionID: "latest"}, &resolved); err != nil || resolved.ID != "sess-x" {
		t.Fatalf("SessionResolve = %q, %v", resolved.ID, err)
	}
	var killed SessionKillResponse
	if err := h.SessionKill(&SessionKillRequest{ID: "sess-x"}, &killed); err == nil {
		t.Error("killing a finished session should error")
	}

	var audited AuditWriteResponse
	if err := h.AuditWrite(&AuditWriteRequest{E: audit.Entry{
		Action: "forget", Agent: "agent-x", NewText: "n", Source: "gaps-test",
	}}, &audited); err != nil {
		t.Errorf("AuditWrite: %v", err)
	}
}

func TestHostHandlers_TokenLedgerAggregates(t *testing.T) {
	h := newDirectHost(t)

	if err := h.TokenRecord(&TokenRecordRequest{
		Agent: "tok-agent", Model: "claude-sonnet-4-6-20260301",
		Input: 1000, Output: 100, CacheRead: 500,
	}, &TokenRecordResponse{}); err != nil {
		t.Fatalf("TokenRecord priced: %v", err)
	}
	if err := h.TokenRecord(&TokenRecordRequest{
		Agent: "tok-agent", Model: "mystery-model",
		Input: 10, Output: 1,
	}, &TokenRecordResponse{}); err != nil {
		t.Fatalf("TokenRecord unpriced: %v", err)
	}

	var sum TokenSummaryResponse
	if err := h.TokenSummary(&TokenSummaryRequest{Days: 30}, &sum); err != nil {
		t.Fatalf("TokenSummary: %v", err)
	}
	if !sum.S.Loaded || sum.S.Requests != 2 {
		t.Fatalf("summary = %+v, want Loaded with 2 requests", sum.S)
	}
	if !sum.S.Partial {
		t.Error("unpriced model did not flag Partial")
	}
	if sum.S.CacheHitRate <= 0 {
		t.Errorf("cache hit rate = %v, want >0 from cache-read tokens", sum.S.CacheHitRate)
	}
}

func TestHostHandlers_KGSurfaceAndExport(t *testing.T) {
	h := newDirectHost(t)

	var up KGUpsertResponse
	for _, n := range []struct{ id, label string }{
		{"node-a", "Node A"}, {"node-b", "Node B"},
	} {
		req := &KGUpsertRequest{ID: n.id, Label: n.label, EntityType: "concept"}
		if err := h.KGUpsert(req, &up); err != nil {
			t.Fatalf("KGUpsert %s: %v", n.id, err)
		}
	}
	var linked KGLinkResponse
	if err := h.KGLink(&KGLinkRequest{From: "node-a", To: "node-b", Relation: "relates_to"}, &linked); err != nil {
		t.Fatalf("KGLink: %v", err)
	}

	var nodes KGNodesResponse
	if err := h.KGNodes(&KGNodesRequest{}, &nodes); err != nil || len(nodes.Nodes) < 2 {
		t.Fatalf("KGNodes = %d, %v", len(nodes.Nodes), err)
	}
	var edges KGEdgesResponse
	if err := h.KGEdges(&KGEdgesRequest{}, &edges); err != nil || len(edges.Edges) != 1 {
		t.Fatalf("KGEdges = %d, %v", len(edges.Edges), err)
	}

	outDir := t.TempDir()
	var exported KGExportObsidianResponse
	if err := h.KGExportObsidian(&KGExportObsidianRequest{OutDir: outDir}, &exported); err != nil {
		t.Errorf("KGExportObsidian into a fresh dir: %v", err)
	}
	// An outDir that is a regular file must fail rather than silently skip.
	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.KGExportObsidian(&KGExportObsidianRequest{OutDir: blocked}, &exported); err == nil {
		t.Error("export into a file path returned no error")
	}
}

func TestHostHandlers_StoreOverviewAndStateAndShutdown(t *testing.T) {
	h := newDirectHost(t)
	ctx := context.Background()

	if err := h.mem.Remember(ctx, "ov-agent", "a live fact for overview"); err != nil {
		t.Fatal(err)
	}
	if err := h.mem.Remember(ctx, "ov-agent", "a fact about to be retired"); err != nil {
		t.Fatal(err)
	}
	// One of the two becomes a tombstone so both counters move.
	adv := h.mem.Advanced()
	all, err := adv.List("ov-agent")
	if err != nil || len(all) < 2 {
		t.Fatalf("seed list: %v (%d)", err, len(all))
	}
	tomb := all[0] // newest = "about to be retired"
	tomb.SupersededBy = memory.SupersededByAgent
	if err := adv.UpdateFact("ov-agent", tomb); err != nil {
		t.Fatal(err)
	}

	var ov StoreOverviewResponse
	if err := h.StoreOverview(&StoreOverviewRequest{}, &ov); err != nil {
		t.Fatalf("StoreOverview: %v", err)
	}
	if ov.TotalAgents == 0 || ov.TotalLiveFacts != 1 || ov.TotalTombstones != 1 {
		t.Errorf("overview = %+v, want 1 live + 1 tombstone", ov)
	}
	if ov.Consolidations != 0 || ov.FactsConsumed != 0 {
		t.Errorf("fresh counters = (%d,%d), want zeros", ov.Consolidations, ov.FactsConsumed)
	}

	var kgst KGStateResponse
	if err := h.KGState(&KGStateRequest{}, &kgst); err != nil {
		t.Fatalf("KGState: %v", err)
	}

	stopped := make(chan struct{})
	h.stop = func() { close(stopped) }
	var sd ShutdownResponse
	if err := h.Shutdown(&ShutdownRequest{}, &sd); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-stopped: // fired asynchronously, per the handler's grace period
	case <-time.After(3 * time.Second):
		t.Error("stop hook never invoked")
	}
}
