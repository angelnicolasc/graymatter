package memory

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// ── touchFacts read-modify-write ────────────────────────────────────────────
//
// The RMW rewrite exists because the old write-back marshalled the
// recall-time snapshot over the stored fact, stomping any mutation that
// landed between List and the write. These tests attack exactly that window
// with real concurrency; run with -race in CI.

// TestTouchFacts_RMWPreservesConcurrentWeightChange: while recalls touch a
// fact's access metadata, a consolidation-style decay lowers its weight. The
// final weight must be the lowered one — the access bump rides on top of the
// current stored fact, never over it.
func TestTouchFacts_RMWPreservesConcurrentWeightChange(t *testing.T) {
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
	const fact = "the deployment freeze window runs through january 2nd"
	if err := s.Put(ctx, "rmw", fact); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Goroutine A: recalls that touch the fact's access metadata.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.Recall(ctx, "rmw", "deployment freeze window", 5)
			}
		}
	}()

	// Goroutine B: a decay-style UpdateFact lands mid-flight. The old
	// snapshot write-back could overwrite this with the stale weight.
	time.Sleep(20 * time.Millisecond)
	facts, err := s.List("rmw")
	if err != nil || len(facts) != 1 {
		t.Fatalf("list: %v (%d facts)", err, len(facts))
	}
	f := facts[0]
	f.Weight = 0.42
	if err := s.UpdateFact("rmw", f); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	after, err := s.List("rmw")
	if err != nil || len(after) != 1 {
		t.Fatalf("final list: %v (%d facts)", err, len(after))
	}
	if after[0].Weight != 0.42 {
		t.Errorf("concurrent weight change was stomped: weight = %v, want 0.42", after[0].Weight)
	}
	if after[0].AccessCount == 0 {
		t.Error("the concurrent access bumps were lost — RMW must keep both effects")
	}
}

// ── vector-path explain ─────────────────────────────────────────────────────
//
// Every explain test so far ran keyword-only. The stub embedder exercises the
// vector signal: ranks must appear, receipts must match a plain recall, and
// the arithmetic must reproduce with three signals.

// TestRecallExplain_VectorPath: with a real embedder wired, receipts carry
// vector ranks, the fused score reproduces from all three signals, and the
// facts match a plain recall exactly.
func TestRecallExplain_VectorPath(t *testing.T) {
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      &stubEmbedder{vec: []float32{1, 2}},
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	for _, text := range []string{
		"the api gateway rate limit is 60 requests per minute",
		"postgres runs on the db-01 box with max_conns 200",
		"staging restarts every night at 02:00 utc",
	} {
		if err := s.Put(ctx, "vec", text); err != nil {
			t.Fatal(err)
		}
	}

	recalled, err := s.Recall(ctx, "vec", "rate limit gateway", 3)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := s.RecallExplain(ctx, "vec", "rate limit gateway", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != len(recalled) {
		t.Fatalf("vector path: %d receipts vs %d recalled facts", len(receipts), len(recalled))
	}
	w := DefaultSignalWeights()
	matched := 0
	for i, r := range receipts {
		if r.Text != recalled[i] {
			t.Errorf("position %d: %q != %q", i, r.Text, recalled[i])
		}
		if r.Ranks.VectorRank <= 0 {
			t.Errorf("receipt %d: vector rank %d, want >= 1 when an embedder is wired", i, r.Ranks.VectorRank)
		}
		if r.Ranks.KeywordRank > 0 {
			matched++
		}
		// Rebuild the fused score from whatever ranks the receipt carries: a
		// rank of 0 means the signal did not rank the fact and contributes
		// nothing. Recency always ranks.
		want := 0.0
		if r.Ranks.VectorRank > 0 {
			want += w.Vector / (r.Ranks.K + float64(r.Ranks.VectorRank))
		}
		if r.Ranks.KeywordRank > 0 {
			want += w.Keyword / (r.Ranks.K + float64(r.Ranks.KeywordRank))
		}
		if r.Ranks.RecencyRank > 0 {
			want += w.Recency / (r.Ranks.K + float64(r.Ranks.RecencyRank))
		}
		if r.Ranks.FusedScore != want {
			t.Errorf("receipt %d: fused %v does not reproduce from its signals (want %v)", i, r.Ranks.FusedScore, want)
		}
	}
	if matched == 0 {
		t.Error("no receipt carries a keyword rank; the query matches at least one fact")
	}
}

// TestRecallExplain_VectorPathDeterministic: same store, same query, the
// receipt bytes are identical across calls with the vector signal live.
func TestRecallExplain_VectorPathDeterministic(t *testing.T) {
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      &stubEmbedder{vec: []float32{1, 2}},
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if err := s.Put(ctx, "vec", fmt.Sprintf("fact %d about the api gateway rate limit", i)); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.RecallExplain(ctx, "vec", "gateway rate limit", 5)
	if err != nil {
		t.Fatal(err)
	}
	blob := func(rs []RecallReceipt) string { return fmt.Sprintf("%v", rs) }
	want := blob(first)
	for i := 0; i < 5; i++ {
		got, err := s.RecallExplain(ctx, "vec", "gateway rate limit", 5)
		if err != nil {
			t.Fatal(err)
		}
		if blob(got) != want {
			t.Fatalf("vector-path explain is not deterministic on run %d", i+2)
		}
	}
}

// ── same-clock-tick determinism ─────────────────────────────────────────────
//
// Windows wall clocks tick at ~15.6 ms, so facts written in the same tick
// share CreatedAt. The recency ranking derives from list order (ID asc within
// equal stamps), which must be (a) stable across repeated calls per store and
// (b) total — every fact gets a unique recency rank.

// TestRecall_SameTickFactsDeterministicOrder: a frozen clock makes every fact
// share CreatedAt; the order must still be total and stable across repeated
// calls, and recency ranks must follow fact ID ascending.
func TestRecall_SameTickFactsDeterministicOrder(t *testing.T) {
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	frozen := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return frozen }

	ctx := context.Background()
	for i := 0; i < 12; i++ {
		if err := s.Put(ctx, "tick", fmt.Sprintf("same-tick fact %02d about deployment runbooks", i)); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.Recall(ctx, "tick", "deployment runbooks", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 12 {
		t.Fatalf("got %d facts, want 12", len(first))
	}

	// Stable across repeated calls on the same store.
	for round := 0; round < 5; round++ {
		again, err := s.Recall(ctx, "tick", "deployment runbooks", 12)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("same-tick order changed on round %d:\n%v\n%v", round+2, first, again)
		}
	}

	// Total order: recency rank = position in the newest-first list; with
	// equal stamps the list is ID-ascending, so the full recall must be in
	// ID-ascending order too (all scores tie; the tiebreak is the ID).
	receipts, err := s.RecallExplain(ctx, "tick", "deployment runbooks", 12)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(receipts))
	for _, r := range receipts {
		ids = append(ids, r.Provenance.FactID)
		if r.Ranks.RecencyRank != len(ids) {
			t.Errorf("receipt %s recency rank %d, want its list position %d (ranks must be total)", r.Provenance.FactID, r.Ranks.RecencyRank, len(ids))
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("same-tick facts not in ID-ascending order: %v", ids)
	}
}
