package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// Recall ranks by fused RRF score, and equal scores used to leave the order
// unspecified: sort.Slice is not stable, and each of the three signal
// rankings turns scores into ranks that the fusion then reads. Six facts
// written in the same instant produced all six rotations of the result, both
// across repeated calls on one store and across freshly built stores.
//
// The order is now total: score descending, then oldest first, then fact ID.

// tiebreakTexts are written in this order and are constructed so every signal
// ties. They share every scoreable term with the query, so keyword scores are
// equal; the clock is frozen, so recency scores are equal; the keyword
// embedder stores no vectors, so there is no vector signal. Only the
// tie-break can order them.
var tiebreakTexts = []string{
	"alpha deployment pipeline note",
	"bravo deployment pipeline note",
	"charlie deployment pipeline note",
	"delta deployment pipeline note",
	"echo deployment pipeline note",
	"foxtrot deployment pipeline note",
}

const tiebreakQuery = "deployment pipeline"

// tiebreakFrozen is the instant every fact in these tests is created at.
var tiebreakFrozen = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// newTiebreakStore builds a store whose clock never advances, so every fact
// shares a CreatedAt and every recency score is identical.
func newTiebreakStore(t *testing.T) (*Store, func()) {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.now = func() time.Time { return tiebreakFrozen }

	ctx := context.Background()
	for _, text := range tiebreakTexts {
		if err := s.Put(ctx, "tiebreak", text); err != nil {
			t.Fatalf("Put %q: %v", text, err)
		}
	}
	return s, func() { _ = s.Close() }
}

// label strips the shared suffix so failures read as "alpha bravo charlie"
// rather than as six near-identical sentences.
func label(results []string) string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, strings.Fields(r)[0])
	}
	return strings.Join(out, " ")
}

// TestRecall_TieBreakIsDeterministicAcrossCalls covers repeated calls against
// one store.
func TestRecall_TieBreakIsDeterministicAcrossCalls(t *testing.T) {
	s, cleanup := newTiebreakStore(t)
	defer cleanup()
	ctx := context.Background()

	first, err := s.Recall(ctx, "tiebreak", tiebreakQuery, 4)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	s.wg.Wait()

	for i := 2; i <= 150; i++ {
		got, err := s.Recall(ctx, "tiebreak", tiebreakQuery, 4)
		if err != nil {
			t.Fatalf("Recall call %d: %v", i, err)
		}
		s.wg.Wait()
		if label(got) != label(first) {
			t.Fatalf("call %d returned a different order than call 1\n  call 1: %s\n  call %d: %s",
				i, label(first), i, label(got))
		}
	}
}

// TestRecall_TieBreakIsDeterministicAcrossStores covers freshly built stores,
// where the ULIDs differ between runs. Insertion order has to survive that.
func TestRecall_TieBreakIsDeterministicAcrossStores(t *testing.T) {
	var first string
	for i := 1; i <= 25; i++ {
		s, cleanup := newTiebreakStore(t)
		got, err := s.Recall(context.Background(), "tiebreak", tiebreakQuery, 4)
		if err != nil {
			t.Fatalf("Recall on store %d: %v", i, err)
		}
		s.wg.Wait()
		cleanup()

		if i == 1 {
			first = label(got)
			continue
		}
		if label(got) != first {
			t.Fatalf("store %d returned a different order than store 1\n  store 1: %s\n  store %d: %s",
				i, first, i, label(got))
		}
	}
}

// TestRecall_TieBreakOrdersOldestFirst pins the order itself, not only its
// stability. A stable-but-wrong order would pass the two tests above.
//
// Every signal ties here, so the tie-break decides alone. CreatedAt is equal
// by construction, which leaves fact ID, and ULIDs increase monotonically
// within a process — so ascending ID is insertion order. The oldest fact
// written comes first.
func TestRecall_TieBreakOrdersOldestFirst(t *testing.T) {
	s, cleanup := newTiebreakStore(t)
	defer cleanup()

	got, err := s.Recall(context.Background(), "tiebreak", tiebreakQuery, len(tiebreakTexts))
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	s.wg.Wait()

	want := "alpha bravo charlie delta echo foxtrot"
	if label(got) != want {
		t.Errorf("tied facts are not returned oldest-first\n  got:  %s\n  want: %s", label(got), want)
	}
}

// TestRecall_TieBreakPrefersOlderCreatedAt covers the middle term of the
// order: when scores tie but CreatedAt differs, the older fact ranks first.
//
// Reaching that term takes care. Backdating a fact under the default weights
// does not produce a tie — it lowers the fact's recency score, so it ranks
// lower on merit rather than on the tie-break. The recency weight is therefore
// set to zero, which leaves CreatedAt with exactly one route into the result:
// the tie-break inside the keyword ranking, where these facts tie.
//
// The fact written last is backdated to be the oldest, so ID order and
// CreatedAt order disagree. An implementation that compared IDs alone would
// return it last instead of first.
func TestRecall_TieBreakPrefersOlderCreatedAt(t *testing.T) {
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
		SignalWeights: &SignalWeights{Vector: 0, Keyword: 1, Recency: 0},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	s.now = func() time.Time { return tiebreakFrozen }

	ctx := context.Background()
	for _, text := range tiebreakTexts {
		if err := s.Put(ctx, "tiebreak", text); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	facts, err := s.List("tiebreak")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// With a frozen clock List is reverse insertion order, so index 0 is the
	// last fact written and therefore the highest ULID.
	highestID := facts[0]
	highestID.CreatedAt = tiebreakFrozen.Add(-72 * time.Hour)
	if err := s.UpdateFact("tiebreak", highestID); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	got, err := s.Recall(ctx, "tiebreak", tiebreakQuery, len(tiebreakTexts))
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	s.wg.Wait()

	want := strings.Fields(highestID.Text)[0]
	if len(got) == 0 {
		t.Fatal("Recall returned nothing")
	}
	if first := strings.Fields(got[0])[0]; first != want {
		t.Errorf("the oldest fact should rank first when scores tie, even with the highest ID\n"+
			"  got first:  %s\n  want first: %s\n  full order: %s", first, want, label(got))
	}
}

// TestRecall_TieBreakInKeywordRanking covers the keyword ranking specifically.
//
// The corpus above cannot reach it. Every fact there contains both query
// terms, so df equals the fact count and idf is log((n+1)/(df+1)) = log(1) = 0
// — every keyword score is zero, no fact is scored at all, and the keyword
// ranking is empty. A tie-break defect there would be invisible, which is what
// mutation testing showed: reverting that sort to compare scores alone left
// every test above passing.
//
// Here only half the corpus matches the query, so idf is positive and the
// matching facts score equally and non-zero. The recency weight is set to zero
// so the keyword ranking is the only thing ordering them.
func TestRecall_TieBreakInKeywordRanking(t *testing.T) {
	matching := []string{
		"alpha deployment pipeline note",
		"bravo deployment pipeline note",
		"charlie deployment pipeline note",
		"delta deployment pipeline note",
	}
	unrelated := []string{
		"echo kitchen catering roster",
		"foxtrot kitchen catering roster",
		"golf kitchen catering roster",
		"hotel kitchen catering roster",
	}

	recall := func() []string {
		s, err := Open(StoreConfig{
			DataDir:       t.TempDir(),
			Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
			DecayHalfLife: 720 * time.Hour,
			SignalWeights: &SignalWeights{Vector: 0, Keyword: 1, Recency: 0},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()
		s.now = func() time.Time { return tiebreakFrozen }

		ctx := context.Background()
		for _, text := range append(append([]string{}, matching...), unrelated...) {
			if err := s.Put(ctx, "kwtie", text); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		got, err := s.Recall(ctx, "kwtie", tiebreakQuery, len(matching))
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		s.wg.Wait()
		return got
	}

	want := "alpha bravo charlie delta"
	for i := 1; i <= 20; i++ {
		if got := label(recall()); got != want {
			t.Fatalf("run %d: facts with equal keyword scores are not ordered oldest-first\n"+
				"  got:  %s\n  want: %s", i, got, want)
		}
	}
}
