package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func confidenceRecallStore(t *testing.T, indexed bool) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: t.TempDir(), CandidateRetrieval: indexed})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	return s
}

func confidenceRecallSeed(t *testing.T, s *Store, agent, text, confidence string, day int) Fact {
	t.Helper()
	f, err := s.PutReturningFact(context.Background(), agent, text)
	if err != nil {
		t.Fatal(err)
	}
	f.Confidence = confidence
	f.CreatedAt = s.now().Add(time.Duration(day) * 24 * time.Hour)
	if err := s.UpdateFact(agent, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func confidenceRecallCorpus(t *testing.T, s *Store) []Fact {
	t.Helper()
	var facts []Fact
	for i, row := range []struct{ text, confidence string }{
		{"orchard routing routing", "unverified"},
		{"routing cutoff", "verified"},
		{"orchard pruning", "inferred"},
		{"routing tier", ""},
		{"orchard observations", "historical-unknown"},
		{"routing cutoff", "verified"},
	} {
		facts = append(facts, confidenceRecallSeed(t, s, "confidence", row.text, row.confidence, i))
	}
	old := confidenceRecallSeed(t, s, "confidence", "orchard routing retired", "unverified", 7)
	old.SupersededBy = facts[1].ID
	if err := s.UpdateFact("confidence", old); err != nil {
		t.Fatal(err)
	}
	alias := confidenceRecallSeed(t, s, "confidence", "unrelated = vocabulary", "verified", 8)
	alias.Kind = KindAlias
	if err := s.UpdateFact("confidence", alias); err != nil {
		t.Fatal(err)
	}
	return facts
}

type independentlyRankedConfidence struct {
	fact             Fact
	keyword, recency int
	base, final      float64
}

// This deliberately does not call the production filter, tokeniser, scorer,
// factor or fusion. The fixture uses lowercase whitespace tokens without
// stopwords so its hand-specified vocabulary is independent of those helpers.
func independentConfidenceExpected(facts []Fact, query string, minConfidence *string, weight float64, signals SignalWeights, floor float64) []independentlyRankedConfidence {
	level := func(c string) int {
		switch c {
		case "verified":
			return 2
		case "", "inferred":
			return 1
		default:
			return 0
		}
	}
	var live []Fact
	for _, f := range facts {
		if minConfidence == nil || level(f.Confidence) >= level(*minConfidence) {
			live = append(live, f)
		}
	}
	before := func(a, b Fact) bool {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	}
	sort.Slice(live, func(i, j int) bool {
		if !live[i].CreatedAt.Equal(live[j].CreatedAt) {
			return live[i].CreatedAt.After(live[j].CreatedAt)
		}
		return live[i].ID < live[j].ID
	})
	df := map[string]int{}
	frequencies := map[string]map[string]int{}
	for _, f := range live {
		tf := map[string]int{}
		for _, token := range strings.Fields(f.Text) {
			tf[token]++
		}
		frequencies[f.ID] = tf
		for token := range tf {
			df[token]++
		}
	}
	type keyword struct {
		fact  Fact
		score float64
	}
	var kw []keyword
	for _, f := range live {
		score := 0.0
		for _, token := range strings.Fields(query) {
			score += float64(frequencies[f.ID][token]) * math.Log(float64(len(live)+1)/float64(df[token]+1))
		}
		if score > 0 {
			kw = append(kw, keyword{f, score / float64(len(strings.Fields(f.Text))+1)})
		}
	}
	sort.Slice(kw, func(i, j int) bool {
		if kw[i].score != kw[j].score {
			return kw[i].score > kw[j].score
		}
		return before(kw[i].fact, kw[j].fact)
	})
	ranks := map[string]int{}
	for i, e := range kw {
		ranks[e.fact.ID] = i + 1
	}
	out := make([]independentlyRankedConfidence, 0, len(live))
	for i, f := range live {
		base := signals.Recency / (60 + float64(i+1))
		if r := ranks[f.ID]; r > 0 {
			base += signals.Keyword / (60 + float64(r))
		}
		out = append(out, independentlyRankedConfidence{f, ranks[f.ID], i + 1, base, base * (1 + weight*float64(level(f.Confidence)-1))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].final != out[j].final {
			return out[i].final > out[j].final
		}
		return before(out[i].fact, out[j].fact)
	})
	if len(out) > 0 && floor > 0 {
		threshold := floor * out[0].final
		for i, e := range out {
			if e.final < threshold {
				out = out[:i]
				break
			}
		}
	}
	return out
}

func TestConfidenceRecallIndependentArithmeticAndFilter(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			s := confidenceRecallStore(t, indexed)
			facts := confidenceRecallCorpus(t, s)
			for _, weights := range []SignalWeights{{Keyword: 1, Recency: .5}, {Keyword: .6, Recency: .2}, {Recency: 1}, {Keyword: 1}, {}} {
				s.cfg.SignalWeights = &weights
				for _, relevance := range []float64{0, .9} {
					s.cfg.MinRelevance = relevance
					for _, filter := range []string{"none", "unverified", "inferred", "verified"} {
						var minimum *string
						if filter != "none" {
							minimum = &filter
						}
						for _, weight := range []float64{0, .2, .5} {
							opts := RecallOptions{MinConfidence: minimum, ConfidenceWeight: &weight}
							want := independentConfidenceExpected(facts, "orchard routing", minimum, weight, weights, relevance)
							for _, topK := range []int{1, 0, 3, 50, int(^uint(0) >> 1)} {
								got, err := s.RecallExplainWithOptions(context.Background(), "confidence", "orchard routing", topK, opts)
								if err != nil {
									t.Fatal(err)
								}
								seen := map[string]bool{}
								var selected []independentlyRankedConfidence
								limit := topK
								if limit <= 0 {
									limit = 8
								}
								for _, row := range want {
									if seen[row.fact.Text] {
										continue
									}
									seen[row.fact.Text] = true
									selected = append(selected, row)
									if len(selected) >= limit {
										break
									}
								}
								if len(got.Receipts) != len(selected) {
									t.Fatalf("filter=%s weight=%g topK=%d got=%d expected=%d", filter, weight, topK, len(got.Receipts), len(selected))
								}
								for i, r := range got.Receipts {
									e := selected[i]
									if r.Provenance.FactID != e.fact.ID || r.Ranks.KeywordRank != e.keyword || r.Ranks.RecencyRank != e.recency {
										t.Fatalf("filter=%s weight=%g receipt=%+v expected=%+v", filter, weight, r, e)
									}
									if r.Ranking == nil || math.Abs(r.Ranks.FusedScore-e.base) > 1e-15 || math.Abs(r.Ranking.FinalScore-e.final) > 1e-15 {
										t.Fatalf("non-reconstructible receipt: %+v expected=%+v", r, e)
									}
									if r.Ranking.BaseScore != r.Ranks.FusedScore || r.Ranking.FinalScore != r.Ranks.FusedScore*r.Ranking.Factor || r.Ranking.ConfidenceWeight != weight || r.Ranking.Policy != ConfidencePolicy {
										t.Fatalf("ranking fields disagree: %+v", r)
									}
								}
							}
						}
					}
				}
			}
		})
	}
}

func TestConfidenceRecallLegacyNeutralityAndUniformOrder(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		s := confidenceRecallStore(t, indexed)
		var facts []Fact
		for i, text := range []string{"routing cutoff", "orchard pruning", "routing tier", "orchard observations"} {
			facts = append(facts, confidenceRecallSeed(t, s, "neutral", text, "", i))
		}
		legacy, err := s.RecallExplain(context.Background(), "neutral", "orchard routing", 8)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range legacy {
			if r.Ranking != nil {
				t.Fatal("legacy receipt included confidence ranking")
			}
		}
		for _, category := range []string{"", "inferred", "verified", "unverified"} {
			for i := range facts {
				facts[i].Confidence = category
				if err := s.UpdateFact("neutral", facts[i]); err != nil {
					t.Fatal(err)
				}
			}
			for _, weight := range []float64{0, .1, .5} {
				got, err := s.RecallExplainWithOptions(context.Background(), "neutral", "orchard routing", 8, RecallOptions{ConfidenceWeight: &weight})
				if err != nil {
					t.Fatal(err)
				}
				for i, r := range got.Receipts {
					if r.Provenance.FactID != legacy[i].Provenance.FactID || r.Ranks != legacy[i].Ranks {
						t.Fatalf("uniform category changed base/order: %s %+v", category, r)
					}
				}
			}
		}
		old, _, err := s.RecallDetailed(context.Background(), "neutral", "routing", 0)
		if err != nil || len(old) != 0 {
			t.Fatalf("legacy zero changed: %v %v", old, err)
		}
		newResult, err := s.RecallWithOptions(context.Background(), "neutral", "routing", 0, RecallOptions{})
		if err != nil || len(newResult.Facts) != 4 {
			t.Fatalf("new default topK: %+v %v", newResult, err)
		}
		if newResult.Retrieval != nil {
			t.Fatal("legacy options omitted metadata")
		}
	}
}

func TestConfidenceRecallLineageAndConfidenceOnlyInvalidation(t *testing.T) {
	s := confidenceRecallStore(t, true)
	facts := confidenceRecallCorpus(t, s)
	minimum := "verified"
	r, err := s.RecallExplainWithOptions(context.Background(), "confidence", "orchard routing", 50, RecallOptions{MinConfidence: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Receipts) != 1 {
		t.Fatalf("duplicate verified texts: %+v", r)
	}
	if r.Receipts[0].Provenance.FactID != facts[5].ID {
		t.Fatal("dedup receipt chose wrong canonical ID")
	}
	// Excluding the duplicate makes the replacement itself observable. Its
	// unverified tombstoned predecessor remains in the historical lineage.
	facts[5].Confidence = "unverified"
	if err := s.UpdateFact("confidence", facts[5]); err != nil {
		t.Fatal(err)
	}
	r, err = s.RecallExplainWithOptions(context.Background(), "confidence", "orchard routing", 50, RecallOptions{MinConfidence: &minimum})
	if err != nil || len(r.Receipts) != 1 || r.Receipts[0].Provenance.FactID != facts[1].ID || len(r.Receipts[0].Provenance.Supersedes) != 1 {
		t.Fatalf("lost filtered predecessor lineage: %+v %v", r, err)
	}
	facts[2].Confidence = "verified"
	if err := s.UpdateFact("confidence", facts[2]); err != nil {
		t.Fatal(err)
	}
	r, err = s.RecallExplainWithOptions(context.Background(), "confidence", "orchard routing", 50, RecallOptions{MinConfidence: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Receipts) != 2 || r.Receipts[1].Provenance.FactID != facts[2].ID {
		t.Fatalf("confidence-only update stale: %+v", r)
	}
	facts[2].Confidence = "unverified"
	if err := s.UpdateFact("confidence", facts[2]); err != nil {
		t.Fatal(err)
	}
	r, err = s.RecallExplainWithOptions(context.Background(), "confidence", "orchard routing", 50, RecallOptions{MinConfidence: &minimum})
	if err != nil || len(r.Receipts) != 1 {
		t.Fatalf("confidence-only demotion stale: %+v %v", r, err)
	}
}

func TestConfidenceRecallScanIndexPlainExplainParity(t *testing.T) {
	for _, stemmed := range []bool{false, true} {
		s := confidenceRecallStore(t, true)
		confidenceRecallCorpus(t, s)
		s.cfg.StemKeywords = stemmed
		for _, signals := range []SignalWeights{{Keyword: 1, Recency: .5}, {Keyword: 1}, {Recency: 1}} {
			s.cfg.SignalWeights = &signals
			for _, floor := range []float64{0, .95} {
				s.cfg.MinRelevance = floor
				for _, filter := range []string{"none", "unverified", "inferred", "verified"} {
					var minimum *string
					if filter != "none" {
						minimum = &filter
					}
					weight := .3
					opts := RecallOptions{MinConfidence: minimum, ConfidenceWeight: &weight}
					for _, query := range []string{"orchard routing", "orchards routes", "unknown phrase", "unrelated"} {
						s.cfg.CandidateRetrieval = false
						scan, err := s.RecallWithOptions(context.Background(), "confidence", query, 8, opts)
						if err != nil {
							t.Fatal(err)
						}
						explained, err := s.RecallExplainWithOptions(context.Background(), "confidence", query, 8, opts)
						if err != nil {
							t.Fatal(err)
						}
						s.cfg.CandidateRetrieval = true
						indexed, err := s.RecallWithOptions(context.Background(), "confidence", query, 8, opts)
						if err != nil {
							t.Fatal(err)
						}
						iReceipts, err := s.RecallExplainWithOptions(context.Background(), "confidence", query, 8, opts)
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(scan, indexed) || len(explained.Receipts) != len(scan.Facts) || len(iReceipts.Receipts) != len(scan.Facts) {
							t.Fatalf("parity mismatch stem=%t filter=%s q=%q scan=%+v indexed=%+v", stemmed, filter, query, scan, indexed)
						}
						for i, r := range explained.Receipts {
							ir := iReceipts.Receipts[i]
							if r.Text != scan.Facts[i] || r.Provenance.FactID != ir.Provenance.FactID || r.Ranks.KeywordRank != ir.Ranks.KeywordRank || r.Ranks.RecencyRank != ir.Ranks.RecencyRank || math.Abs(r.Ranks.FusedScore-ir.Ranks.FusedScore) > 1e-15 || math.Abs(r.Ranking.FinalScore-ir.Ranking.FinalScore) > 1e-15 {
								t.Fatalf("receipt parity mismatch: %+v %+v", r, ir)
							}
						}
					}
				}
			}
		}
	}
}

type confidenceHintGraph struct {
	calls  int
	labels []string
}

func (*confidenceHintGraph) UpsertNode(string, string, string) error { return nil }
func (g *confidenceHintGraph) NeighborTexts(string, int) ([]string, error) {
	g.calls++
	return g.labels, nil
}

func TestConfidenceRecallKGFeedbackAndLearningDoNotLeak(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		s := confidenceRecallStore(t, indexed)
		confidenceRecallSeed(t, s, "hints", "routing archive retention", "verified", 0)
		confidenceRecallSeed(t, s, "hints", "routing secretneedle concealed", "unverified", 1)
		graph := &confidenceHintGraph{labels: []string{"secretneedle graph hint"}}
		s.SetKG(graph, &recordingExtractor{})
		s.cfg.UsageAliasLearning = true
		for _, minimum := range []string{"inferred", "verified", "unverified"} {
			graph.calls = 0
			r, err := s.RecallWithOptions(context.Background(), "hints", "obscureword", 8, RecallOptions{MinConfidence: &minimum})
			if err != nil {
				t.Fatal(err)
			}
			if graph.calls != 0 || r.Retrieval.KG != "suppressed_min_confidence" {
				t.Fatalf("filtered graph enrichment: %+v calls=%d", r, graph.calls)
			}
			if minimum != "unverified" && strings.Contains(strings.Join(r.Facts, " ")+r.Feedback, "secretneedle") {
				t.Fatalf("excluded vocabulary leaked: %+v", r)
			}
			if r.Feedback == "" {
				t.Fatal("fixture needs weak-match feedback")
			}
			if _, exists := s.peekPendingMiss("hints"); exists {
				t.Fatal("filtered recall learned a pending alias miss")
			}
		}
		weight := .2
		r, err := s.RecallWithOptions(context.Background(), "hints", "routing", 8, RecallOptions{ConfidenceWeight: &weight})
		if err != nil {
			t.Fatal(err)
		}
		if graph.calls == 0 || len(r.Facts) != 3 || r.Retrieval.KG != "hints_without_confidence_receipts" {
			t.Fatalf("unfiltered graph contract changed: %+v calls=%d", r, graph.calls)
		}
	}
}

func TestConfidenceRecallUnknownEffectiveLabelAndEmptyMetadata(t *testing.T) {
	s := confidenceRecallStore(t, true)
	f := confidenceRecallSeed(t, s, "unknown", "unknown observations", "historical-unknown", 0)
	weight := .2
	r, err := s.RecallExplainWithOptions(context.Background(), "unknown", "observations", 8, RecallOptions{ConfidenceWeight: &weight})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Receipts) != 1 || r.Receipts[0].Ranking.EffectiveConfidence != "unverified" || r.Receipts[0].Provenance.Confidence != f.Confidence || r.Receipts[0].Ranking.Factor != .8 {
		t.Fatalf("unknown label promoted/lost: %+v", r)
	}
	minConfidence := "verified"
	empty, err := s.RecallWithOptions(context.Background(), "unknown", "observations", 8, RecallOptions{MinConfidence: &minConfidence, ConfidenceWeight: &weight})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Facts) != 0 || empty.Retrieval == nil || !strings.Contains(empty.Retrieval.Text(), "min_confidence=verified") || empty.Retrieval.KG != "suppressed_min_confidence" {
		t.Fatalf("empty metadata: %+v", empty)
	}
}

func TestConfidenceRecallBatchSharedIsolationAndConcurrentWeights(t *testing.T) {
	s := confidenceRecallStore(t, true)
	confidenceRecallSeed(t, s, "namespace", "routing checked", "verified", 0)
	confidenceRecallSeed(t, s, "namespace", "routing unchecked", "unverified", 1)
	confidenceRecallSeed(t, s, SharedAgentID, "shared checked", "verified", 0)
	confidenceRecallSeed(t, s, "foreign", "foreign checked", "verified", 0)
	minimum := "verified"
	weight := .5
	opts := RecallOptions{MinConfidence: &minimum, ConfidenceWeight: &weight}
	batch, err := s.BatchRecallWithOptions(context.Background(), "namespace", []string{"routing", "unchecked"}, 8, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || !reflect.DeepEqual(MergedFacts(batch), []string{"routing checked"}) {
		t.Fatalf("batch filter escaped: %+v", batch)
	}
	for _, r := range batch {
		if r.Retrieval == nil || r.Retrieval.ConfidenceWeight != weight {
			t.Fatalf("batch policy lost: %+v", r)
		}
	}
	all, err := s.RecallAllWithOptions(context.Background(), "namespace", "routing", 8, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Facts) != 2 || !reflect.DeepEqual(all.Facts, []string{"routing checked", "shared checked"}) {
		t.Fatalf("namespace merge escaped: %+v", all)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := 0.0
			want := "routing unchecked"
			if i%2 == 1 {
				w = .5
				want = "routing checked"
			}
			r, err := s.RecallWithOptions(context.Background(), "namespace", "routing", 1, RecallOptions{ConfidenceWeight: &w})
			if err != nil || len(r.Facts) != 1 || r.Facts[0] != want {
				t.Errorf("cross-query policy leak: %+v %v", r, err)
			}
		}(i)
	}
	wg.Wait()
	if s.cfg.ConfidenceWeight != 0 {
		t.Fatal("query mutated store default")
	}
}

func TestConfidenceRecallAllTouchesOnlyFinalCanonicalResults(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("indexed=%t/duplicate=%t", indexed, duplicate), func(t *testing.T) {
				s := confidenceRecallStore(t, indexed)
				selected := confidenceRecallSeed(t, s, "all-access", "routing selected", "verified", 0)
				confidenceRecallSeed(t, s, "all-access", "routing excluded", "unverified", 1)
				sharedText := "routing shared"
				if duplicate {
					sharedText = selected.Text
				}
				confidenceRecallSeed(t, s, SharedAgentID, sharedText, "verified", 0)
				confidenceRecallSeed(t, s, "foreign", "routing foreign", "verified", 0)
				minimum := "verified"
				weight := .2
				result, err := s.RecallAllWithOptions(context.Background(), "all-access", "routing", 1, RecallOptions{MinConfidence: &minimum, ConfidenceWeight: &weight})
				if err != nil || !reflect.DeepEqual(result.Facts, []string{selected.Text}) {
					t.Fatalf("merged result: %+v / %v", result, err)
				}
				for _, namespace := range []string{"all-access", SharedAgentID, "foreign"} {
					facts, err := s.List(namespace)
					if err != nil {
						t.Fatal(err)
					}
					for _, fact := range facts {
						want := 0
						if fact.ID == selected.ID {
							want = 1
						}
						if fact.AccessCount != want {
							t.Fatalf("access updated for discarded/duplicate fact: %+v; want %d", fact, want)
						}
					}
				}
			})
		}
	}
}

func TestConfidenceLegacyAllTouchesOnlyFinalCanonicalResults(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("indexed=%t/duplicate=%t", indexed, duplicate), func(t *testing.T) {
				s := confidenceRecallStore(t, indexed)
				selected := confidenceRecallSeed(t, s, "legacy-all-access", "routing selected", "", 0)
				sharedText := "routing discarded shared"
				if duplicate {
					sharedText = selected.Text
				}
				shared := confidenceRecallSeed(t, s, SharedAgentID, sharedText, "", 0)
				result, err := s.RecallAll(context.Background(), "legacy-all-access", "routing", 1)
				if err != nil || !reflect.DeepEqual(result, []string{selected.Text}) {
					t.Fatalf("legacy merge selection changed: %v / %v", result, err)
				}
				for _, topK := range []int{0, -1} {
					result, err = s.RecallAll(context.Background(), "legacy-all-access", "routing", topK)
					if err != nil || len(result) != 0 {
						t.Fatalf("legacy nonpositive topK changed: %v / %v", result, err)
					}
				}
				for _, expected := range []struct {
					fact   Fact
					access int
				}{{selected, 1}, {shared, 0}} {
					facts, err := s.List(expected.fact.AgentID)
					if err != nil || len(facts) != 1 || facts[0].ID != expected.fact.ID || facts[0].AccessCount != expected.access {
						t.Fatalf("legacy merge touched discarded/duplicate identity: %+v / %v; want id=%s access=%d", facts, err, expected.fact.ID, expected.access)
					}
				}
			})
		}
	}
}

func TestConfidenceLegacyAllRetainsConfiguredDefault(t *testing.T) {
	s := confidenceRecallStore(t, true)
	s.cfg.ConfidenceWeight = .2
	s.cfg.SignalWeights = &SignalWeights{Recency: 1}
	verified := confidenceRecallSeed(t, s, "legacy-all-default", "verified policy", "verified", 0)
	unchecked := confidenceRecallSeed(t, s, "legacy-all-default", "unverified policy", "unverified", 1)
	result, err := s.RecallAll(context.Background(), "legacy-all-default", "policy", 1)
	if err != nil || !reflect.DeepEqual(result, []string{verified.Text}) {
		t.Fatalf("legacy all lost configured default: %v / %v", result, err)
	}
	zero := 0.0
	explicit, err := s.RecallAllWithOptions(context.Background(), "legacy-all-default", "policy", 1, RecallOptions{ConfidenceWeight: &zero})
	if err != nil || !reflect.DeepEqual(explicit.Facts, []string{unchecked.Text}) || explicit.Retrieval.ConfidenceWeight != 0 {
		t.Fatalf("explicit zero did not override configured all default: %+v / %v", explicit, err)
	}
}

func TestConfidenceIndexUpgradeAndReadOnlyFallback(t *testing.T) {
	s := confidenceRecallStore(t, true)
	confidenceRecallSeed(t, s, "upgrade", "verified archive", "verified", 0)
	confidenceRecallSeed(t, s, "upgrade", "unverified archive", "unverified", 1)
	minimum := "verified"
	// This unit test models unusable index metadata. The binary compatibility
	// gate separately runs the real previous and current writers on one store.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		st, _ := idxReadState(tx, "upgrade")
		st.Version = 2
		data, _ := json.Marshal(st)
		return tx.Bucket(bucketIdxMeta).Put([]byte("upgrade"), data)
	}); err != nil {
		t.Fatal(err)
	}
	path := s.cfg.DataDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(StoreConfig{DataDir: path, ReadOnly: true, CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := ro.RecallWithOptions(context.Background(), "upgrade", "archive", 8, RecallOptions{MinConfidence: &minimum})
	if err != nil || !reflect.DeepEqual(r.Facts, []string{"verified archive"}) {
		t.Fatalf("read-only old index fallback: %+v %v", r, err)
	}
	if err := ro.db.View(func(tx *bolt.Tx) error {
		st, _ := idxReadState(tx, "upgrade")
		if st.Version != 2 {
			t.Fatal("read-only query rebuilt index")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = ro.Close()
	rw, err := Open(StoreConfig{DataDir: path, CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rw.Close() })
	r, err = rw.RecallWithOptions(context.Background(), "upgrade", "archive", 8, RecallOptions{MinConfidence: &minimum})
	if err != nil || !reflect.DeepEqual(r.Facts, []string{"verified archive"}) {
		t.Fatalf("index rebuild: %+v %v", r, err)
	}
	if err := rw.db.View(func(tx *bolt.Tx) error {
		st, _ := idxReadState(tx, "upgrade")
		if st.Version != 4 {
			t.Fatalf("index version=%d", st.Version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
