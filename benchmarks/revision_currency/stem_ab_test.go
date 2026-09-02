package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The A/B that decides whether stemming ships.
//
// Three of the eight probes the keyword ranker missed on this corpus missed on
// morphology alone — "backups" against "backup retention", "pager rotation"
// against "rotations were stretched", "deploy" against "deployment moved". The
// question this answers is not "does the stemmer stem" (stem_test.go covers
// that) but "does folding morphology into the keyword signal actually move the
// endpoint, and does it cost anything anywhere else".
//
// Both arms build the same 600-fact corpus with the same revision edges on the
// same scripted timeline. The only difference is StemKeywords.
func buildArm(t *testing.T, dir string, corpus []string, doStem bool) *memory.Store {
	t.Helper()
	store, err := memory.Open(memory.StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: halfLife,
		StemKeywords:  doStem,
	})
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
				t.Fatalf("%s: nothing to revise for %q", f.ID, old)
			}
			if _, err := store.Revise(ctx, agentID, next, victims...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := scriptTimeline(store, corpus); err != nil {
		t.Fatal(err)
	}
	return store
}

func scoreArm(t *testing.T, store *memory.Store) (primary int, failures []string) {
	t.Helper()
	ctx := context.Background()
	for _, f := range families {
		receipts, err := store.RecallExplain(ctx, agentID, f.Query, corpusSize)
		if err != nil {
			t.Fatal(err)
		}
		curAt, staleAt := -1, -1
		for i, r := range receipts {
			low := strings.ToLower(r.Text)
			if curAt < 0 && strings.Contains(low, strings.ToLower(f.Correct)) {
				curAt = i
			}
			for _, s := range f.Stale {
				if strings.Contains(low, strings.ToLower(s)) && staleAt < 0 {
					staleAt = i
				}
			}
		}
		a := curAt >= 0 && (staleAt < 0 || curAt < staleAt)
		b := curAt >= 0 && curAt < topK
		if a && b {
			primary++
		} else {
			failures = append(failures, f.ID)
		}
	}
	return primary, failures
}

func TestStemmingMovesTheEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two 600-fact stores; skipped in -short")
	}
	corpus := buildCorpus()
	if err := validate(corpus); err != nil {
		t.Fatal(err)
	}

	plainDir, err := os.MkdirTemp("", "stem-off")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(plainDir) }()
	stemDir, err := os.MkdirTemp("", "stem-on")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(stemDir) }()

	off := buildArm(t, plainDir, corpus, false)
	defer func() { _ = off.Close() }()
	on := buildArm(t, stemDir, corpus, true)
	defer func() { _ = on.Close() }()

	offScore, offFail := scoreArm(t, off)
	onScore, onFail := scoreArm(t, on)

	t.Logf("stemming off: %d/%d  (fails: %s)", offScore, len(families), strings.Join(offFail, " "))
	t.Logf("stemming on : %d/%d  (fails: %s)", onScore, len(families), strings.Join(onFail, " "))

	if onScore < offScore {
		t.Errorf("stemming made the endpoint worse: %d -> %d", offScore, onScore)
	}
	// Every probe the plain arm solved must still be solved. A net gain that
	// hides a regression on a probe that used to work is not a gain worth
	// shipping — it is a trade nobody asked for.
	offSolved := map[string]bool{}
	for _, f := range families {
		offSolved[f.ID] = true
	}
	for _, id := range offFail {
		offSolved[id] = false
	}
	for _, id := range onFail {
		if offSolved[id] {
			t.Errorf("%s worked without stemming and broke with it", id)
		}
	}
}
