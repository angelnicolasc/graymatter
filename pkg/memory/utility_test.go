package memory

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestUtilitySearchScopesAndIdentity(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	for _, row := range []struct{ ns, text string }{{"a", "deployment project alpha"}, {SharedAgentID, "deployment shared beta"}, {"b", "deployment private forbidden"}} {
		if err := s.Put(ctx, row.ns, row.text); err != nil {
			t.Fatal(err)
		}
	}
	want, err := s.RecallAll(ctx, "a", "deployment", 8)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SearchScoped(ctx, "a", []string{"deployment"}, 8, true, RecallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, f := range got.Facts {
		texts = append(texts, f.Text)
		if f.ID == "" || f.Namespace == "b" {
			t.Fatalf("invalid identity %+v", f)
		}
	}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("ranking changed: %v != %v", texts, want)
	}
	scoped, err := s.SearchScoped(ctx, "a", []string{"deployment"}, 8, false, RecallOptions{})
	if err != nil || len(scoped.Facts) != 1 || scoped.Facts[0].Namespace != "a" {
		t.Fatalf("project scope %+v %v", scoped, err)
	}
	shared, err := s.SearchScoped(ctx, SharedAgentID, []string{"deployment"}, 8, true, RecallOptions{})
	if err != nil || len(shared.Facts) != 1 || shared.Facts[0].Namespace != SharedAgentID {
		t.Fatalf("shared scope %+v %v", shared, err)
	}
	batch, err := s.SearchScoped(ctx, "a", []string{"deployment", "deployment"}, 8, true, RecallOptions{})
	if err != nil || len(batch.Facts) != 2 || len(batch.PerQuery) != 2 || !reflect.DeepEqual(batch.PerQuery[0], batch.PerQuery[1]) {
		t.Fatalf("batch %+v %v", batch, err)
	}
}

func TestUtilitySearchOmitsUnscopedGraphHints(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Put(ctx, "a", "routing public project fact"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "b", "routing foreign secret"); err != nil {
		t.Fatal(err)
	}
	graph := &confidenceHintGraph{labels: []string{"routing foreign secret"}}
	extractor := &recordingExtractor{}
	s.SetKG(graph, extractor)
	got, err := s.SearchScoped(ctx, "a", []string{"routing"}, 8, true, RecallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 || got.Facts[0].ID == "" || got.Facts[0].Namespace != "a" || got.GraphHints == "" || graph.calls != 0 || extractor.calls != 0 {
		t.Fatalf("unscoped graph reached compact search: %+v graph=%d extractor=%d", got, graph.calls, extractor.calls)
	}
	legacy, err := s.Recall(ctx, "a", "routing", 8)
	if err != nil || len(legacy) != 2 || legacy[1] != "routing foreign secret" || graph.calls != 2 {
		t.Fatalf("legacy graph behavior changed: %v %v calls=%d", legacy, err, graph.calls)
	}
}

func TestUtilityPutManyAtomicValidationReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir, CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutManyReturningFacts(context.Background(), "a", []WriteFact{{Text: "first"}, {Text: " "}}); err == nil {
		t.Fatal("accepted invalid member")
	}
	rows, _ := s.List("a")
	if len(rows) != 0 {
		t.Fatal("partial batch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.PutManyReturningFacts(ctx, "a", []WriteFact{{Text: "cancelled"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
	facts, err := s.PutManyReturningFacts(context.Background(), "a", []WriteFact{{Text: "first atomic"}, {Text: "second atomic"}})
	if err != nil || len(facts) != 2 || facts[0].ID == facts[1].ID {
		t.Fatalf("write %+v %v", facts, err)
	}
	s.Close()
	s, err = Open(StoreConfig{DataDir: dir, CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err = s.List("a")
	if err != nil || len(rows) != 2 {
		t.Fatalf("reopen %v %v", rows, err)
	}
	got, err := s.SearchScoped(context.Background(), "a", []string{"atomic"}, 8, false, RecallOptions{})
	if err != nil || len(got.Facts) != 2 {
		t.Fatalf("index not committed %+v %v", got, err)
	}
}

func TestUtilityPutManyTransactionFailure(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A malformed index root makes durable index insertion fail inside tx.
	if err = s.db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists(bucketIdxTerms)
		if e != nil {
			return e
		}
		return b.Put([]byte("a"), []byte("not a bucket"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutManyReturningFacts(context.Background(), "a", []WriteFact{{Text: "first atomic"}, {Text: "second atomic"}}); err == nil {
		t.Fatal("expected tx failure")
	}
	rows, _ := s.List("a")
	if len(rows) != 0 {
		t.Fatalf("rollback left rows %+v", rows)
	}
}

func TestUtilityPutManyConcurrentIdentities(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			facts, e := s.PutManyReturningFacts(context.Background(), "a", []WriteFact{{Text: "one"}, {Text: "two"}})
			if e != nil {
				t.Error(e)
				return
			}
			for _, f := range facts {
				ids <- f.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] || id == "" {
			t.Fatal("repeated identity")
		}
		seen[id] = true
	}
	if len(seen) != 8 {
		t.Fatalf("identities %d", len(seen))
	}
}

type utilityEmbedder struct{ fn func(string) []float32 }

func (p *utilityEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return p.fn(text), nil
}
func (*utilityEmbedder) Name() string    { return "utility-local" }
func (*utilityEmbedder) Dimensions() int { return 2 }

func TestUtilityPutManySnapshotsCallerOptions(t *testing.T) {
	confidence := "verified"
	input := []WriteFact{{Text: "first", Options: WriteOptions{Confidence: &confidence}}, {Text: "second"}}
	provider := &utilityEmbedder{fn: func(string) []float32 { confidence = "invalid"; input[1].Text = "changed after validation"; return nil }}
	s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	facts, err := s.PutManyReturningFacts(context.Background(), "a", input)
	if err != nil || facts[0].Confidence != "verified" || facts[1].Text != "second" {
		t.Fatalf("mutable inputs affected commit: %+v %v", facts, err)
	}
}

func TestUtilityPutManyRollbackAfterFirstRow(t *testing.T) {
	provider := &utilityEmbedder{fn: func(text string) []float32 {
		if text == "second" {
			return []float32{1, 0}
		}
		return nil
	}}
	s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPendingVector).Put([]byte("a"), []byte("invalid namespace"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutManyReturningFacts(context.Background(), "a", []WriteFact{{Text: "first"}, {Text: "second"}}); err == nil {
		t.Fatal("expected failure after inserting rows")
	}
	rows, err := s.List("a")
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed second member retained partial batch: %+v %v", rows, err)
	}
}
