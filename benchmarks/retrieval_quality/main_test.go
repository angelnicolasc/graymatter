package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A benchmark whose own arithmetic is untested is a number generator. These
// tests cover the parts that decide what gets published: that the shipped
// fixtures are internally consistent, that a broken fixture fails loudly
// instead of quietly scoring zero, and that the metrics count what they say.

const fixtureDir = "../fixtures"

// TestShippedFixturesAreValid is the guard that matters most. If a gold fact
// ID were renamed, every HitRate would drop to zero and look like a finding
// about retrieval rather than a typo in a JSON file.
func TestShippedFixturesAreValid(t *testing.T) {
	corpus, err := loadCorpus(filepath.Join(fixtureDir, "corpus-v1.jsonl"))
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	queries, err := loadQueries(filepath.Join(fixtureDir, "queries-v1.jsonl"))
	if err != nil {
		t.Fatalf("load queries: %v", err)
	}
	if err := validateFixtures(corpus, queries); err != nil {
		t.Fatalf("shipped fixtures are inconsistent: %v", err)
	}

	// The protocol in RESULTS.md rests on these properties; assert them here
	// so a corpus edit cannot silently invalidate the published method.
	var gold, stale, replacement int
	newest := 0
	for _, f := range corpus {
		switch f.Kind {
		case "gold":
			gold++
		case "stale":
			stale++
		case "replacement":
			replacement++
		}
		if f.Session > newest {
			newest = f.Session
		}
	}
	if gold < 3 {
		t.Errorf("need at least 3 planted gold facts, have %d", gold)
	}
	if stale == 0 || replacement == 0 {
		t.Errorf("the contradiction pair is missing: stale=%d replacement=%d", stale, replacement)
	}

	// Gold facts must be planted early and asked about late, or the benchmark
	// is not measuring what it claims to measure.
	for _, f := range corpus {
		if f.Kind != "gold" {
			continue
		}
		if f.Session > 10 {
			t.Errorf("gold fact %s is at session %d; it must be planted early to be "+
				"out of reach of a sliding window", f.ID, f.Session)
		}
	}
	for _, q := range queries {
		if q.AskedAt < newest-10 {
			t.Errorf("query %s is asked at session %d but the corpus runs to %d; "+
				"queries must be late for the window comparison to mean anything",
				q.ID, q.AskedAt, newest)
		}
	}
}

// TestValidateFixtures_RejectsBrokenFixtures is the regression test for the
// validator itself.
func TestValidateFixtures_RejectsBrokenFixtures(t *testing.T) {
	base := []fact{
		{ID: "f1", Session: 1, Domain: "a", Kind: "gold", Text: "alpha"},
		{ID: "f2", Session: 2, Domain: "b", Kind: "filler", Text: "beta"},
		{ID: "f3", Session: 3, Domain: "c", Kind: "filler", Text: "gamma"},
	}
	okQueries := []query{
		{ID: "q1", Domain: "a", Text: "x", Gold: []string{"f1"}},
		{ID: "q2", Domain: "b", Text: "x", Gold: []string{"f2"}},
		{ID: "q3", Domain: "c", Text: "x", Gold: []string{"f3"}},
		{ID: "q4", Domain: "a", Text: "x", Gold: []string{"f1"}},
		{ID: "q5", Domain: "b", Text: "x", Gold: []string{"f2"}},
	}
	if err := validateFixtures(base, okQueries); err != nil {
		t.Fatalf("valid fixtures rejected: %v", err)
	}

	for _, tc := range []struct {
		name    string
		corpus  []fact
		queries []query
	}{
		{
			name:    "unknown gold id",
			corpus:  base,
			queries: append(clone(okQueries[:4]), query{ID: "q5", Domain: "b", Text: "x", Gold: []string{"f99"}}),
		},
		{
			name:    "unknown forbidden id",
			corpus:  base,
			queries: append(clone(okQueries[:4]), query{ID: "q5", Domain: "b", Text: "x", Gold: []string{"f2"}, Forbidden: []string{"f99"}}),
		},
		{
			name:    "query with no gold",
			corpus:  base,
			queries: append(clone(okQueries[:4]), query{ID: "q5", Domain: "b", Text: "x"}),
		},
		{
			name:    "too few queries",
			corpus:  base,
			queries: okQueries[:4],
		},
		{
			name:    "too few domains",
			corpus:  base,
			queries: []query{{ID: "q1", Domain: "a", Text: "x", Gold: []string{"f1"}}, {ID: "q2", Domain: "a", Text: "x", Gold: []string{"f1"}}, {ID: "q3", Domain: "a", Text: "x", Gold: []string{"f1"}}, {ID: "q4", Domain: "a", Text: "x", Gold: []string{"f1"}}, {ID: "q5", Domain: "b", Text: "x", Gold: []string{"f1"}}},
		},
		{
			name:    "duplicate fact id",
			corpus:  append(clone3(base), fact{ID: "f1", Session: 4, Domain: "a", Text: "delta"}),
			queries: okQueries,
		},
		{
			name: "duplicate fact text",
			// Fact IDs are recovered from returned text, so two facts sharing
			// text would make the mapping ambiguous and the metrics wrong.
			corpus:  append(clone3(base), fact{ID: "f4", Session: 4, Domain: "a", Text: "alpha"}),
			queries: okQueries,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateFixtures(tc.corpus, tc.queries); err == nil {
				t.Error("broken fixture accepted; a benchmark run against it would publish nonsense")
			}
		})
	}
}

// TestMeasure_CountsWhatItClaims pins the metric arithmetic against a system
// whose answers are fixed by hand.
func TestMeasure_CountsWhatItClaims(t *testing.T) {
	corpus := []fact{
		{ID: "f1", Text: "one two three"},   // 3 words -> 3*1.33 = 3 tokens
		{ID: "f2", Text: "four five"},       // 2 words
		{ID: "f3", Text: "six"},             // 1 word
		{ID: "dead", Text: "stale content"}, // 2 words
	}
	queries := []query{
		{ID: "hit", Gold: []string{"f1"}},
		{ID: "miss", Gold: []string{"f2"}},
		{ID: "deadly", Gold: []string{"f3"}, Forbidden: []string{"dead"}},
	}
	answers := map[string][]string{
		"hit":    {"f1"},         // hit, no dead
		"miss":   {"f3"},         // miss, no dead
		"deadly": {"f3", "dead"}, // hit AND dead
	}

	got := measure("stub", "fixed-K", queries, func(q query) ([]string, time.Duration) {
		return answers[q.ID], time.Millisecond
	}, corpus)

	if got.HitRate < 66.6 || got.HitRate > 66.7 {
		t.Errorf("HitRate = %.2f, want 2 of 3 (66.67)", got.HitRate)
	}
	if got.DeadRate < 33.3 || got.DeadRate > 33.4 {
		t.Errorf("DeadRate = %.2f, want 1 of 3 (33.33)", got.DeadRate)
	}
	if got.AvgReturned != (1+1+2)/3.0 {
		t.Errorf("AvgReturned = %.2f, want 1.33", got.AvgReturned)
	}
	if len(got.PerQuery) != 3 {
		t.Fatalf("per-query outcomes = %d, want 3", len(got.PerQuery))
	}
	if !got.PerQuery[0].Hit || got.PerQuery[1].Hit || !got.PerQuery[2].Hit {
		t.Errorf("per-query hits wrong: %+v", got.PerQuery)
	}
	if got.PerQuery[0].Dead || got.PerQuery[1].Dead || !got.PerQuery[2].Dead {
		t.Errorf("per-query dead flags wrong: %+v", got.PerQuery)
	}
}

// TestSameSet backs the ADR-006 equivalence check. If this were wrong, the
// benchmark could report the ADR confirmed when it is not.
func TestSameSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
		want bool
	}{
		{"identical", []string{"a", "b"}, []string{"a", "b"}, true},
		{"reordered", []string{"a", "b"}, []string{"b", "a"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"different members", []string{"a", "b"}, []string{"a", "c"}, false},
		{"duplicates matter", []string{"a", "a"}, []string{"a", "b"}, false},
		{"both empty", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameSet(tc.a, tc.b); got != tc.want {
				t.Errorf("sameSet(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func clone(qs []query) []query {
	out := make([]query, len(qs))
	copy(out, qs)
	return out
}

func clone3(fs []fact) []fact {
	out := make([]fact, len(fs))
	copy(out, fs)
	return out
}

// TestReadmeQualityTableMatchesMeasurement gates every number README.md
// publishes from this benchmark, including the adaptive column.
//
// An earlier round removed a five-fold-wrong token table from
// docs/benchmarks.md that had survived several releases because nothing
// compared it against a run. Publishing a quality table without gating it
// would recreate that.
func TestReadmeQualityTableMatchesMeasurement(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(raw)

	readme = strings.ReplaceAll(readme, "**", "")

	measured, err := runAll(fixtureDir)
	if err != nil {
		t.Fatalf("runAll: %v", err)
	}
	window, ok := measured.byName("window-8")
	if !ok {
		t.Fatal("window-8 missing from the measured suite")
	}
	gm, ok := measured.byName("graymatter-fixed-k")
	if !ok {
		t.Fatal("graymatter-fixed-k missing from the measured suite")
	}
	adaptive, ok := measured.byName("graymatter-adaptive")
	if !ok {
		t.Fatal("graymatter-adaptive missing from the measured suite")
	}

	// Each published row, as it appears in README.md, against the measurement.
	// Bold markers (**value**) are stripped from the readme before matching so
	// formatting changes do not break the numeric gate.
	for _, tc := range []struct {
		label string
		row   string
	}{
		{
			label: "HitRate on the planted old fact",
			row: fmt.Sprintf("| Finds a fact planted 96 sessions ago | %.0f%% | %.0f%% | %.0f%% |",
				window.HitRate, gm.HitRate, adaptive.HitRate),
		},
		{
			label: "dead-fact rate",
			row: fmt.Sprintf("| Returns a superseded fact | %.0f%% | %.0f%% | %.0f%% |",
				window.DeadRate, gm.DeadRate, adaptive.DeadRate),
		},
		{
			label: "tokens per query",
			row: fmt.Sprintf("| Tokens per query | %.0f | %.0f | %.0f |",
				window.AvgTokens, gm.AvgTokens, adaptive.AvgTokens),
		},
	} {
		if !strings.Contains(readme, tc.row) {
			t.Errorf("README.md does not carry the measured %s.\n"+
				"  expected row: %s\n"+
				"Update the table in README.md from `go run ./benchmarks/retrieval_quality`.",
				tc.label, tc.row)
		}
	}

	// The prose around the table states two directional relationships. Both are
	// checked, so a reversal fails here instead of leaving a stale sentence
	// standing next to a correct table.
	if gm.AvgTokens <= window.AvgTokens {
		t.Errorf("README.md states GrayMatter costs more per query than a window at "+
			"equal fact count, but the measurement is GrayMatter %.0f, window %.0f. "+
			"The relationship reversed; update the sentence.",
			gm.AvgTokens, window.AvgTokens)
	}
	if adaptive.AvgTokens >= window.AvgTokens {
		t.Errorf("README.md states MinRelevance drops below the window on tokens, but "+
			"the measurement is adaptive %.0f, window %.0f. "+
			"The relationship reversed; update the sentence.",
			adaptive.AvgTokens, window.AvgTokens)
	}
	if adaptive.HitRate != gm.HitRate {
		t.Errorf("README.md states MinRelevance keeps the same recall, but the "+
			"measurement is adaptive %.0f%% against fixed-K %.0f%%. "+
			"Update the sentence.", adaptive.HitRate, gm.HitRate)
	}
}

// Hand-computed reference: n=10, k=7, z=1.96 gives
//
//	p=0.7, den=1.38416, center=0.64464, half=0.24773 -> [39.7%, 89.2%].
//
// The interval must contain the point estimate, stay inside [0,100], and
// collapse toward the estimate as n grows.
func TestWilsonInterval_KnownValues(t *testing.T) {
	lo, hi := wilsonInterval(7, 10, 1.96)
	if math.Abs(lo-39.7) > 0.15 || math.Abs(hi-89.2) > 0.15 {
		t.Fatalf("wilson(7,10) = [%.1f, %.1f], want ~[39.7, 89.2]", lo, hi)
	}

	if lo, hi := wilsonInterval(0, 5, 1.96); lo != 0 || hi >= 60 {
		t.Fatalf("wilson(0,5) = [%.1f, %.1f], want [0, <60]", lo, hi)
	}
	if lo, hi := wilsonInterval(5, 5, 1.96); hi != 100 || lo <= 45 {
		t.Fatalf("wilson(5,5) = [%.1f, %.1f], want hi=100 and lo>45", lo, hi)
	}

	smallLo, _ := wilsonInterval(5, 8, 1.96)
	bigLo, bigHi := wilsonInterval(5000, 8000, 1.96)
	if bigHi-bigLo >= smallLo { // sanity: large-n interval far tighter
		t.Fatalf("interval did not shrink with n: small half-width %.1f vs large %.1f", smallLo, bigHi-bigLo)
	}

	lo, hi = wilsonInterval(3, 0, 1.96)
	if lo != 0 || hi != 0 {
		t.Fatalf("n=0 must yield [0,0], got [%.1f, %.1f]", lo, hi)
	}
}
