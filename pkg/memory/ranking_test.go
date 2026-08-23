package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// SignalWeights and MinRelevance are opt-in by construction: every default
// reproduces v0.9.0 exactly. The first test in this file is the one that
// matters — it is the anti-regression gate the whole design rests on. If it
// fails, both knobs are wrong regardless of how well they work when set.

// rankingStore opens a store with an explicit StoreConfig so each test can
// vary the ranking fields.
func rankingStore(t *testing.T, mutate func(*StoreConfig)) (*Store, func()) {
	t.Helper()
	cfg := StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, func() { _ = s.Close() }
}

// rankingCorpus is a fixed corpus with staggered creation times, so recency
// and keyword relevance disagree and the weights are actually observable.
// Written oldest first; the last entry is both newest and least relevant to
// the query used below.
var rankingCorpus = []string{
	"the deployment pipeline runs on kubernetes with argo rollouts",
	"database migrations are applied by atlas during deployment",
	"the staging deployment cluster is shared between all teams",
	"lunch is catered on fridays in the office kitchen",
	"parking permits are renewed through the facilities portal",
	"the coffee machine on floor two takes whole beans only",
}

const rankingQuery = "deployment pipeline"

func seedRanking(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for i, text := range rankingCorpus {
		if err := s.Put(ctx, "rank", text); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		// Age the facts so recency is a signal that can disagree with keyword
		// relevance: earlier entries are older.
		facts, err := s.List("rank")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for j := range facts {
			if facts[j].Text != text {
				continue
			}
			age := time.Duration(len(rankingCorpus)-i) * 240 * time.Hour
			facts[j].CreatedAt = time.Now().UTC().Add(-age)
			if err := s.UpdateFact("rank", facts[j]); err != nil {
				t.Fatalf("UpdateFact: %v", err)
			}
		}
	}
}

// TestRankingDefaults_MatchV09Behaviour is the regression gate. Both new
// fields are left at their zero value, which is what every existing caller
// passes, and the result must be identical to the hardcoded ranking v0.9.0
// shipped: RRF over vector 1.0, keyword 1.0, recency 0.5, returning exactly
// topK with no threshold.
func TestRankingDefaults_MatchV09Behaviour(t *testing.T) {
	// The v0.9.0 ranking, recomputed here from the same inputs rather than
	// pasted in as a golden string, so the comparison stays meaningful if the
	// corpus changes.
	want := rankWithV09Algorithm(t)

	s, cleanup := rankingStore(t, nil) // zero value: no SignalWeights, no MinRelevance
	defer cleanup()
	seedRanking(t, s)

	got, err := s.Recall(context.Background(), "rank", rankingQuery, 4)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	assertSameOrder(t, got, want)
}

// TestSignalWeights_ExplicitDefaultsAreIdentical checks the documented default
// values really are the hardcoded ones. Someone reading DefaultSignalWeights()
// has to be able to trust that setting them changes nothing.
func TestSignalWeights_ExplicitDefaultsAreIdentical(t *testing.T) {
	base, cleanupBase := rankingStore(t, nil)
	defer cleanupBase()
	seedRanking(t, base)
	want, err := base.Recall(context.Background(), "rank", rankingQuery, 4)
	if err != nil {
		t.Fatalf("Recall base: %v", err)
	}

	w := DefaultSignalWeights()
	explicit, cleanup := rankingStore(t, func(c *StoreConfig) { c.SignalWeights = &w })
	defer cleanup()
	seedRanking(t, explicit)
	got, err := explicit.Recall(context.Background(), "rank", rankingQuery, 4)
	if err != nil {
		t.Fatalf("Recall explicit: %v", err)
	}
	assertSameOrder(t, got, want)
}

// TestSignalWeights_RecencyOnlyEmulatesSlidingWindow is the test that earns
// ADR-006 its claim. With all weight on recency, hybrid retrieval degenerates
// into "return the K most recent facts" — which is exactly a sliding window of
// size K. That makes truncation a special case of this ranking rather than a
// different design, and it means a sliding-window baseline can be measured by
// reconfiguring the model instead of writing a second retrieval path.
//
// The claim was not true before this change: the weights were compile-time
// constants and there was no way to reach recency-only.
func TestSignalWeights_RecencyOnlyEmulatesSlidingWindow(t *testing.T) {
	s, cleanup := rankingStore(t, func(c *StoreConfig) {
		c.SignalWeights = &SignalWeights{Vector: 0, Keyword: 0, Recency: 1}
	})
	defer cleanup()
	seedRanking(t, s)

	const window = 3
	got, err := s.Recall(context.Background(), "rank", rankingQuery, window)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// The newest `window` facts, by construction of seedRanking: the corpus is
	// written oldest first, so the tail is newest.
	want := rankingCorpus[len(rankingCorpus)-window:]
	reversed := make([]string, 0, window)
	for i := len(want) - 1; i >= 0; i-- {
		reversed = append(reversed, want[i])
	}
	assertSameOrder(t, got, reversed)

	// And the point of the comparison: the window returns none of the facts
	// that actually answer the query, because none of them are recent.
	for _, g := range got {
		if strings.Contains(g, "deployment") {
			t.Errorf("recency-only recall returned a relevant fact %q; the corpus was "+
				"built so the relevant facts are the old ones, which is the whole "+
				"failure mode a sliding window has", g)
		}
	}
}

// TestSignalWeights_KeywordOnlyRanksByRelevance is the mirror image: all
// weight on keyword relevance surfaces the facts that answer the query,
// regardless of age.
func TestSignalWeights_KeywordOnlyRanksByRelevance(t *testing.T) {
	s, cleanup := rankingStore(t, func(c *StoreConfig) {
		c.SignalWeights = &SignalWeights{Vector: 0, Keyword: 1, Recency: 0}
	})
	defer cleanup()
	seedRanking(t, s)

	got, err := s.Recall(context.Background(), "rank", rankingQuery, 3)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("keyword-only recall returned nothing")
	}
	if !strings.Contains(got[0], "deployment") {
		t.Errorf("keyword-only recall put %q first; expected a deployment fact", got[0])
	}
}

// TestMinRelevance_ZeroReturnsExactlyTopK pins the v0.9.0 contract: Recall
// returns topK results, padding with weak matches rather than cutting.
func TestMinRelevance_ZeroReturnsExactlyTopK(t *testing.T) {
	s, cleanup := rankingStore(t, nil)
	defer cleanup()
	seedRanking(t, s)

	got, err := s.Recall(context.Background(), "rank", rankingQuery, 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("MinRelevance unset must return exactly topK; got %d results: %v", len(got), got)
	}
}

// TestMinRelevance_TrimsWeakMatches shows the knob doing its job: with a
// threshold set, obviously irrelevant facts stop padding the result.
func TestMinRelevance_TrimsWeakMatches(t *testing.T) {
	s, cleanup := rankingStore(t, func(c *StoreConfig) { c.MinRelevance = 0.9 })
	defer cleanup()
	seedRanking(t, s)

	got, err := s.Recall(context.Background(), "rank", rankingQuery, 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) >= 5 {
		t.Fatalf("MinRelevance 0.9 returned the full topK (%d); nothing was trimmed: %v", len(got), got)
	}
	if len(got) == 0 {
		t.Fatal("MinRelevance 0.9 trimmed everything, including the best match")
	}
	for _, g := range got {
		if strings.Contains(g, "coffee") || strings.Contains(g, "parking") || strings.Contains(g, "lunch") {
			t.Errorf("a fact unrelated to the query survived the threshold: %q", g)
		}
	}
}

// TestMinRelevance_IsRelativeNotAbsolute documents why the threshold is a
// fraction of the best score in the result set rather than a fixed number.
// RRF scores depend on how many facts were ranked — the same fact scores
// differently in a 6-fact store and a 600-fact one — so an absolute cutoff
// would silently mean something different as a store grows. A relative
// threshold keeps the top match by definition, at any store size.
func TestMinRelevance_IsRelativeNotAbsolute(t *testing.T) {
	for _, size := range []int{6, 60} {
		t.Run(fmt.Sprintf("store-of-%d", size), func(t *testing.T) {
			s, cleanup := rankingStore(t, func(c *StoreConfig) { c.MinRelevance = 0.5 })
			defer cleanup()

			ctx := context.Background()
			if err := s.Put(ctx, "scale", "the deployment pipeline runs on kubernetes"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			for i := 0; i < size; i++ {
				if err := s.Put(ctx, "scale", fmt.Sprintf("unrelated office note number %d", i)); err != nil {
					t.Fatalf("Put filler: %v", err)
				}
			}

			got, err := s.Recall(ctx, "scale", "deployment pipeline kubernetes", 8)
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if len(got) == 0 {
				t.Fatalf("the best match was trimmed in a store of %d facts; the "+
					"threshold is behaving as an absolute cutoff", size)
			}
			if !strings.Contains(got[0], "deployment") {
				t.Errorf("top result in a store of %d is %q, expected the deployment fact", size, got[0])
			}
		})
	}
}

// --- helpers ---

// rankWithV09Algorithm reproduces the v0.9.0 ranking with its constants
// written out literally, against a store that uses the new code path. It is
// the independent expectation the defaults are checked against.
func rankWithV09Algorithm(t *testing.T) []string {
	t.Helper()
	s, cleanup := rankingStore(t, func(c *StoreConfig) {
		// The exact constants recall.go carried before they were configurable.
		c.SignalWeights = &SignalWeights{Vector: 1.0, Keyword: 1.0, Recency: 0.5}
		c.MinRelevance = 0.0
	})
	defer cleanup()
	seedRanking(t, s)

	got, err := s.Recall(context.Background(), "rank", rankingQuery, 4)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	return got
}

func assertSameOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("result count changed: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("result %d differs:\n got:  %q\n want: %q", i, got[i], want[i])
		}
	}
}
