package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The scale curve: does revision currency survive a bigger corpus?
//
// The corpus grows by filler only — the 35 revision families and their probes
// are byte-identical at every scale, which is the whole reason the points are
// comparable. A corpus that regenerated its families per size would make the
// probes incomparable between points and the curve meaningless. Every arm
// carries the supersede edges, because that is the configuration the product
// ships once a correction is recorded through Store.Revise.
//
// Latency is captured alongside accuracy on purpose. Currency is a structural
// property and should hold flat across sizes; retrieval cost is not, and a
// benchmark that reported only the first would hide the price of the second.

func buildScaled(t *testing.T, dir string, total int, doStem bool) (*memory.Store, []string) {
	t.Helper()
	return buildScaledCfg(t, dir, total, memory.StoreConfig{StemKeywords: doStem})
}

// buildScaledCfg is buildScaled with the rest of the store config exposed, so a
// caller can vary the retrieval path as well as the tokenisation fold. DataDir,
// Embedder and DecayHalfLife are always the harness's, because a scale point
// measured against a different embedder or half-life is not a point on this
// curve.
func buildScaledCfg(t *testing.T, dir string, total int, cfg memory.StoreConfig) (*memory.Store, []string) {
	t.Helper()
	corpus := buildCorpusOfSize(total)
	if err := validate(corpus); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	cfg.Embedder = embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword})
	cfg.DecayHalfLife = halfLife
	store, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	later := make(map[string]bool)
	for _, f := range families {
		for _, s := range f.Statements[1:] {
			later[s] = true
		}
	}
	for _, line := range corpus {
		if later[line] {
			continue
		}
		if err := store.Put(ctx, agentID, line); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range families {
		for i := 0; i < len(f.Statements)-1; i++ {
			old, next := f.Statements[i], f.Statements[i+1]
			facts, err := store.List(agentID)
			if err != nil {
				t.Fatal(err)
			}
			var victims []memory.Fact
			for _, c := range facts {
				if c.Text == old && !c.IsSuperseded() {
					victims = append(victims, c)
				}
			}
			if len(victims) == 0 {
				t.Fatalf("%s: nothing to revise for %q at scale %d", f.ID, old, total)
			}
			if _, err := store.Revise(ctx, agentID, next, victims...); err != nil {
				t.Fatal(err)
			}
		}
	}
	return store, corpus
}

func TestGrayMatterScaleCurve(t *testing.T) {
	if testing.Short() {
		t.Skip("builds stores up to 3000 facts; skipped in -short")
	}
	ctx := context.Background()

	for _, size := range []int{600, 1000, 2000, 3000} {
		for _, doStem := range []bool{false, true} {
			dir, err := os.MkdirTemp("", "gmscale")
			if err != nil {
				t.Fatal(err)
			}
			store, corpus := buildScaled(t, dir, size, doStem)

			var a, b, primary, staleShown int
			var total time.Duration
			for _, f := range families {
				start := time.Now()
				receipts, err := store.RecallExplain(ctx, agentID, f.Query, len(corpus))
				if err != nil {
					t.Fatal(err)
				}
				total += time.Since(start)

				curAt, staleAt := -1, -1
				for i, r := range receipts {
					low := strings.ToLower(r.Text)
					if curAt < 0 && strings.Contains(low, strings.ToLower(f.Correct)) {
						curAt = i
					}
					for _, s := range f.Stale {
						if strings.Contains(low, strings.ToLower(s)) {
							if i < topK {
								staleShown++
							}
							if staleAt < 0 {
								staleAt = i
							}
						}
					}
				}
				okA := curAt >= 0 && (staleAt < 0 || curAt < staleAt)
				okB := curAt >= 0 && curAt < topK
				if okA {
					a++
				}
				if okB {
					b++
				}
				if okA && okB {
					primary++
				}
			}
			_ = store.Close()
			_ = os.RemoveAll(dir)

			label := "no stemming"
			if doStem {
				label = "with stemming"
			}
			t.Logf("%5d facts %-13s  A %2d/%d  B %2d/%d  A^B %2d/%d  stale shown %d  mean recall %v",
				size, label, a, len(families), b, len(families), primary, len(families),
				staleShown, (total / time.Duration(len(families))).Round(time.Millisecond))
		}
	}
}
