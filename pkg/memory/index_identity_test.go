package memory

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// PRE-REGISTERED GATE 1 for candidate-set retrieval, and the only one that can veto the whole
// approach: the candidate-set path must return exactly what the scan returns.
//
// Not "the same top-3", not "close enough" — the same fact IDs in the same
// order with the same fused scores, and the same weak-match block. An
// optimisation that changes an answer is a re-specification wearing an
// optimisation's clothes, and it would silently invalidate every measurement
// this lab has taken on the scan path.
//
// The corpus is built to hit the places the two paths could diverge:
//
//	same-instant writes    the tie-break is CreatedAt then ID, and the index
//	                       stores facts in timestamp-then-ID key order. Walking
//	                       that backwards reverses the ID half of the tie —
//	                       invisible unless facts share a clock tick, which on
//	                       Windows (~15.6 ms) is the common case, not the rare
//	                       one.
//	tombstones             superseded facts leave the corpus, so they must
//	                       leave the postings and the document frequencies too,
//	                       or idf drifts and every score with it.
//	aliases                vocabulary, not content: never a posting, never a df
//	                       contribution, never a result — but they still expand
//	                       the query, so the candidate set is computed from the
//	                       EXPANDED query or the two paths retrieve different
//	                       facts.
//	duplicate texts        recall dedups by text as it walks the ranking, so a
//	                       head window of exactly topK entries can come up
//	                       short and the indexed path has to widen it.
//	unknown query terms    zero postings: the candidate set is empty and the
//	                       ranking is pure recency, which is the case that
//	                       proves the spine carries the recency signal exactly.
func TestIndexedRecallIsIdenticalToScan(t *testing.T) {
	for _, doStem := range []bool{false, true} {
		name := "stem-off"
		if doStem {
			name = "stem-on"
		}
		t.Run(name, func(t *testing.T) {
			// ONE store, both paths. Two stores would carry different ULIDs
			// for the same sentences, and the comparison would silently
			// weaken to "same texts, similar scores" — which is exactly the
			// kind of comparison that lets a tie-break bug through. The
			// index is maintained on every write regardless of the flag, so
			// flipping it between calls changes only which path reads.
			st := buildIdentityCorpus(t, false, doStem)
			defer func() { _ = st.Close() }()
			scan, indexed := readerPair(st)

			ctx := context.Background()
			for _, q := range identityQueries {
				for _, topK := range []int{1, 3, 8, 25} {
					wantTexts, wantFB, err := scan.RecallDetailed(ctx, "id", q, topK)
					if err != nil {
						t.Fatalf("scan %q: %v", q, err)
					}
					gotTexts, gotFB, err := indexed.RecallDetailed(ctx, "id", q, topK)
					if err != nil {
						t.Fatalf("indexed %q: %v", q, err)
					}
					if len(wantTexts) != len(gotTexts) {
						t.Fatalf("%q topK=%d: scan returned %d facts, indexed %d",
							q, topK, len(wantTexts), len(gotTexts))
					}
					for i := range wantTexts {
						if wantTexts[i] != gotTexts[i] {
							t.Fatalf("%q topK=%d rank %d:\n  scan    %q\n  indexed %q",
								q, topK, i, wantTexts[i], gotTexts[i])
						}
					}
					if wantFB != gotFB {
						t.Errorf("%q topK=%d: weak-match block differs\n  scan    %q\n  indexed %q",
							q, topK, wantFB, gotFB)
					}
				}
			}

			// The receipts carry the fused scores and the per-signal ranks, so
			// comparing them tests the arithmetic and not only its argmax —
			// the same reason the golden ranking fixture records scores.
			for _, q := range identityQueries {
				wantR, err := scan.RecallExplain(ctx, "id", q, 12)
				if err != nil {
					t.Fatal(err)
				}
				gotR, err := indexed.RecallExplain(ctx, "id", q, 12)
				if err != nil {
					t.Fatal(err)
				}
				if len(wantR) != len(gotR) {
					t.Fatalf("%q: %d receipts vs %d", q, len(wantR), len(gotR))
				}
				for i := range wantR {
					w, g := wantR[i].Ranks, gotR[i].Ranks
					if wantR[i].Provenance.FactID != gotR[i].Provenance.FactID ||
						w.FusedScore != g.FusedScore || w.K != g.K ||
						w.KeywordRank != g.KeywordRank || w.RecencyRank != g.RecencyRank ||
						w.VectorRank != g.VectorRank {
						t.Fatalf("%q receipt %d:\n  scan    id=%s score=%v kw=%d rec=%d vec=%d\n  indexed id=%s score=%v kw=%d rec=%d vec=%d",
							q, i, wantR[i].Provenance.FactID, w.FusedScore, w.KeywordRank, w.RecencyRank, w.VectorRank,
							gotR[i].Provenance.FactID, g.FusedScore, g.KeywordRank, g.RecencyRank, g.VectorRank)
					}
					// Lineage is receipt data the indexed path assembles from
					// the tombstones it loads separately; a divergence here
					// means the retiredBy map lost an edge.
					if len(wantR[i].Provenance.Supersedes) != len(gotR[i].Provenance.Supersedes) {
						t.Errorf("%q receipt %d: lineage %v vs %v", q, i,
							wantR[i].Provenance.Supersedes, gotR[i].Provenance.Supersedes)
					}
				}
			}
		})
	}
}

var identityQueries = []string{
	"how long are invoices retained?",
	"who approves the deployment?",
	"deployments",
	"payment processor",
	"quixotic zephyr nonesuch",       // nothing in the corpus: pure recency
	"the and of",                     // stopwords only: no terms at all
	"clearinghouse submissions",      // reaches the corpus only through an alias
	"replica region service catalog", // ubiquitous terms: the candidate set is everything
}

// readerPair returns two views of one store: one that recalls through the
// scan and one that recalls through the candidate-set index. They share the
// bbolt handle, the facts and the fact IDs, so any difference between them is
// a difference in the retrieval path and nothing else.
func readerPair(s *Store) (scan, indexed *pathReader) {
	return &pathReader{s: s, indexed: false}, &pathReader{s: s, indexed: true}
}

type pathReader struct {
	s       *Store
	indexed bool
}

func (r *pathReader) RecallDetailed(ctx context.Context, agentID, q string, topK int) ([]string, string, error) {
	r.s.cfg.CandidateRetrieval = r.indexed
	return r.s.RecallDetailed(ctx, agentID, q, topK)
}

func (r *pathReader) RecallExplain(ctx context.Context, agentID, q string, topK int) ([]RecallReceipt, error) {
	r.s.cfg.CandidateRetrieval = r.indexed
	return r.s.RecallExplain(ctx, agentID, q, topK)
}

// buildIdentityCorpus writes the corpus once. The index is maintained on every
// write regardless of the retrieval flag, because maintenance is
// unconditional — which is itself the property that makes turning the flag on
// safe on a store that has been running without it.
func buildIdentityCorpus(t *testing.T, indexed, doStem bool) *Store {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      8760 * time.Hour,
		StemKeywords:       doStem,
		CandidateRetrieval: indexed,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A scripted clock with deliberate collisions: every third fact shares its
	// predecessor's timestamp, which is what puts the ID tie-break under load.
	clock := &explainClock{}
	s.now = func() time.Time {
		return clock.now()
	}

	rng := rand.New(rand.NewSource(20260831))
	nouns := []string{"invoice", "invoices", "deployment", "deployments", "replica",
		"region", "service", "catalog", "payment", "payments", "retention", "approval"}
	verbs := []string{"needs", "requires", "blocks", "mirrors", "drains", "retains"}

	for i := 0; i < 240; i++ {
		if i%3 != 0 {
			clock.offset += time.Hour
		}
		txt := fmt.Sprintf("%s %s %s after %d days in region %d",
			nouns[rng.Intn(len(nouns))], verbs[rng.Intn(len(verbs))],
			nouns[rng.Intn(len(nouns))], rng.Intn(90), i%7)
		if err := s.Put(ctx, "id", txt); err != nil {
			t.Fatal(err)
		}
	}
	// Duplicate texts: the dedup-by-text contract means the head window the
	// indexed path loads has to widen past topK.
	for i := 0; i < 6; i++ {
		clock.offset += time.Hour
		if err := s.Put(ctx, "id", "the deployment approval sits with the release manager"); err != nil {
			t.Fatal(err)
		}
	}
	// A revision chain, so the corpus carries tombstones and lineage.
	clock.offset += time.Hour
	first, err := s.putReturningFact(ctx, "id", "invoices are retained for 30 days")
	if err != nil {
		t.Fatal(err)
	}
	clock.offset += time.Hour
	if _, err := s.Revise(ctx, "id", "invoices are retained for 90 days", first); err != nil {
		t.Fatal(err)
	}
	// An alias, which must expand the query and never appear in a result.
	if _, err := s.PutAlias(ctx, "id", "clearinghouse", []string{"payments"}); err != nil {
		t.Fatal(err)
	}
	return s
}
