package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// testConsolidateCfg is a minimal ConsolidateConfig for unit tests.
type testConsolidateCfg struct {
	threshold int
	halfLife  time.Duration
	llm       string
	apiKey    string
	model     string
	ollamaURL string
	ollamaMdl string
}

func (c *testConsolidateCfg) GetAnthropicAPIKey() string        { return c.apiKey }
func (c *testConsolidateCfg) GetConsolidateLLM() string         { return c.llm }
func (c *testConsolidateCfg) GetConsolidateModel() string       { return c.model }
func (c *testConsolidateCfg) GetConsolidateThreshold() int      { return c.threshold }
func (c *testConsolidateCfg) GetDecayHalfLife() time.Duration   { return c.halfLife }
func (c *testConsolidateCfg) GetOllamaURL() string              { return c.ollamaURL }
func (c *testConsolidateCfg) GetOllamaConsolidateModel() string { return c.ollamaMdl }

func defaultTestCfg() *testConsolidateCfg {
	return &testConsolidateCfg{
		threshold: 100,
		halfLife:  720 * time.Hour,
		llm:       "",
	}
}

// TestConsolidate_DecayReducesWeight verifies the exponential decay math.
// After one half-life, a fact's weight must be within 1% of 0.5.
func TestConsolidate_DecayReducesWeight(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Insert a fact and back-date its AccessedAt to exactly one half-life ago.
	if err := s.Put(ctx, "decay-agent", "Some decayable fact."); err != nil {
		t.Fatalf("Put: %v", err)
	}
	facts, _ := s.List("decay-agent")
	if len(facts) == 0 {
		t.Fatal("expected 1 fact")
	}
	halfLife := 720 * time.Hour
	facts[0].AccessedAt = time.Now().UTC().Add(-halfLife)
	_ = s.UpdateFact("decay-agent", facts[0])

	cfg := &testConsolidateCfg{threshold: 9999, halfLife: halfLife}
	if err := s.Consolidate(ctx, "decay-agent", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	updated, _ := s.List("decay-agent")
	if len(updated) == 0 {
		t.Fatal("fact was pruned unexpectedly")
	}
	got := updated[0].Weight
	// e^(-ln2 * 1) = 0.5 exactly; allow 1% tolerance for clock jitter.
	if math.Abs(got-0.5) > 0.01 {
		t.Errorf("weight after one half-life = %.4f, want ≈0.5", got)
	}
}

// TestConsolidate_PrunesDead verifies that facts with weight < 0.01 are deleted.
func TestConsolidate_PrunesDead(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "prune-agent", "This fact will die.")
	_ = s.Put(ctx, "prune-agent", "This fact survives.")

	facts, _ := s.List("prune-agent")
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}

	// Drive one fact's weight to near-zero and AccessedAt far in the past.
	for i, f := range facts {
		if f.Text == "This fact will die." {
			facts[i].Weight = 0.005
			facts[i].AccessedAt = time.Now().UTC().Add(-9999 * time.Hour)
			_ = s.UpdateFact("prune-agent", facts[i])
		}
	}

	cfg := &testConsolidateCfg{threshold: 9999, halfLife: 1 * time.Hour}
	if err := s.Consolidate(ctx, "prune-agent", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	remaining, _ := s.List("prune-agent")
	for _, f := range remaining {
		if f.Text == "This fact will die." {
			t.Error("dead fact was not pruned")
		}
	}
	found := false
	for _, f := range remaining {
		if f.Text == "This fact survives." {
			found = true
		}
	}
	if !found {
		t.Error("surviving fact was incorrectly pruned")
	}
}

// TestMaybeConsolidate_BelowThreshold verifies no-op when count < threshold.
func TestMaybeConsolidate_BelowThreshold(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "threshold-agent", "Fact one.")
	_ = s.Put(ctx, "threshold-agent", "Fact two.")

	// Threshold is 50; we have 2 facts. Consolidation must not run.
	cfg := &testConsolidateCfg{threshold: 50, halfLife: 720 * time.Hour}
	if err := s.MaybeConsolidate(ctx, "threshold-agent", cfg); err != nil {
		t.Fatalf("MaybeConsolidate: %v", err)
	}

	facts, _ := s.List("threshold-agent")
	if len(facts) != 2 {
		t.Errorf("expected 2 facts after no-op, got %d", len(facts))
	}
}

// TestMaybeConsolidate_AtThreshold triggers consolidation when count == threshold.
func TestMaybeConsolidate_AtThreshold(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const n = 5
	for i := 0; i < n; i++ {
		_ = s.Put(ctx, "at-thresh", fmt.Sprintf("Fact number %d", i))
	}
	// Set all weights near-zero so pruning will fire.
	facts, _ := s.List("at-thresh")
	for _, f := range facts {
		f.Weight = 0.001
		f.AccessedAt = time.Now().UTC().Add(-9999 * time.Hour)
		_ = s.UpdateFact("at-thresh", f)
	}

	cfg := &testConsolidateCfg{threshold: n, halfLife: 1 * time.Hour}
	if err := s.MaybeConsolidate(ctx, "at-thresh", cfg); err != nil {
		t.Fatalf("MaybeConsolidate at threshold: %v", err)
	}

	// All facts had weight ~0 → all should be pruned.
	remaining, _ := s.List("at-thresh")
	if len(remaining) != 0 {
		t.Errorf("expected 0 facts after prune, got %d", len(remaining))
	}
}

// TestLaunchAsyncConsolidate_SemaphoreCapacity verifies that triggers beyond
// MaxAsyncConsolidations are dropped (non-blocking), not queued indefinitely.
func TestLaunchAsyncConsolidate_SemaphoreCapacity(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:                dir,
		Embedder:               nil,
		DecayHalfLife:          720 * time.Hour,
		MaxAsyncConsolidations: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	_ = s.Put(ctx, "sema-agent", "Fact for semaphore test.")

	var completed int64
	// Fill the semaphore manually so all Launch calls are dropped.
	s.sema <- struct{}{}

	cfg := &testConsolidateCfg{threshold: 1, halfLife: 720 * time.Hour}
	const launches = 10
	var wg sync.WaitGroup
	for i := 0; i < launches; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.LaunchAsyncConsolidate("sema-agent", cfg)
			atomic.AddInt64(&completed, 1)
		}()
	}
	wg.Wait()

	// All Launch calls must return without blocking.
	if completed != launches {
		t.Errorf("expected %d completed launches, got %d", launches, completed)
	}
	// Release the semaphore slot.
	<-s.sema
}

// TestConsolidate_Empty is a no-op safety check: empty store must not error.
func TestConsolidate_Empty(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	cfg := defaultTestCfg()
	if err := s.Consolidate(context.Background(), "ghost-agent", cfg); err != nil {
		t.Errorf("Consolidate on empty store should not error: %v", err)
	}
}

// TestConsolidate_SummarizeGuard verifies that with no LLM configured the
// summarisation step is skipped gracefully (no data loss, no error).
func TestConsolidate_SummarizeGuard(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const n = 10
	for i := 0; i < n; i++ {
		_ = s.Put(ctx, "noLLM-agent", fmt.Sprintf("Important fact %d", i))
	}

	// threshold=n → summarise would fire, but llm="" → should be skipped.
	cfg := &testConsolidateCfg{threshold: n, halfLife: 720 * time.Hour, llm: ""}
	if err := s.Consolidate(ctx, "noLLM-agent", cfg); err != nil {
		t.Fatalf("Consolidate with no LLM: %v", err)
	}
	// All facts should still exist (no summarisation happened).
	facts, _ := s.List("noLLM-agent")
	if len(facts) == 0 {
		t.Error("all facts vanished despite no LLM summarisation running")
	}
}

// TestConsolidate_PutBeforeDelete verifies the atomic contract: when
// summarisation succeeds, old facts are deleted; if Put fails, nothing is lost.
// We test the happy path here (Put succeeds, batch is deleted).
func TestConsolidate_PutBeforeDelete(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const n = 20
	for i := 0; i < n; i++ {
		_ = s.Put(ctx, "atomic-agent", fmt.Sprintf("Batch fact %d", i))
	}

	// Weight all facts identically so all qualify for the summarise batch.
	// With llm="" the summarise step is skipped — nothing deleted.
	cfg := &testConsolidateCfg{threshold: n, halfLife: 720 * time.Hour, llm: ""}
	if err := s.Consolidate(ctx, "atomic-agent", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	facts, _ := s.List("atomic-agent")
	// Without a real LLM no summarisation fires; original n facts intact.
	if len(facts) != n {
		t.Errorf("expected %d facts, got %d", n, len(facts))
	}
}

// TestConsolidate_OllamaUnreachableReports covers the misconfiguration that
// used to be invisible. Ollama is a supported embedding backend, so setting
// ConsolidateLLM="ollama" looks reasonable — and when nothing answers on that
// URL, consolidation must say so through OnConsolidateError instead of
// quietly doing nothing forever. The cycle itself still runs decay and
// pruning, and the batch stays live: a broken summariser degrades to the
// exact behaviour of ConsolidateLLM="".
func TestConsolidate_OllamaUnreachableReports(t *testing.T) {
	var reported []error
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
		OnConsolidateError: func(_ string, err error) {
			reported = append(reported, err)
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	// Port 1 on localhost is reserved and refuses connections immediately.
	cfg := &testConsolidateCfg{threshold: 3, halfLife: 720 * time.Hour, llm: "ollama",
		ollamaURL: "http://127.0.0.1:1"}
	for i := 0; i < 5; i++ { // over the threshold, so summarisation is attempted
		if err := s.Put(ctx, "ollama-agent", fmt.Sprintf("observation number %d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	if err := s.Consolidate(ctx, "ollama-agent", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	if len(reported) == 0 {
		t.Error("an unreachable consolidation LLM reported nothing")
	}
	for _, e := range reported {
		if errors.Is(e, ErrConsolidateLLMUnsupported) {
			t.Errorf("sentinel ErrConsolidateLLMUnsupported is retired but was returned: %v", e)
		}
	}
	facts, _ := s.List("ollama-agent")
	if len(facts) != 5 {
		t.Errorf("fallback must leave the store untouched; got %d facts, want 5", len(facts))
	}
}

// TestConsolidate_DisabledLLMReportsNothing is the other half: an empty
// ConsolidateLLM means summarisation is off by choice, not broken, and must
// stay silent.
func TestConsolidate_DisabledLLMReportsNothing(t *testing.T) {
	var reported []error
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
		OnConsolidateError: func(_ string, err error) {
			reported = append(reported, err)
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	cfg := &testConsolidateCfg{threshold: 3, halfLife: 720 * time.Hour, llm: ""}
	for i := 0; i < 5; i++ {
		if err := s.Put(ctx, "quiet-agent", fmt.Sprintf("observation number %d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	if err := s.Consolidate(ctx, "quiet-agent", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(reported) != 0 {
		t.Errorf("consolidation with the LLM disabled reported errors: %v", reported)
	}
}

// TestConsolidate_DecayIsIdempotent is the regression test for a decay model
// that did not decay on the schedule it documented.
//
// Consolidate computed weight *= exp(-lambda * hoursSinceAccessed) using the
// FULL time since last access on every run, rather than the time since the
// last decay. Nothing tracked that a fact had already been decayed, so each
// cycle re-applied the whole elapsed period. A fact one half-life stale went
// 0.5, 0.25, 0.125, 0.0625 across four cycles that ran in the same
// millisecond — the half-life was effectively "per consolidation run", not
// per 30 days, and with AsyncConsolidate on a busy agent could prune a
// month-old fact in minutes.
func TestConsolidate_DecayIsIdempotent(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.Put(ctx, "idem", "a fact nobody recalls"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	facts, err := s.List("idem")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	f := facts[0]
	// Exactly one half-life since last access.
	f.AccessedAt = time.Now().UTC().Add(-720 * time.Hour)
	f.CreatedAt = f.AccessedAt
	if err := s.UpdateFact("idem", f); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	cfg := defaultTestCfg()
	for run := 1; run <= 5; run++ {
		if err := s.Consolidate(ctx, "idem", cfg); err != nil {
			t.Fatalf("Consolidate run %d: %v", run, err)
		}
		got, err := s.List("idem")
		if err != nil {
			t.Fatalf("List run %d: %v", run, err)
		}
		if len(got) == 0 {
			t.Fatalf("fact was pruned after %d consolidation runs; at one half-life "+
				"stale its weight should be 0.5 no matter how often consolidation runs", run)
		}
		if math.Abs(got[0].Weight-0.5) > 0.01 {
			t.Errorf("after %d consolidation runs weight is %.6f, want ~0.5. "+
				"Elapsed time has not changed between runs, so the weight must not either.",
				run, got[0].Weight)
		}
	}
}

// TestConsolidate_DecayStillTracksElapsedTime guards the fix from the obvious
// overcorrection: making decay idempotent must not make it stop happening.
func TestConsolidate_DecayStillTracksElapsedTime(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.Put(ctx, "elapsed", "a fact nobody recalls"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	cfg := defaultTestCfg()

	for _, tc := range []struct {
		halfLives int
		want      float64
	}{{1, 0.5}, {2, 0.25}, {3, 0.125}} {
		facts, err := s.List("elapsed")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		f := facts[0]
		f.AccessedAt = time.Now().UTC().Add(-time.Duration(tc.halfLives) * 720 * time.Hour)
		if err := s.UpdateFact("elapsed", f); err != nil {
			t.Fatalf("UpdateFact: %v", err)
		}

		if err := s.Consolidate(ctx, "elapsed", cfg); err != nil {
			t.Fatalf("Consolidate: %v", err)
		}
		got, err := s.List("elapsed")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) == 0 {
			t.Fatalf("fact pruned at %d half-lives, expected weight ~%.3f", tc.halfLives, tc.want)
		}
		if math.Abs(got[0].Weight-tc.want) > 0.01 {
			t.Errorf("at %d half-lives stale: weight %.6f, want ~%.3f",
				tc.halfLives, got[0].Weight, tc.want)
		}
	}
}

// TestConsolidate_DecayNeverRaisesWeight covers the interaction with the
// tombstones from ADR-007. A superseded fact has its weight zeroed so ordinary
// pruning collects it; decay recomputing weight from elapsed time must never
// hand that weight back, or a retired fact would sit in the store forever.
func TestConsolidate_DecayNeverRaisesWeight(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.Put(ctx, "retired", "a fact the agent retired"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	facts, err := s.List("retired")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	f := facts[0]
	f.SupersededBy = SupersededByAgent
	f.Weight = 0
	// Freshly accessed: elapsed time alone would compute a weight near 1.0.
	f.AccessedAt = time.Now().UTC()
	if err := s.UpdateFact("retired", f); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	if err := s.Consolidate(ctx, "retired", defaultTestCfg()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	got, err := s.List("retired")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) > 0 && got[0].Weight > 0.01 {
		t.Errorf("consolidation raised a retired fact's weight from 0 to %.6f; "+
			"it would never be pruned", got[0].Weight)
	}
}
