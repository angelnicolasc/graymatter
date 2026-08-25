package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// W5 acceptance: every rule gets its own synthetic-store fixture; the JSON
// output is stable byte-for-byte across runs over the same store; human
// output carries the same verdicts.

func openHealthStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.Open(memory.StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func finding(t *testing.T, rep healthAgentReport, rule string) healthFinding {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Rule == rule {
			return f
		}
	}
	t.Fatalf("finding %q missing from report %+v", rule, rep)
	return healthFinding{}
}

// TestHealthRuleSupersedeLoop pins the ratio thresholds.
func TestHealthRuleSupersedeLoop(t *testing.T) {
	cases := []struct {
		name       string
		live       int
		tombstones int
		status     string
	}{
		{"clean", 20, 1, "ok"},
		{"warn-band", 6, 4, "warn"},
		{"fail-band", 2, 8, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var facts []memory.Fact
			now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
			for i := 0; i < tc.live; i++ {
				facts = append(facts, memory.Fact{
					ID: fmt.Sprintf("L%02d", i), AgentID: "a",
					Text:      fmt.Sprintf("live fact %d with plenty of substance", i),
					CreatedAt: now, AccessedAt: now, Weight: 0.9,
				})
			}
			for i := 0; i < tc.tombstones; i++ {
				facts = append(facts, memory.Fact{
					ID: fmt.Sprintf("T%02d", i), AgentID: "a",
					Text:      fmt.Sprintf("retired fact %d", i),
					CreatedAt: now, AccessedAt: now, Weight: 0,
					SupersededBy: "successor",
				})
			}
			got := finding(t, auditAgentHealth("a", facts), "supersede-loop")
			if got.Status != tc.status {
				t.Errorf("status = %q (%s), want %q", got.Status, got.Detail, tc.status)
			}
		})
	}
}

// TestHealthRuleDumping plants a burst of thin facts inside one hour; the
// rule must fire on the burst and stay quiet on well-formed content.
func TestHealthRuleDumping(t *testing.T) {
	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	mk := func(n int, text string, step time.Duration) []memory.Fact {
		facts := make([]memory.Fact, 0, n)
		for i := 0; i < n; i++ {
			facts = append(facts, memory.Fact{
				ID: fmt.Sprintf("D%03d", i), AgentID: "a",
				Text:       strings.ReplaceAll(text, "%d", fmt.Sprint(i)),
				CreatedAt:  base.Add(time.Duration(i) * step),
				AccessedAt: base.Add(time.Duration(i) * step),
				Weight:     0.9,
			})
		}
		return facts
	}

	burst := mk(10, "short note %d", 5*time.Minute)
	if got := finding(t, auditAgentHealth("a", burst), "dumping"); got.Status != "warn" {
		t.Errorf("thin burst status = %q (%s), want warn", got.Status, got.Detail)
	}

	substantive := mk(10, "this is a substantive observation number %d about the deployment pipeline and its rollback behaviour", 5*time.Minute)
	if got := finding(t, auditAgentHealth("a", substantive), "dumping"); got.Status != "ok" {
		t.Errorf("substantive writes flagged: %s", got.Detail)
	}

	scattered := mk(10, "short note %d", 3*time.Hour)
	if got := finding(t, auditAgentHealth("a", scattered), "dumping"); got.Status != "ok" {
		t.Errorf("scattered thin notes flagged as a burst: %s", got.Detail)
	}
}

// TestHealthRuleNearPruneCritical connects the doctor to W1's pin: a fact
// that says "always" but weighs almost nothing must be listed.
func TestHealthRuleNearPruneCritical(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	facts := []memory.Fact{
		{ID: "A", AgentID: "a", Text: "The deploy policy is canary first", CreatedAt: now, AccessedAt: now, Weight: 0.04},
		{ID: "B", AgentID: "a", Text: "Always answer in Spanish when asked", CreatedAt: now, AccessedAt: now, Weight: 0.9},
		{ID: "C", AgentID: "a", Text: "low weight but nothing critical here", CreatedAt: now, AccessedAt: now, Weight: 0.01},
	}
	got := finding(t, auditAgentHealth("a", facts), "near-prune-critical")
	if got.Status != "warn" {
		t.Fatalf("status = %q, want warn", got.Status)
	}
	if len(got.Items) != 1 || !strings.HasPrefix(got.Items[0], "A ") {
		t.Errorf("items = %v, want exactly fact A", got.Items)
	}
}

// TestHealthRuleDuplicates checks grouping, ratios and thresholds.
func TestHealthRuleDuplicates(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	f := func(i int, text string) memory.Fact {
		return memory.Fact{ID: fmt.Sprintf("X%02d", i), AgentID: "a",
			Text: text, CreatedAt: now, AccessedAt: now, Weight: 0.9}
	}
	// 10 live, 4 of which normalise to the same text → 40% → fail band.
	facts := []memory.Fact{
		f(0, "Deploy to staging every Friday"),
		f(1, "deploy to staging every friday!"),
		f(2, "Deploy  to  Staging   Every Friday."),
		f(3, "deploy to staging every friday"),
		f(4, "unrelated one"),
		f(5, "another unrelated"),
		f(6, "third unrelated"),
		f(7, "fourth unrelated"),
		f(8, "fifth unrelated"),
		f(9, "sixth unrelated"),
	}
	got := finding(t, auditAgentHealth("a", facts), "duplicates")
	if got.Status != "fail" {
		t.Errorf("40%% duplicates: status=%q detail=%s, want fail", got.Status, got.Detail)
	}
	if len(got.Items) != 1 || !strings.Contains(got.Items[0], "4x") {
		t.Errorf("expected one group of 4, items=%v", got.Items)
	}
}

// TestHealthReport_StableAndVerdict drives the full pipeline twice over one
// seeded store and requires byte-identical JSON both times.
func TestHealthReport_StableAndVerdict(t *testing.T) {
	s := openHealthStore(t)
	ctx := context.Background()

	// A healthy-looking agent plus planted offenders.
	for i := 0; i < 12; i++ {
		txt := fmt.Sprintf("healthy fact %d: the pipeline rolls back automatically on failed probes", i)
		if err := s.Put(ctx, "clean-agent", txt); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ { // dumping agent
		if err := s.Put(ctx, "dump-agent", fmt.Sprintf("tiny %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	dumpFacts, _ := s.List("dump-agent")
	for i := range dumpFacts {
		dumpFacts[i].CreatedAt = time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		if err := s.UpdateFact("dump-agent", dumpFacts[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(ctx, "pin-agent", "Critical security policy: never ship keys to prod"); err != nil {
		t.Fatal(err)
	}
	pinFacts, _ := s.List("pin-agent")
	pinFacts[0].Weight = 0.02
	if err := s.UpdateFact("pin-agent", pinFacts[0]); err != nil {
		t.Fatal(err)
	}

	render := func() string {
		report := healthReport{Verdict: "ok"}
		for _, agent := range []string{"clean-agent", "dump-agent", "pin-agent"} {
			facts, err := s.List(agent)
			if err != nil {
				t.Fatal(err)
			}
			rep := auditAgentHealth(agent, facts)
			for _, fd := range rep.Findings {
				if severity(fd.Status) > severity(report.Verdict) {
					report.Verdict = fd.Status
				}
			}
			report.Agents = append(report.Agents, rep)
		}
		b, err := json.MarshalIndent(&report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	first := render()
	second := render()
	if first != second {
		t.Errorf("health JSON not stable between runs:\n--- run1\n%s\n--- run2\n%s", first, second)
	}

	var parsed healthReport
	if err := json.Unmarshal([]byte(first), &parsed); err != nil {
		t.Fatalf("JSON does not parse: %v", err)
	}
	if parsed.Verdict == "ok" {
		t.Errorf("verdict %q despite planted offences:\n%s", parsed.Verdict, first)
	}

	// The pin suggestion names the exact critical fact.
	var pinRep healthAgentReport
	for _, a := range parsed.Agents {
		if a.Agent == "pin-agent" {
			pinRep = a
		}
	}
	fd := finding(t, pinRep, "near-prune-critical")
	if fd.Status != "warn" || len(fd.Items) != 1 || !strings.Contains(fd.Items[0], "never ship keys") {
		t.Errorf("near-prune finding = %+v, want the pinned-suggestion item", fd)
	}

	// Human rendering stays deterministic too.
	var buf bytes.Buffer
	renderHealth(&buf, parsed)
	if buf.Len() == 0 || !strings.Contains(buf.String(), "Store health · verdict") {
		t.Error("human rendering lost its header")
	}
}
