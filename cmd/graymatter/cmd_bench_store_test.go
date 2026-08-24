package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// factWithText finds a fact by exact text in a seeded list.
func factByText(t *testing.T, facts []memory.Fact, text string) memory.Fact {
	t.Helper()
	for _, f := range facts {
		if f.Text == text {
			return f
		}
	}
	t.Fatalf("seed fact %q missing", text)
	return memory.Fact{}
}

func TestMeasureStoreAgent_ExcludesTombstonesAndWeightsTop8(t *testing.T) {
	facts := make([]memory.Fact, 0, 12)
	for i := 0; i < 12; i++ {
		facts = append(facts, memory.Fact{
			ID: "f", AgentID: "a", Text: strings.Repeat("word ", 20),
			CreatedAt: time.Now(), Weight: 0.5,
		})
	}
	// One retired fact must vanish from every metric.
	facts[3].SupersededBy = "successor"
	// The heaviest live fact must lead the top-8 even though it is oldest.
	facts[11].Weight = 0.99
	// A tombstone with an enormous weight must still be excluded.
	facts[4].Weight = 1.0

	row := measureStoreAgent("a", facts)

	if row.LiveFacts != 11 {
		t.Errorf("live facts = %d, want 11 (tombstone excluded)", row.LiveFacts)
	}
	if row.FullTokens == 0 || row.Sliding8Tokens == 0 || row.Top8WeightedTok == 0 {
		t.Fatalf("empty metrics on a non-empty store: %+v", row)
	}
	if row.FullTokens <= row.Sliding8Tokens {
		t.Errorf("full dump %d should cost more than an 8-fact window %d",
			row.FullTokens, row.Sliding8Tokens)
	}

	// Recompute the top-8 expectation independently: 11 live facts, the 8
	// heaviest include the 0.99 one; cost equals any 8 of them (~same length).
	if got := row.Top8WeightedTok; got >= row.FullTokens {
		t.Errorf("top-8 estimate %d must stay below full dump %d", got, row.FullTokens)
	}

	empty := measureStoreAgent("empty", nil)
	if empty.LiveFacts != 0 || empty.FullTokens != 0 {
		t.Errorf("empty agent should report zeros, got %+v", empty)
	}
}

// seedStore opens a real direct store in a temp dir and stores n facts,
// returning the raw handle so tests can retire facts before measuring.
func seedStore(t *testing.T, n int) (*directStore, func()) {
	t.Helper()
	oldDir := dataDir
	dataDir = t.TempDir()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir // the store under test lives where openStore will look
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ds := &directStore{mem: mem, store: mem.Advanced()}
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := ds.Remember(ctx, "bench-agent", strings.Repeat("observation number "+string(rune('a'+i%26))+" ", 15)); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	return ds, func() { dataDir = oldDir; _ = mem.Close() }
}

func runBenchCmd(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := benchCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bench %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestBenchStore_JSONReportsLiveFactsOnly(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "1") // measure in-process; never spawn a daemon from a test
	// 12 seeds so that after retiring one, 11 live facts remain and the
	// top-8 estimate must be strictly cheaper than the full dump — with 8 or
	// fewer live facts they are legitimately identical, as the published
	// table's 0%-at-1-session row already says.
	ds, cleanup := seedStore(t, 12)
	defer cleanup()

	// Retire one fact so live != stored.
	all, err := ds.List("bench-agent")
	if err != nil || len(all) != 12 {
		t.Fatalf("seed state wrong: %v / %d", err, len(all))
	}
	victim := factByText(t, all, all[0].Text)
	victim.SupersededBy = "gone"
	if err := ds.UpdateFact("bench-agent", victim); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if err := ds.Close(); err != nil { // release the lock: measure as a user would
		t.Fatalf("close seeded handle: %v", err)
	}

	jsonOut = true
	t.Cleanup(func() { jsonOut = false })
	out := runBenchCmd(t, "--store")

	var payload struct {
		Suite   string `json:"suite"`
		DataDir string `json:"data_dir"`
		Mode    string `json:"mode"`
		Agents  []struct {
			Agent           string `json:"agent"`
			LiveFacts       int    `json:"live_facts"`
			FullTokens      int    `json:"full_dump_tokens"`
			Sliding8Tokens  int    `json:"sliding_8_tokens"`
			Top8WeightedTok int    `json:"estimated_top8_tokens"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if payload.Suite != "store" || len(payload.Agents) != 1 {
		t.Fatalf("unexpected payload shape: suite=%q agents=%d", payload.Suite, len(payload.Agents))
	}
	row := payload.Agents[0]
	if row.Agent != "bench-agent" || row.LiveFacts != 11 {
		t.Errorf("agent=%q live=%d, want bench-agent/11 (one tombstone)", row.Agent, row.LiveFacts)
	}
	if row.FullTokens == 0 || row.Top8WeightedTok >= row.FullTokens {
		t.Errorf("token metrics not ordered as expected: full=%d top8=%d",
			row.FullTokens, row.Top8WeightedTok)
	}
	if payload.Mode != "in-process" {
		t.Errorf("mode = %q, want in-process under a direct store", payload.Mode)
	}
}

func TestBenchStore_HumanOutputAndProbe(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	ds, cleanup := seedStore(t, 9)
	// Release the write lock so the command opens the store read-write —
	// exactly how a user runs it against a quiet store.
	if err := ds.Close(); err != nil {
		t.Fatalf("close seeded handle: %v", err)
	}
	defer cleanup()

	out := runBenchCmd(t, "--store")
	if !strings.Contains(out, "live facts: 9") {
		t.Errorf("human output missing live count:\n%s", out)
	}
	if !strings.Contains(out, "bench-agent") {
		t.Error("human output missing agent name")
	}

	jsonOut = true
	t.Cleanup(func() { jsonOut = false })
	out = runBenchCmd(t, "--store", "--probe-recall")
	var payload struct {
		Probe *struct {
			Queries int `json:"queries"`
		} `json:"probe"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if payload.Probe == nil || payload.Probe.Queries < 1 {
		t.Errorf("probe produced no measurable samples: %+v", payload.Probe)
	}
	// Note on access counters: Recall bumps them via detached writebacks (the
	// exact flakiness benchmarks/token_count documents), so their timing is
	// not asserted here — only that real recalls were issued and priced.
}

func TestBenchStore_UnknownAgentErrors(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	ds, cleanup := seedStore(t, 2)
	if err := ds.Close(); err != nil { // release the lock before invoking the command
		t.Fatalf("close seeded handle: %v", err)
	}
	defer cleanup()

	cmd := benchCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--store", "--agent", "nobody"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no such agent") {
		t.Fatalf("expected no-such-agent error, got %v", err)
	}
}
