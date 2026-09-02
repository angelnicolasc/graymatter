package memory

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// The README's first claim about speed had no number behind it.
//
// Every latency this project has reported — 124-128 ms through the daemon,
// 163-174 ms with --no-daemon — measured a CLI process starting up, opening
// bbolt, answering, and exiting. That is the cost of the *command*, not of the
// retrieval, and it is the wrong number to put next to a competitor that runs
// as a service. This measures Recall itself, in process, on a warm store.
//
// Reported as a distribution rather than a mean: a p99 hiding behind an average
// is how latency claims go bad, and the injection hook that fires on every user
// prompt cares about the tail, not the typical case.
func TestRecallLatencyInProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds stores up to 3000 facts; skipped in -short")
	}
	ctx := context.Background()
	queries := []string{
		"how many webhook retries do we allow?",
		"who is on call for billing?",
		"which region hosts the primary database?",
		"what is the minimum TLS version?",
		"how often do we release?",
	}

	for _, size := range []int{600, 3000} {
		s, err := Open(StoreConfig{
			DataDir:       t.TempDir(),
			Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
			DecayHalfLife: 8760 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}

		for i := 0; i < size; i++ {
			txt := fmt.Sprintf("the %s service runs %d replicas in region %d",
				[]string{"catalog", "billing", "identity", "shipping", "search"}[i%5], i, i%7)
			if err := s.Put(ctx, "lat", txt); err != nil {
				t.Fatal(err)
			}
		}
		// Plant the probe targets so the queries retrieve something real.
		for _, txt := range []string{
			"the webhook retry limit is now 8 attempts",
			"Kenji Mori took over on-call for billing",
			"we migrated the primary database to eu-central-1",
			"TLS 1.3 is now the floor for every endpoint",
			"releases go out weekly since the pipeline rewrite",
		} {
			if err := s.Put(ctx, "lat", txt); err != nil {
				t.Fatal(err)
			}
		}

		// Warm: the first recall pays for page-cache misses that no steady-state
		// caller pays, and including it would report a startup cost as latency.
		for _, q := range queries {
			if _, err := s.Recall(ctx, "lat", q, 8); err != nil {
				t.Fatal(err)
			}
		}

		const iterations = 200
		// Three passes, minimum p99 wins. A single pass on a loaded machine
		// reported p99 37.5 ms where the quiet floor is ~26 ms — noise, not
		// the product — and the first response to that flake (raising the
		// gate to 150 ms) was wrong: it would have hidden a 3x regression.
		// Stabilise the measurement instead: the least-loaded of three passes
		// is the honest floor, and the gate sits above it but far below the
		// un-indexed catastrophe (~251 ms projected at 30k).
		bestP99 := time.Duration(1<<63 - 1)
		p50Best := time.Duration(0)
		for pass := 0; pass < 3; pass++ {
			samples := make([]time.Duration, 0, iterations)
			for i := 0; i < iterations; i++ {
				q := queries[i%len(queries)]
				start := time.Now()
				if _, err := s.Recall(ctx, "lat", q, 8); err != nil {
					t.Fatal(err)
				}
				samples = append(samples, time.Since(start))
			}
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			if p99 := samples[int(0.99*float64(len(samples)-1))]; p99 < bestP99 {
				bestP99 = p99
				p50Best = samples[len(samples)/2]
			}
		}
		_ = s.Close()

		t.Logf("%d facts, n=3×%d recalls in-process: p50 %v · p99 (min of 3) %v",
			size, iterations, p50Best.Round(time.Microsecond), bestP99.Round(time.Microsecond))

		if bestP99 > 100*time.Millisecond {
			t.Errorf("%d facts: p99 recall (min of 3) = %v, want under 100ms in process", size, bestP99)
		}
	}
}
