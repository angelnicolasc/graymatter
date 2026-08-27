package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// Explain is a public surface (docs/api-stability.md), so its contract is
// tested from three angles here:
//
//  1. Parity  — the facts explain returns are exactly the facts Recall
//     returns, on identical stores. Explain is a read-out of the ranking,
//     never a second implementation.
//  2. Shape   — the ranks add up: a keyword-only store has no vector ranks,
//     recency ranks every live fact, and the fused score reproduces from the
//     published RRF arithmetic using the receipt's own numbers.
//  3. Determinism — the same query against the same store produces
//     byte-identical JSON. Determinism is the brand (docs claim it in
//     "Recall result ordering"); a receipt that wiggles between runs would
//     break every downstream golden diff and screenshot.

// explainCorpus is written in this order; the query overlaps two of the three
// facts so the ranking has something to separate.
var explainCorpus = []string{
	"deployments are signed with the team gpg key before publishing",
	"the staging cluster restarts every night at 02:00 utc",
	"release notes are drafted from merged pull requests",
}

const explainQuery = "gpg signing deployments"

// explainEpoch anchors the scripted clock. The tests compare two independent
// stores, and on Windows the wall clock ticks at ~15.6 ms — facts written in
// the same tick share CreatedAt and rank by random ULID, which flips across
// instances. A scripted clock advancing per write makes every fact's age
// unambiguous and the two stores identical.
var explainEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type explainClock struct{ offset time.Duration }

func (c *explainClock) now() time.Time { return explainEpoch.Add(c.offset) }

func newExplainStore(t *testing.T) (*Store, *explainClock, func()) {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock := &explainClock{}
	s.now = clock.now
	return s, clock, func() { _ = s.Close() }
}

func seedExplainCorpus(t *testing.T, s *Store, clock *explainClock) {
	t.Helper()
	ctx := context.Background()
	for _, text := range explainCorpus {
		clock.offset += time.Hour // strictly ordered CreatedAt, immune to clock ticks
		if err := s.Put(ctx, "explain-agent", text); err != nil {
			t.Fatalf("Put %q: %v", text, err)
		}
	}
}

// TestRecallExplain_MatchesRecall seeds two identical stores and requires the
// explain receipts to cover exactly the facts Recall returns, in order.
func TestRecallExplain_MatchesRecall(t *testing.T) {
	s1, clock1, close1 := newExplainStore(t)
	defer close1()
	seedExplainCorpus(t, s1, clock1)
	s2, clock2, close2 := newExplainStore(t)
	defer close2()
	seedExplainCorpus(t, s2, clock2)

	recalled, err := s1.Recall(context.Background(), "explain-agent", explainQuery, 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	receipts, err := s2.RecallExplain(context.Background(), "explain-agent", explainQuery, 5)
	if err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}

	if len(receipts) != len(recalled) {
		t.Fatalf("RecallExplain returned %d receipts, Recall returned %d facts", len(receipts), len(recalled))
	}
	for i, r := range receipts {
		if r.Text != recalled[i] {
			t.Errorf("receipt %d text = %q, want Recall's %q", i, r.Text, recalled[i])
		}
	}
}

// TestRecallExplain_RankArithmetic checks the receipt's own numbers reproduce
// the fused score: with a keyword-only embedder there is no vector rank, and
// fused = keyword/(k+keyword_rank) + recency/(k+recency_rank) at the default
// weights. If the receipt's numbers cannot rebuild the score, they describe
// nothing.
func TestRecallExplain_RankArithmetic(t *testing.T) {
	s, clock, close := newExplainStore(t)
	defer close()
	seedExplainCorpus(t, s, clock)

	receipts, err := s.RecallExplain(context.Background(), "explain-agent", explainQuery, 5)
	if err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}
	if len(receipts) == 0 {
		t.Fatal("RecallExplain returned no receipts for a matching query")
	}

	w := DefaultSignalWeights()
	matched := 0
	for i, r := range receipts {
		if r.Ranks.VectorRank != 0 {
			t.Errorf("receipt %d: vector rank %d on a keyword-only store, want 0 (signal absent)", i, r.Ranks.VectorRank)
		}
		if r.Ranks.KeywordRank > 0 {
			matched++
		}
		if r.Ranks.RecencyRank <= 0 {
			t.Errorf("receipt %d: recency rank %d, want >= 1 (recency ranks every live fact)", i, r.Ranks.RecencyRank)
		}
		if r.Ranks.K != 60 {
			t.Errorf("receipt %d: k = %v, want 60", i, r.Ranks.K)
		}
		// Rebuild the fused score from the receipt's own numbers. An absent
		// rank (0) contributes nothing — that is exactly how the fusion loop
		// treats a signal that did not rank the fact.
		want := 0.0
		if r.Ranks.KeywordRank > 0 {
			want += w.Keyword / (r.Ranks.K + float64(r.Ranks.KeywordRank))
		}
		if r.Ranks.RecencyRank > 0 {
			want += w.Recency / (r.Ranks.K + float64(r.Ranks.RecencyRank))
		}
		if r.Ranks.FusedScore != want {
			t.Errorf("receipt %d: fused score %v does not reproduce from ranks (want %v)", i, r.Ranks.FusedScore, want)
		}
		if r.Weight <= 0 || r.Weight > 1 {
			t.Errorf("receipt %d: weight %v outside [0,1]", i, r.Weight)
		}
		if r.AgeDays < 0 {
			t.Errorf("receipt %d: negative age %v", i, r.AgeDays)
		}
		if r.Provenance.FactID == "" {
			t.Errorf("receipt %d: empty fact_id", i)
		}
		if r.Provenance.SupersededBy != "" {
			t.Errorf("receipt %d: recalled fact carries a tombstone (%q)", i, r.Provenance.SupersededBy)
		}
	}
	if matched == 0 {
		t.Error("no receipt carries a keyword rank; the corpus query should match at least one fact")
	}
}

// TestRecallExplain_Provenance pins the provenance fields to the fact's
// stored metadata, which List is the ground truth for.
func TestRecallExplain_Provenance(t *testing.T) {
	s, clock, close := newExplainStore(t)
	defer close()
	seedExplainCorpus(t, s, clock)

	receipts, err := s.RecallExplain(context.Background(), "explain-agent", explainQuery, 5)
	if err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}
	facts, err := s.List("explain-agent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := make(map[string]Fact, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}

	for _, r := range receipts {
		f, ok := byID[r.Provenance.FactID]
		if !ok {
			t.Errorf("receipt fact_id %q not in store List", r.Provenance.FactID)
			continue
		}
		if r.Provenance.WrittenAt != f.CreatedAt {
			t.Errorf("receipt %q written_at %v, want stored CreatedAt %v", r.Provenance.FactID, r.Provenance.WrittenAt, f.CreatedAt)
		}
		if r.Weight != f.Weight {
			t.Errorf("receipt %q weight %v, want stored weight %v", r.Provenance.FactID, r.Weight, f.Weight)
		}
		if r.Text != f.Text {
			t.Errorf("receipt %q text drifted from stored text", r.Provenance.FactID)
		}
	}
}

// TestRecallExplain_OverheadUnderBudget is the published acceptance criterion
// ("cost of --explain over normal recall: < 5 ms"): the receipts must be a
// read-out of the ranking's own state, never a second scoring pass. Measured
// as the delta of per-method minima across interleaved runs — the minimum is
// stable against scheduler noise where a mean is not.
func TestRecallExplain_OverheadUnderBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("times full recalls over a seeded store; skipped in -short")
	}

	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	topics := []string{"deploy", "database", "cache", "auth", "billing", "search", "queue", "logging", "metrics", "oncall"}
	for i := 0; i < 3000; i++ {
		text := "fact covering the deployment and runbook procedures for the staging cluster"
		if err := s.Put(ctx, "explain-bench", text+" with payload "+topics[i%len(topics)]+" "+fmt.Sprint(i%97)); err != nil {
			t.Fatal(err)
		}
	}

	const runs = 7
	minOf := func(fns []func() error) time.Duration {
		best := time.Hour
		for _, fn := range fns {
			start := time.Now()
			if err := fn(); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	var recalls, explains []func() error
	for i := 0; i < runs; i++ {
		q := "deployment runbook procedures " + fmt.Sprint(i)
		recalls = append(recalls, func() error {
			_, err := s.Recall(ctx, "explain-bench", q, 8)
			return err
		})
		explains = append(explains, func() error {
			_, err := s.RecallExplain(ctx, "explain-bench", q, 8)
			return err
		})
	}

	recallMin := minOf(recalls)
	explainMin := minOf(explains)
	overhead := explainMin - recallMin
	t.Logf("recall min %v · explain min %v · overhead %v", recallMin, explainMin, overhead)
	if overhead > 5*time.Millisecond {
		t.Errorf("explain overhead %v exceeds the published 5 ms budget", overhead)
	}
}

// TestRecallExplain_Deterministic is the byte-identity gate: the same query
// against the same store must marshal to identical bytes on every call.
// (RecallExplain's own touches only move AccessedAt, which no receipt field
// reads, so repeats on one store are valid.)
func TestRecallExplain_Deterministic(t *testing.T) {
	s, clock, close := newExplainStore(t)
	defer close()
	seedExplainCorpus(t, s, clock)

	run := func() string {
		receipts, err := s.RecallExplain(context.Background(), "explain-agent", explainQuery, 5)
		if err != nil {
			t.Fatalf("RecallExplain: %v", err)
		}
		b, err := json.Marshal(receipts)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	first := run()
	for i := 0; i < 9; i++ {
		if got := run(); got != first {
			t.Fatalf("explain output not byte-identical on run %d:\nfirst: %s\nrun:   %s", i+2, first, got)
		}
	}
}

// TestRecallExplain_EmptyAndMissing verifies the empty paths: an agent with no
// facts returns an empty result, not an error, and so does an unknown agent.
func TestRecallExplain_EmptyAndMissing(t *testing.T) {
	s, clock, close := newExplainStore(t)
	defer close()

	receipts, err := s.RecallExplain(context.Background(), "explain-agent", explainQuery, 5)
	if err != nil {
		t.Fatalf("unknown agent: %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("unknown agent returned %d receipts, want 0", len(receipts))
	}

	seedExplainCorpus(t, s, clock)
	receipts, err = s.RecallExplain(context.Background(), "other-agent", explainQuery, 5)
	if err != nil {
		t.Fatalf("empty agent: %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("empty agent returned %d receipts, want 0", len(receipts))
	}
}

// TestRecallExplain_SameStateAsRecall pins the side-effect contract: explain
// and a plain recall leave the same access-metadata footprints, because an
// explained recall is a recall. Two identical stores: one recalled, one
// explained — List must agree everywhere.
func TestRecallExplain_SameStateAsRecall(t *testing.T) {
	s1, clock1, close1 := newExplainStore(t)
	defer close1()
	seedExplainCorpus(t, s1, clock1)
	s2, clock2, close2 := newExplainStore(t)
	defer close2()
	seedExplainCorpus(t, s2, clock2)

	if _, err := s1.Recall(context.Background(), "explain-agent", explainQuery, 3); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if _, err := s2.RecallExplain(context.Background(), "explain-agent", explainQuery, 3); err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}

	a, err := s1.List("explain-agent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	b, err := s2.List("explain-agent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// The two stores are seeded at different wall-clock instants, so compare
	// the access footprints per fact text: how many times each fact was
	// touched. That is the side-effect under test.
	countBy := func(facts []Fact) map[string]int {
		m := make(map[string]int, len(facts))
		for _, f := range facts {
			m[f.Text] = f.AccessCount
		}
		return m
	}
	if !reflect.DeepEqual(countBy(a), countBy(b)) {
		t.Errorf("explain left different access counts than recall:\nrecall:  %v\nexplain: %v", countBy(a), countBy(b))
	}
}
