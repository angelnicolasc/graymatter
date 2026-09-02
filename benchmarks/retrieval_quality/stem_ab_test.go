package main

import (
	"path/filepath"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The other published suite's verdict on stemming.
//
// The revision harness gained four probes from folding morphology into the
// keyword signal. That is one corpus, written by the same hand that wrote the
// stemmer's motivating examples, so it cannot decide the default on its own.
// This runs the frozen retrieval-quality fixtures — a corpus this change was
// not designed against — through both arms and compares hit rates.
func TestStemmingOnFrozenFixtures(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two stores over the frozen corpus; skipped in -short")
	}
	corpus, err := loadCorpus(filepath.Join(fixtureDir, "corpus-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	queries, err := loadQueries(filepath.Join(fixtureDir, "queries-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	off, err := runGrayMatter(corpus, queries, "stem-off", "fixed-K", memory.StoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	on, err := runGrayMatter(corpus, queries, "stem-on", "fixed-K",
		memory.StoreConfig{StemKeywords: true})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("frozen fixtures — stemming off: hit rate %.1f%% [%.1f, %.1f]",
		off.HitRate, off.HitLo, off.HitHi)
	t.Logf("frozen fixtures — stemming on : hit rate %.1f%% [%.1f, %.1f]",
		on.HitRate, on.HitLo, on.HitHi)

	if on.HitRate < off.HitRate {
		t.Errorf("stemming lowered the hit rate on the frozen corpus: %.1f%% -> %.1f%%",
			off.HitRate, on.HitRate)
	}
}

// The revert criterion for stemming's default, as a property rather than an
// aggregate.
//
// A hit rate that holds can still hide a trade: four queries won, four lost,
// same total. What the scale-corpus measurement actually found is stronger —
// the queries stemming fails are a STRICT SUBSET of the ones it fails without.
// It won four (backups/backup, rotations/rotation, releases/release,
// deployment/deploy) and lost none, at 5k, 10k and 30k facts alike.
//
// That is the shape a default-on flag has to keep. One query that passes
// without the stemmer and fails with it is a regression no aggregate would
// report, and it is the signal to revert. It is also the gate a Snowball
// migration has to clear again from scratch: the +4 was measured with this
// Porter, and a different stemmer does not inherit the number.
func TestStemmingNeverLosesAQueryItWinsWithout(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two stores over the frozen corpus; skipped in -short")
	}
	corpus, err := loadCorpus(filepath.Join(fixtureDir, "corpus-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	queries, err := loadQueries(filepath.Join(fixtureDir, "queries-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	off, err := runGrayMatter(corpus, queries, "stem-off", "fixed-K", memory.StoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	on, err := runGrayMatter(corpus, queries, "stem-on", "fixed-K",
		memory.StoreConfig{StemKeywords: true})
	if err != nil {
		t.Fatal(err)
	}

	hitOn := make(map[string]bool, len(on.PerQuery))
	for _, o := range on.PerQuery {
		hitOn[o.QueryID] = o.Hit
	}
	var lost []string
	for _, o := range off.PerQuery {
		if o.Hit && !hitOn[o.QueryID] {
			lost = append(lost, o.QueryID)
		}
	}
	if len(lost) > 0 {
		t.Errorf("stemming lost %d queries it wins without it: %v; "+
			"the default is ON on the strength of a strict-subset property; this breaks it",
			len(lost), lost)
	}
	won := 0
	for _, o := range off.PerQuery {
		if !o.Hit && hitOn[o.QueryID] {
			won++
		}
	}
	t.Logf("frozen fixtures — stemming wins %d queries, loses %d", won, len(lost))
}
