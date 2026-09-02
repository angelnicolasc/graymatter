package memory

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// Reconciles the two latency instruments, and splits the cost.
//
// Two numbers were reported for 3000 facts and they disagreed by ~2.2×: 25 ms
// from TestRecallLatencyInProcess and 56 ms from the scale curve. They were not
// measuring the same call — the curve asked for the whole pool as top-k so it
// could score A over the full ranking, while the latency test asked for 8. This
// separates them, and separates the two costs inside a recall:
//
//	load   listLite pulls every fact for the agent, before any scoring
//	score  keywordScore re-tokenises each of those facts, per query
//
// The split matters for the inverted index: an index removes the scoring pass
// but not the load, so if the load dominates, the index alone will not reach a
// latency target.
//
// Gated with the rest of the measurements in this package. It asserts nothing
// — it is a diagnostic that prints a split — but it builds a 3000-fact store
// and times 60 recalls, and under -race in the blocking suite that is minutes
// of a budget shared with tests that do assert something.
//
// Run it as: GRAYMATTER_SCALE_GATE=1 go test -run TestLatencyBreakdown -v
// ./pkg/memory  — without -race, which distorts the split it reports.
func TestLatencyBreakdown(t *testing.T) {
	if os.Getenv("GRAYMATTER_SCALE_GATE") != "1" {
		t.Skip("set GRAYMATTER_SCALE_GATE=1 to run the latency breakdown")
	}
	if testing.Short() {
		t.Skip("builds a 3000-fact store; skipped in -short")
	}
	ctx := context.Background()
	const size = 3000

	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 8760 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	for i := 0; i < size; i++ {
		txt := fmt.Sprintf("the %s service runs %d replicas in region %d",
			[]string{"catalog", "billing", "identity", "shipping", "search"}[i%5], i, i%7)
		if err := s.Put(ctx, "lat", txt); err != nil {
			t.Fatal(err)
		}
	}
	const q = "which service runs the most replicas?"

	timeIt := func(n int, f func()) (p50, p99 time.Duration) {
		xs := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			f()
			xs = append(xs, time.Since(start))
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
		return xs[len(xs)/2], xs[int(0.99*float64(len(xs)-1))]
	}

	// Warm.
	for i := 0; i < 5; i++ {
		_, _ = s.Recall(ctx, "lat", q, 8)
	}

	loadP50, loadP99 := timeIt(60, func() {
		if _, err := s.listLite("lat"); err != nil {
			t.Fatal(err)
		}
	})
	k8P50, k8P99 := timeIt(60, func() {
		if _, err := s.Recall(ctx, "lat", q, 8); err != nil {
			t.Fatal(err)
		}
	})
	kAllP50, kAllP99 := timeIt(20, func() {
		if _, err := s.RecallExplain(ctx, "lat", q, size); err != nil {
			t.Fatal(err)
		}
	})

	t.Logf("%d facts:", size)
	t.Logf("  listLite alone (load, no scoring)  : p50 %v · p99 %v",
		loadP50.Round(time.Microsecond), loadP99.Round(time.Microsecond))
	t.Logf("  Recall top-8                       : p50 %v · p99 %v",
		k8P50.Round(time.Microsecond), k8P99.Round(time.Microsecond))
	t.Logf("  RecallExplain top-%d (what the curve measured): p50 %v · p99 %v",
		size, kAllP50.Round(time.Microsecond), kAllP99.Round(time.Microsecond))
	if k8P50 > 0 {
		t.Logf("  the load is %.0f%% of a top-8 recall", float64(loadP50)/float64(k8P50)*100)
		t.Logf("  asking for the whole pool costs %.1fx over top-8", float64(kAllP50)/float64(k8P50))
	}
}
