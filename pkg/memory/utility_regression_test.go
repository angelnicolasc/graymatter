package memory

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestUtilitySearchPreservesVectorRecoveryMetadata(t *testing.T) {
	fixture := makeNativeRecoveryFixture(t)
	cfg := fixture.cfg
	cfg.ReadOnly, cfg.StrictWrite = true, false
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tc := range []struct {
		fact     Fact
		fallback string
	}{{fixture.replacement, "pending_vector_index"}, {fixture.other, "vector_index_unavailable"}} {
		got, err := s.SearchScoped(context.Background(), tc.fact.AgentID, []string{"signal"}, 8, false, RecallOptions{})
		if err != nil || len(got.Facts) != 1 || got.Facts[0].ID != tc.fact.ID || got.Facts[0].Receipt == nil || got.Facts[0].Receipt.Ranks.VectorRank != 0 {
			t.Fatalf("canonical fallback result=%+v err=%v", got, err)
		}
		requireNativeRecoveryFallback(t, got.Retrieval, tc.fallback)
	}
	if s.PendingVectorCount() != 1 {
		t.Fatal("read-only search changed pending delivery")
	}
}

func TestUtilitySearchLineageContentChangedClearsSupersedes(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "original policy evidence")
	head := reviseLineageFact(t, s, "current policy evidence", source)
	if err := s.db.Update(func(tx *bolt.Tx) error { return writeDivergentBackupHead(tx, head) }); err != nil {
		t.Fatal(err)
	}
	got, err := s.SearchScoped(context.Background(), lineageAgent, []string{"policy"}, 8, false, RecallOptions{})
	if err != nil || len(got.Facts) != 1 || got.Facts[0].ID != head.ID || got.Facts[0].Receipt == nil {
		t.Fatalf("divergent head result=%+v err=%v", got, err)
	}
	p := got.Facts[0].Receipt.Provenance
	if p.LineageStatus != LineageContentChanged || len(p.Supersedes) != 0 || len(p.Replaces) != 0 {
		t.Fatalf("divergent content claims predecessor support: %+v", p)
	}
}

func TestUtilitySearchLineageSurvivesPrunedIntermediates(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed-%t", indexed), func(t *testing.T) {
			s := openLineageStore(t, t.TempDir(), indexed)
			source := putLineageFact(t, s, "original shipment policy")
			middle := reviseLineageFact(t, s, "intermediate shipment policy", source)
			head := reviseLineageFact(t, s, "current shipment policy", middle)
			pruneLineageRow(t, s, source.ID)
			pruneLineageRow(t, s, middle.ID)
			got, err := s.SearchScoped(context.Background(), lineageAgent, []string{"shipment"}, 8, true, RecallOptions{})
			if err != nil || len(got.Facts) != 1 || got.Facts[0].ID != head.ID || got.Facts[0].Receipt == nil {
				t.Fatalf("pruned chain result=%+v err=%v", got, err)
			}
			p := got.Facts[0].Receipt.Provenance
			if p.LineageStatus != LineageVerified || len(p.Supersedes) != 2 || !slices.Contains(p.Supersedes, source.ID) || !slices.Contains(p.Supersedes, middle.ID) {
				t.Fatalf("pruned chain lost authoritative provenance: %+v", p)
			}
		})
	}
}

func TestUtilitySearchSnapshotsQueriesBeforeCallbacks(t *testing.T) {
	queries := []string{"alpha", "beta"}
	var seen []string
	s, err := Open(StoreConfig{DataDir: t.TempDir(), OnRecall: func(_ string, query string, _ int, _ time.Duration) {
		seen = append(seen, query)
		queries[1] = "mutated after input validation"
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Put(context.Background(), "a", "alpha beta durable context"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchScoped(context.Background(), "a", queries, 8, false, RecallOptions{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"alpha", "beta"}) {
		t.Fatalf("callback changed the validated search batch: %v", seen)
	}
}

func TestUtilitySearchRejectsMixedContentVersions(t *testing.T) {
	for _, changeText := range []bool{false, true} {
		t.Run(fmt.Sprintf("change-text-%t", changeText), func(t *testing.T) {
			s, err := Open(StoreConfig{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			confidence := "verified"
			written, err := s.PutManyReturningFacts(ctx, "a", []WriteFact{{Text: "deployment policy original", Options: WriteOptions{Confidence: &confidence}}})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.cfg.OnRecall = func(_ string, _ string, _ int, _ time.Duration) {
				calls++
				if calls != 1 {
					return
				}
				changed := written[0]
				changed.Confidence = "unverified"
				if changeText {
					changed.Text = "deployment policy changed"
				}
				if err := s.UpdateFact("a", changed); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.SearchScoped(ctx, "a", []string{"deployment", "policy"}, 8, false, RecallOptions{})
			if err == nil || !strings.Contains(err.Error(), "content changed") || len(got.Facts) != 0 || calls != 2 {
				t.Fatalf("mixed versions must fail visibly without replay: result=%+v err=%v calls=%d", got, err, calls)
			}
			current, ok := s.loadFact("a", written[0].ID)
			if !ok || current.AccessCount != 0 {
				t.Fatalf("failed search touched fact: %+v found=%v", current, ok)
			}
		})
	}
}
