package memory

import (
	"context"
	"errors"
	"reflect"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFactSummaryStreamingMatchesLegacyAndDoesNotWrite(t *testing.T) {
	s := inspectionStore(t, true)
	ctx := context.Background()
	first := inspectionPut(t, s, "a", "live fact")
	retired := inspectionPut(t, s, "a", "retired fact")
	alias, err := s.putReturningFactKind(ctx, "a", "db = database", KindAlias, "")
	if err != nil {
		t.Fatal(err)
	}
	first.Weight = .5
	first.AccessCount = 4
	retired.Weight = .25
	retired.AccessCount = 99
	retired.SupersededBy = SupersededByAgent
	alias.Weight = .75
	alias.AccessCount = 2
	for _, f := range []Fact{first, retired, alias} {
		if err := s.UpdateFact("a", f); err != nil {
			t.Fatal(err)
		}
	}
	txID := inspectionTxID(t, s)
	streamed, err := s.SummarizeFacts(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Hide the optional capability to exercise backward-compatible fallback.
	legacy, err := ReadFactSummary(ctx, struct{ listOnlyFacts }{s}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if streamed.All.LiveEvidenceCount == nil || *streamed.All.LiveEvidenceCount != 0 || streamed.Live.LiveEvidenceCount == nil || *streamed.Live.LiveEvidenceCount != 0 {
		t.Fatalf("new store must report known zero evidence: %+v", streamed)
	}
	if legacy.All.LiveEvidenceCount != nil || legacy.Live.LiveEvidenceCount != nil {
		t.Fatalf("list-only fallback must leave evidence count unavailable: %+v", legacy)
	}
	// This optional capability is intentionally absent from the legacy adapter.
	comparable := streamed
	comparable.All.LiveEvidenceCount, comparable.Live.LiveEvidenceCount = nil, nil
	if !reflect.DeepEqual(comparable, legacy) {
		t.Fatalf("summary parity: %+v %+v", comparable, legacy)
	}
	if streamed.All.FactCount != 3 || streamed.Live.FactCount != 2 || streamed.Tombstones != 1 || streamed.Recalls != 6 || streamed.Live.AvgWeight != .625 {
		t.Fatalf("summary: %+v", streamed)
	}
	stats, err := s.Stats("a")
	if err != nil || !reflect.DeepEqual(stats, streamed.All) {
		t.Fatalf("stats: %+v %v", stats, err)
	}
	if inspectionTxID(t, s) != txID {
		t.Fatal("aggregation wrote to database")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.SummarizeFacts(cancelled, "a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type listOnlyFacts interface{ List(string) ([]Fact, error) }

func TestFactSummarySkipsTextAndEmbeddingDecoding(t *testing.T) {
	s := inspectionStore(t, false)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte("a"))
		if err != nil {
			return err
		}
		return b.Put([]byte("id"), []byte(`{"id":"id","agent_id":"a","text":{"not":"text"},"embedding":{"not":"vector"},"weight":0.5,"access_count":3}`))
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Stats("a")
	if err != nil || stats.FactCount != 1 || stats.AvgWeight != .5 {
		t.Fatalf("summary decoded unused payloads: %+v %v", stats, err)
	}
}
