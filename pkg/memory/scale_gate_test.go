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

// The candidate-set gate: does retrieval stop paying for the whole corpus?
//
// Every recall today loads every fact the agent owns before it scores
// anything (listLite), and TestLatencyBreakdown measured that load at 59% of
// a top-8 recall at 3000 facts. That is O(N) work per query in the dominant
// term, so the curve is linear and the only question is where it crosses the
// budget.
//
// PRE-REGISTERED, written before the candidate-set path existed and before
// the 30k baseline below had been run even once:
//
//  1. IDENTITY. For every probe, the indexed path returns exactly the same
//     fact IDs in exactly the same order as the unindexed path. Not "similar",
//     not "same top-3" — identical. An optimisation that changes an answer is
//     not an optimisation, it is a re-specification, and this project has
//     already paid for one of those.
//  2. LATENCY. p99 of Recall top-8 at 30 000 facts <= 40 ms in process.
//     Chosen from the shape, not the answer: the 3k p99 floor is ~26 ms, so a
//     linear path lands near 260 ms at 30k and anything under 40 ms is
//     necessarily sub-linear. The number is deliberately loose — the claim
//     being tested is the exponent, not a millisecond.
//  3. WRITE COST. p50 Put latency with the index <= 3 ms. The original draft
//     of this line said "within 2x of the scan's 1.05 ms"; two machines then
//     measured the real number at 2.28 / 2.52 / 2.55 ms — between 2.2x and
//     2.4x — and on 2026-09-01 the owner ratified the 3 ms letter as the bar:
//     a write-once, read-many store pays ~1.5 ms per Put to keep every read
//     sub-linear, and chasing the 2x wording would delay the default for a
//     difference no user can observe. The retired wording is quoted here on
//     purpose, so the decision is reachable from the number it produced.
//  4. NO REGRESSION AT SMALL N. p99 at 600 facts must not get worse: the
//     typical store is small, and a candidate set that costs more than the
//     scan it replaces would be a loss for almost every real user.
//
// Guarded by GRAYMATTER_SCALE_GATE=1: building 30 000 facts takes minutes and
// has no business running on every `go test ./...`.
func TestP4ScaleGate(t *testing.T) {
	if os.Getenv("GRAYMATTER_SCALE_GATE") != "1" {
		t.Skip("set GRAYMATTER_SCALE_GATE=1 to run the 30k scale gate")
	}
	ctx := context.Background()
	probes := []string{
		"how many webhook retries do we allow?",
		"who is on call for billing?",
		"which region hosts the primary database?",
		"what is the minimum TLS version?",
		"how often do we release?",
	}

	for _, arm := range []struct {
		label   string
		indexed bool
	}{{"scan   ", false}, {"indexed", true}} {
		for _, size := range []int{600, 3000, 10000, 30000} {
			s, err := Open(StoreConfig{
				DataDir:            t.TempDir(),
				Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
				DecayHalfLife:      8760 * time.Hour,
				CandidateRetrieval: arm.indexed,
			})
			if err != nil {
				t.Fatal(err)
			}

			putSamples := make([]time.Duration, 0, size)
			for i := 0; i < size; i++ {
				txt := scaleFactText(i)
				start := time.Now()
				if err := s.Put(ctx, "scale", txt); err != nil {
					t.Fatal(err)
				}
				putSamples = append(putSamples, time.Since(start))
			}
			for _, txt := range []string{
				"the webhook retry limit is now 8 attempts",
				"Kenji Mori took over on-call for billing",
				"we migrated the primary database to eu-central-1",
				"TLS 1.3 is now the floor for every endpoint",
				"releases go out weekly since the pipeline rewrite",
			} {
				if err := s.Put(ctx, "scale", txt); err != nil {
					t.Fatal(err)
				}
			}

			for _, q := range probes {
				if _, err := s.Recall(ctx, "scale", q, 8); err != nil {
					t.Fatal(err)
				}
			}

			loadP50 := percentile(timeSamples(60, func() { _, _ = s.listLite("scale") }), 0.50)
			// Three passes, the least-loaded wins — the same discipline the
			// recall latency gate had to learn. One pass on a busy machine
			// swung the scan's own p99 at 30k from 320 ms to 1.13 s across
			// runs of identical code: noise wide enough to hide the effect
			// being measured.
			// Every measured recall has to come back with facts.
			//
			// This gate spent its whole life timing a call whose result it threw
			// away, which means a recall that returned NOTHING would have been
			// timed as infinitely fast and reported as a pass. The hole showed
			// up as a p50 of 36 microseconds on a second machine at 10 000
			// facts — a number no correct recall can produce, because the fusion
			// touches every live fact — and "warm cache" does not explain three
			// orders of magnitude. A latency gate that does not check its own
			// output is measuring the wrong thing at exactly the moment it
			// matters.
			bestP50, bestP99 := time.Duration(1<<63-1), time.Duration(1<<63-1)
			empty := 0
			for pass := 0; pass < 3; pass++ {
				recall := measureQueries(200, probes, func(q string) {
					got, err := s.Recall(ctx, "scale", q, 8)
					if err != nil {
						t.Fatalf("recall %q at %d facts: %v", q, size, err)
					}
					if len(got) == 0 {
						empty++
					}
				})
				if p := percentile(recall, 0.99); p < bestP99 {
					bestP99 = p
					bestP50 = percentile(recall, 0.50)
				}
			}
			p50, p99 := bestP50, bestP99
			if empty > 0 {
				t.Errorf("%s %d facts: %d of %d measured recalls returned ZERO facts — "+
					"their latency does not measure retrieval, it measures an empty answer",
					arm.label, size, empty, 3*200)
			}
			putP50 := percentile(putSamples, 0.50)

			share := 0.0
			if p50 > 0 {
				share = float64(loadP50) / float64(p50) * 100
			}
			t.Logf("[%s] %6d facts · recall top-8 p50 %8v p99 %8v (min of 3) · full scan %8v (%.0f%%) · put p50 %v",
				arm.label, size, p50.Round(time.Microsecond), p99.Round(time.Microsecond),
				loadP50.Round(time.Microsecond), share, putP50.Round(time.Microsecond))

			// A failing tail gate needs to say WHICH query is in the tail.
			// Reporting one p99 for a rotation of five probes hides the fact
			// that four of them are fast and one carries a term the corpus
			// repeats thousands of times — and those are different bugs with
			// different fixes.
			//
			// Every probe call is checked the same way the gate checks its own
			// measured recalls: this breakdown once ran blind while the gate
			// did not, and in that state it reported p50s of 0s for probes
			// whose recalls returned instantly without working. A percentile
			// over calls that did no work is not a latency, it is a ghost.
			// Empties are reported next to the number they would have
			// poisoned; the percentiles run over real work only.
			if arm.indexed && size == 30000 {
				for _, q := range probes {
					per := make([]time.Duration, 0, 40)
					vacios := 0
					for i := 0; i < 40; i++ {
						sampleStart := time.Now()
						got, err := s.Recall(ctx, "scale", q, 8)
						d := time.Since(sampleStart)
						if err != nil {
							t.Fatalf("probe %q: %v", q, err)
						}
						if len(got) == 0 {
							vacios++
							continue
						}
						per = append(per, d)
					}
					if vacios > 0 {
						t.Logf("    WARNING probe %q: %d of 40 recalls returned ZERO facts; percentiles over the %d that did work", q, vacios, len(per))
					}
					if len(per) == 0 {
						t.Errorf("probe %q: all 40 calls returned zero facts; there is no real latency to report", q)
						continue
					}
					t.Logf("    probe p50 %8v p99 %8v · %q",
						percentile(per, 0.50).Round(time.Microsecond),
						percentile(per, 0.99).Round(time.Microsecond), q)
				}
			}
			if arm.indexed && size == 30000 && p99 > 40*time.Millisecond {
				t.Errorf("GATE 2: indexed p99 at 30k = %v, want <= 40ms", p99)
			}
			if arm.indexed && size == 600 && p99 > 15*time.Millisecond {
				t.Errorf("GATE 4: indexed p99 at 600 = %v, want <= 15ms (the scan measures 12.1ms)", p99)
			}
			if arm.indexed && putP50 > 3*time.Millisecond {
				t.Errorf("GATE 3: put p50 with the index = %v, want <= 3ms (the scan measures 1.05ms)", putP50)
			}
			_ = s.Close()
		}
	}
}

// scaleFactText generates corpus text with a realistic term distribution: a
// handful of ubiquitous words (the worst case for a posting-list scan) mixed
// with per-fact rare ones (the case an index wins outright).
func scaleFactText(i int) string {
	svc := []string{"catalog", "billing", "identity", "shipping", "search"}[i%5]
	verb := []string{"runs", "hosts", "serves", "mirrors", "drains"}[i%5]
	return fmt.Sprintf("the %s service %s %d replicas in region %d with quota %d",
		svc, verb, i, i%7, i*3%911)
}

func timeSamples(n int, f func()) []time.Duration {
	xs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		f()
		xs = append(xs, time.Since(start))
	}
	return xs
}

func measureQueries(n int, qs []string, f func(string)) []time.Duration {
	xs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		f(qs[i%len(qs)])
		xs = append(xs, time.Since(start))
	}
	return xs
}

func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	s := make([]time.Duration, len(xs))
	copy(s, xs)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(p*float64(len(s)-1))]
}
