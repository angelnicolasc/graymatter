package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func runStatusCmd(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := statusCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// statusTestStore seeds a store with two agents, one tombstoned fact, and one
// recalled fact (AccessCount bumped directly, as Recall's writeback would).
func statusTestStore(t *testing.T) func() {
	t.Helper()
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	oldDir := dataDir
	dataDir = t.TempDir()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := mem.Remember(ctx, "planner", strings.Repeat("planning observation ", 12)); err != nil {
		t.Fatalf("remember planner: %v", err)
	}
	if err := mem.Remember(ctx, "writer", strings.Repeat("draft note ", 10)); err != nil {
		t.Fatalf("remember writer: %v", err)
	}
	adv := mem.Advanced()
	facts, _ := adv.List("planner")
	f := facts[0]
	f.SupersededBy = "gone"
	if err := adv.UpdateFact("planner", f); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	live, _ := adv.List("writer")
	live[0].AccessCount = 3
	if err := adv.UpdateFact("writer", live[0]); err != nil {
		t.Fatalf("bump access: %v", err)
	}
	_ = mem.Close() // release the write lock; the command reopens on its own
	return func() { dataDir = oldDir }
}

func TestStatus_HumanSections(t *testing.T) {
	cleanup := statusTestStore(t)
	defer cleanup()

	out := runStatusCmd(t)
	for _, want := range []string{
		"GrayMatter status",
		"2 agents",
		"1 superseded",
		"planner 0",
		"writer 3",
		"total 3",
		"auto-population OFF",
		"init --kg",
		"recorded by 'graymatter run' sessions only",
		"INJECTION",
		"cannot see your chat history",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestStatus_JSONShape(t *testing.T) {
	cleanup := statusTestStore(t)
	defer cleanup()

	jsonOut = true
	t.Cleanup(func() { jsonOut = false })
	out := runStatusCmd(t)

	var payload struct {
		Mode  string `json:"mode"`
		Store *struct {
			TotalAgents     int `json:"total_agents"`
			TotalLiveFacts  int `json:"total_live_facts"`
			TotalTombstones int `json:"total_tombstones"`
			Agents          []struct {
				Agent   string `json:"agent"`
				Recalls int    `json:"recalls"`
			} `json:"agents"`
		} `json:"store"`
		KG *struct {
			AutoPopulate bool `json:"auto_populate"`
		} `json:"kg"`
		Tokens30 *harness.TokenUsageSummary `json:"tokens_30d"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if payload.Store == nil || payload.Store.TotalAgents != 2 || payload.Store.TotalLiveFacts != 1 {
		t.Errorf("unexpected store overview: %+v", payload.Store)
	}
	if payload.Store == nil || payload.Store.TotalTombstones != 1 {
		t.Errorf("tombstone total wrong: %+v", payload.Store)
	}
	if payload.KG == nil || payload.KG.AutoPopulate {
		t.Errorf("KG state wrong: %+v", payload.KG)
	}
}

func TestStatus_EmptyStoreGuides(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	oldDir := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = oldDir })

	out := runStatusCmd(t)
	if !strings.Contains(out, "empty") || !strings.Contains(out, "graymatter init") {
		t.Errorf("empty store should guide toward init:\n%s", out)
	}
}

// TestDirectStoreOverview_MatchesGroundTruth pins the in-process aggregation
// against values computed by hand from List(): live counts exclude
// tombstones, recalls sum access counters, weights average over live facts.
func TestDirectStoreOverview_MatchesGroundTruth(t *testing.T) {
	cleanup := statusTestStore(t)
	defer cleanup()

	ds, err := openDirectStore()
	if err != nil {
		t.Fatalf("open direct: %v", err)
	}
	defer func() { _ = ds.Close() }()

	ov, err := ds.StoreOverview()
	if err != nil {
		t.Fatalf("StoreOverview: %v", err)
	}

	wantLive := map[string]int{"planner": 0, "writer": 1} // planner's single fact is retired
	wantRecalls := map[string]int{"planner": 0, "writer": 3}
	gotLive := map[string]int{}
	gotRecalls := map[string]int{}
	for _, a := range ov.Agents {
		gotLive[a.Agent] = a.LiveFacts
		gotRecalls[a.Agent] = a.Recalls

		// Ground truth straight from List.
		facts, _ := ds.List(a.Agent)
		liveN, recN := 0, 0
		for _, f := range facts {
			if f.SupersededBy == "" {
				liveN++
				recN += f.AccessCount
			}
		}
		if liveN != a.LiveFacts || recN != a.Recalls {
			t.Errorf("agent %q: overview says live=%d recalls=%d, List ground truth is %d/%d",
				a.Agent, a.LiveFacts, a.Recalls, liveN, recN)
		}
	}
	for agent, want := range wantLive {
		if gotLive[agent] != want {
			t.Errorf("agent %q live = %d, want %d", agent, gotLive[agent], want)
		}
	}
	for agent, want := range wantRecalls {
		if gotRecalls[agent] != want {
			t.Errorf("agent %q recalls = %d, want %d", agent, gotRecalls[agent], want)
		}
	}
	if ov.TotalTombstones != 1 {
		t.Errorf("tombstones = %d, want 1", ov.TotalTombstones)
	}
}

// TestStatus_InjectionEstimateReflectsHookBudgets: the INJECTION line quotes
// the session-start hook's real block — the agent's top-5 plus the shared
// namespace's top-3 — not a generic top-8, and __shared__ itself is never
// counted as a hook agent row.
func TestStatus_InjectionEstimateReflectsHookBudgets(t *testing.T) {
	agentFacts := []memory.Fact{
		{ID: "w1", AgentID: "writer", Text: "one two three four five six"},
	}
	sharedFacts := []memory.Fact{
		{ID: "s1", AgentID: memory.SharedAgentID, Text: "shared convention one two three"},
		{ID: "s2", AgentID: memory.SharedAgentID, Text: "shared convention four five six"},
	}
	view := statusView{
		Mode: "in-process",
		Overview: &daemon.StoreOverviewResponse{
			TotalAgents:    2,
			TotalLiveFacts: 3,
			Agents: []daemon.AgentSummary{
				{Agent: "writer", LiveFacts: 1},
				{Agent: memory.SharedAgentID, LiveFacts: 2},
			},
		},
		KG:    &daemon.KGStateResponse{},
		Facts: map[string][]memory.Fact{"writer": agentFacts, memory.SharedAgentID: sharedFacts},
	}

	var out bytes.Buffer
	if err := renderStatus(&out, view); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	got := out.String()

	wantBlock := estimateTopN(agentFacts, hookSessionStartAgentTopK) + estimateTopN(sharedFacts, hookSessionStartSharedTopK)
	wantLine := fmt.Sprintf("INJECTION  est. session-start block (top-%d agent + top-%d shared): ~%d–%d tk/agent",
		hookSessionStartAgentTopK, hookSessionStartSharedTopK, wantBlock, wantBlock)
	if !strings.Contains(got, wantLine) {
		t.Errorf("INJECTION line must quote the hook's real block (%s):\n%s", wantLine, got)
	}

	// The old generic top-8 shape must not come back: top-8 of the agent
	// alone (no shared part) is a different number for this store.
	onlyAgentTop8 := estimateTopN(agentFacts, 8)
	if onlyAgentTop8 != wantBlock {
		// Guard the guard: with these facts the two estimates differ, so the
		// assertion below is meaningful.
		if strings.Contains(got, fmt.Sprintf("~%d–%d tk/agent", onlyAgentTop8, onlyAgentTop8)) &&
			!strings.Contains(got, wantLine) {
			t.Errorf("INJECTION line fell back to a plain top-8 estimate:\n%s", got)
		}
	}
}

// The daemon-side aggregation is pinned against a real daemon in
// internal/daemon TestHostService_CoreSurface; the two implementations share
// semantics (live excludes tombstones; recalls sums AccessCount).

// Cache reads are part of the input side: the percentage must be
// CacheRead / (Input + CacheRead). Dividing by Input+Output printed
// impossible values (>100%) on cache-heavy workloads.
func TestStatus_CacheReadPercentage(t *testing.T) {
	view := statusView{
		Mode: "in-process",
		Overview: &daemon.StoreOverviewResponse{
			TotalAgents:    1,
			TotalLiveFacts: 1,
			Agents:         []daemon.AgentSummary{{Agent: "writer", LiveFacts: 1, Recalls: 3, AvgWeight: 1}},
		},
		KG: &daemon.KGStateResponse{},
		Tokens: harness.TokenUsageSummary{
			Loaded:   true,
			Requests: 10,
			Input:    1_000,
			Output:   500,
			CacheRead: 9_000,
		},
		Facts: map[string][]memory.Fact{
			"writer": {{ID: "f1", AgentID: "writer", Text: "one two three four five"}},
		},
	}

	var out bytes.Buffer
	if err := renderStatus(&out, view); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "cache-read 90%") {
		t.Errorf("cache-read line wrong, want 90%% (CacheRead/(Input+CacheRead)):\n%s", got)
	}
}
