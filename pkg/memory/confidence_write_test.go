package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func confidenceOptions(value string) WriteOptions { return WriteOptions{Confidence: &value} }

func mustConfidenceFact(t *testing.T, s *Store, agent, text, confidence string) Fact {
	t.Helper()
	options := WriteOptions{}
	if confidence != "" {
		options = confidenceOptions(confidence)
	}
	f, err := s.PutWithOptionsReturningFact(context.Background(), agent, text, options)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestConfidenceWriteValidationBeforeEffects(t *testing.T) {
	e := &countingEmbedder{}
	s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: e, VectorBackend: newFlakyVectorStore(0), CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, invalid := range []string{"", " verified", "verified ", "VERIFIED", "unknown"} {
		_, err := s.PutWithOptionsReturningFact(context.Background(), "a", "bad", confidenceOptions(invalid))
		if err == nil {
			t.Errorf("accepted explicit %q", invalid)
		}
	}
	if e.calls.Load() != 0 {
		t.Fatal("validation invoked embedding")
	}
	if agents, err := s.ListAgents(); err != nil || len(agents) != 0 {
		t.Fatalf("validation mutated store: %v, %v", agents, err)
	}
	for _, weight := range []float64{-0.1, 0.5001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := Open(StoreConfig{DataDir: t.TempDir(), ConfidenceWeight: weight}); err == nil {
			t.Errorf("Open accepted weight %v", weight)
		}
	}
	// The established API retains its explicit-empty compatibility contract.
	if err := s.PutConfident(context.Background(), "a", "legacy", ""); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.List("a")
	if len(facts) != 1 || facts[0].Confidence != "" {
		t.Fatalf("legacy empty changed: %+v", facts)
	}
}

func TestConfidenceWriteFailureRollsBackFactMetadataAndLineage(t *testing.T) {
	for _, operation := range []string{"add", "revision"} {
		t.Run(operation, func(t *testing.T) {
			s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: &countingEmbedder{}, VectorBackend: newFlakyVectorStore(0), CandidateRetrieval: true})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			old := mustConfidenceFact(t, s, "a", "source", "unverified")
			// Simulate a failing derived-index write after the fact's own Put.
			// bbolt cannot create a bucket where a scalar value already exists.
			if err := s.db.Update(func(tx *bolt.Tx) error {
				root := tx.Bucket(bucketIdxRecency)
				if err := root.DeleteBucket([]byte("a")); err != nil {
					return err
				}
				return root.Put([]byte("a"), []byte("injected-index-failure"))
			}); err != nil {
				t.Fatal(err)
			}
			var writeErr error
			switch operation {
			case "add":
				_, writeErr = s.PutWithOptionsReturningFact(context.Background(), "a", "replacement", confidenceOptions("verified"))
			case "revision":
				_, writeErr = s.ReviseFactsWithOptions(context.Background(), "a", "replacement", confidenceOptions("verified"), old)
			}
			if writeErr == nil {
				t.Fatal("injected write failure reported success")
			}
			facts, err := s.List("a")
			if err != nil || len(facts) != 1 || facts[0].ID != old.ID || facts[0].IsSuperseded() || facts[0].Confidence != old.Confidence {
				t.Fatalf("failed write committed partial effects: %+v %v", facts, err)
			}
			if pending := s.PendingVectorCount(); pending != 0 {
				t.Fatalf("failed write committed vector intent: %d", pending)
			}
		})
	}
}

func TestConfidenceWriteVisibleAtFirstCommitAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir, CandidateRetrieval: true, VectorBackend: newFlakyVectorStore(0)})
	if err != nil {
		t.Fatal(err)
	}
	committed := make(chan string, 1)
	release := make(chan struct{})
	s.cfg.OnPut = func(_ string, id string, _ time.Duration) { committed <- id; <-release }
	done := make(chan Fact, 1)
	go func() {
		f, err := s.PutWithOptionsReturningFact(context.Background(), "a", "atomic metadata", confidenceOptions("verified"))
		if err != nil {
			t.Error(err)
		}
		done <- f
	}()
	id := <-committed
	first, ok := s.loadFact("a", id)
	if !ok || first.Confidence != "verified" {
		t.Errorf("first visible fact: %+v", first)
	}
	close(release)
	returned := <-done
	if returned.ID != first.ID || returned.Confidence != first.Confidence {
		t.Fatalf("receipt differs from commit: %+v / %+v", returned, first)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(StoreConfig{DataDir: dir, CandidateRetrieval: true, VectorBackend: newFlakyVectorStore(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stored, ok := s.loadFact("a", id)
	if !ok || stored.Confidence != "verified" {
		t.Fatalf("reopen lost label: %+v", stored)
	}
	for _, label := range []string{"", "inferred", "unverified"} {
		f := mustConfidenceFact(t, s, "a", "representation "+label, label)
		if f.Confidence != label {
			t.Fatalf("raw label %q became %q", label, f.Confidence)
		}
	}
	stored.Confidence = "historical-unknown"
	if err := s.UpdateFact("a", stored); err != nil {
		t.Fatal(err)
	}
	back, _ := s.loadFact("a", stored.ID)
	if back.Confidence != "historical-unknown" || EffectiveConfidence(back.Confidence) != "unverified" {
		t.Fatalf("unknown historical metadata changed: %+v", back)
	}
}

func TestConfidenceConcurrentDuplicateWritesKeepExactIdentity(t *testing.T) {
	e := &countingEmbedder{}
	s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: e, VectorBackend: newFlakyVectorStore(0), CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const n = 30
	start := make(chan struct{})
	results := make(chan Fact, n)
	errorsCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		label := []string{"verified", "inferred", "unverified"}[i%3]
		go func() {
			defer wg.Done()
			<-start
			f, err := s.PutWithOptionsReturningFact(context.Background(), "a", "identical text", confidenceOptions(label))
			if err != nil {
				errorsCh <- err
				return
			}
			if f.Confidence != label {
				errorsCh <- fmt.Errorf("receipt confidence %q, want %q", f.Confidence, label)
			}
			results <- f
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	seen := make(map[string]bool)
	for f := range results {
		stored, ok := s.loadFact("a", f.ID)
		if !ok || stored.Confidence != f.Confidence || seen[f.ID] {
			t.Errorf("incorrect exact write: %+v -> %+v", f, stored)
		}
		seen[f.ID] = true
	}
	if len(seen) != n || e.calls.Load() != n {
		t.Fatalf("writes=%d embeds=%d, want %d each", len(seen), e.calls.Load(), n)
	}
}

func TestConfidenceWriteUsesValidatedOptionSnapshot(t *testing.T) {
	e := &confidenceBarrierEmbedder{text: "source", entered: make(chan struct{}, 1), release: make(chan struct{})}
	s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: e, VectorBackend: newFlakyVectorStore(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	label := "verified"
	done := make(chan Fact, 1)
	go func() {
		f, err := s.PutWithOptionsReturningFact(context.Background(), "a", "source", WriteOptions{Confidence: &label})
		if err != nil {
			t.Error(err)
		}
		done <- f
	}()
	<-e.entered
	label = "invalid late mutation"
	close(e.release)
	f := <-done
	if f.Confidence != "verified" {
		t.Fatalf("validated confidence changed during embedding: %+v", f)
	}
}

func TestConfidenceRevisionUnknownHistoricalLabelIsConservative(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	old := mustConfidenceFact(t, s, "a", "source", "")
	old.Confidence = "historical-unknown"
	if err := s.UpdateFact("a", old); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.ReviseFactsWithOptions(context.Background(), "a", "replacement", WriteOptions{}, old)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Confidence != "unverified" {
		t.Fatalf("historical uncertainty promoted: %+v", replacement)
	}
	retired, _ := s.loadFact("a", old.ID)
	if retired.Confidence != "historical-unknown" || retired.SupersededBy != replacement.ID {
		t.Fatalf("original metadata lost: %+v", retired)
	}
}

func TestConfidenceRevisionConservativeSourcesAndExplicitOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		labels   []string
		explicit string
		want     string
	}{
		{"verified is not inherited", []string{"verified"}, "", "inferred"},
		{"legacy neutral", []string{""}, "", "inferred"},
		{"unverified not promoted", []string{"unverified"}, "", "unverified"},
		{"mixed duplicate minimum", []string{"verified", "inferred", "unverified"}, "", "unverified"},
		{"explicit override", []string{"unverified", "verified"}, "verified", "verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cleanup := openTestStore(t)
			defer cleanup()
			var old []Fact
			for _, label := range tc.labels {
				old = append(old, mustConfidenceFact(t, s, "a", "source", label))
			}
			if err := s.SetPinned("a", true, old[0]); err != nil {
				t.Fatal(err)
			}
			opts := WriteOptions{}
			if tc.explicit != "" {
				opts = confidenceOptions(tc.explicit)
			}
			f, err := s.ReviseWithOptions(context.Background(), "a", "source", "replacement", opts)
			if err != nil {
				t.Fatal(err)
			}
			if f.Confidence != tc.want || f.Pinned {
				t.Fatalf("replacement confidence/pin: %+v", f)
			}
			for _, v := range old {
				current, ok := s.loadFact("a", v.ID)
				if !ok || current.SupersededBy != f.ID || current.Confidence != v.Confidence {
					t.Errorf("wrong lineage/label: %+v", current)
				}
			}
		})
	}
}

type confidenceBarrierEmbedder struct {
	text    string
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (e *confidenceBarrierEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.calls.Add(1)
	if text == e.text {
		e.entered <- struct{}{}
		<-e.release
	}
	return []float32{1, 0}, nil
}
func (*confidenceBarrierEmbedder) Name() string    { return "confidence-barrier" }
func (*confidenceBarrierEmbedder) Dimensions() int { return 2 }

func TestConfidenceRevisionRejectsChangedSnapshotBeforeCommit(t *testing.T) {
	for _, mutation := range []string{"confidence", "retired", "deleted"} {
		t.Run(mutation, func(t *testing.T) {
			e := &confidenceBarrierEmbedder{text: "replacement", entered: make(chan struct{}, 1), release: make(chan struct{})}
			s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: e, VectorBackend: newFlakyVectorStore(0), CandidateRetrieval: true})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			old := mustConfidenceFact(t, s, "a", "source", "verified")
			done := make(chan error, 1)
			go func() {
				_, err := s.ReviseFactsWithOptions(context.Background(), "a", "replacement", WriteOptions{}, old)
				done <- err
			}()
			<-e.entered
			switch mutation {
			case "confidence":
				old.Confidence = "unverified"
				err = s.UpdateFact("a", old)
			case "retired":
				err = s.Retire("a", old)
			case "deleted":
				err = s.Delete("a", old.ID)
			}
			close(e.release)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrFactChanged) {
				t.Fatalf("snapshot change returned %v", err)
			}
			facts, _ := s.List("a")
			for _, f := range facts {
				if f.Text == "replacement" {
					t.Errorf("replacement committed from stale snapshot: %+v", f)
				}
			}
		})
	}
}

func TestConfidenceRevisionReceiptSurvivesLaterSourceDeletion(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	old := mustConfidenceFact(t, s, "a", "source", "unverified")
	committed := make(chan string, 1)
	release := make(chan struct{})
	s.cfg.OnPut = func(_ string, id string, _ time.Duration) { committed <- id; <-release }
	type result struct {
		fact Fact
		err  error
	}
	done := make(chan result, 1)
	go func() {
		f, err := s.ReviseFactsWithOptions(context.Background(), "a", "replacement", WriteOptions{}, old)
		done <- result{f, err}
	}()
	id := <-committed
	source, found := s.loadFact("a", old.ID)
	deleteErr := s.Delete("a", old.ID)
	close(release)
	r := <-done
	if !found || source.SupersededBy != id {
		t.Fatalf("observer saw source before retirement: %+v", source)
	}
	if deleteErr != nil {
		t.Fatal(deleteErr)
	}
	if r.err != nil || r.fact.ID != id || r.fact.Confidence != "unverified" {
		t.Fatalf("revision lost its committed receipt: %+v", r)
	}
	if f, ok := s.loadFact("a", id); !ok || f.Confidence != "unverified" {
		t.Fatalf("committed replacement missing: %+v", f)
	}
}

func TestConfidenceLifecyclePatchesReadFreshTransactionState(t *testing.T) {
	for _, op := range []string{"pin", "unpin", "retire", "decay", "touch"} {
		t.Run(op, func(t *testing.T) {
			s, cleanup := openTestStore(t)
			defer cleanup()
			old := mustConfidenceFact(t, s, "a", "source", "verified")
			tx, err := s.db.Begin(true)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				switch op {
				case "pin":
					done <- s.SetPinned("a", true, old)
				case "unpin":
					done <- s.SetPinned("a", false, old)
				case "retire":
					done <- s.Retire("a", old)
				case "decay":
					_, err := s.decayFact("a", old.ID, old.AccessedAt.Add(24*time.Hour), math.Log(2)/24)
					done <- err
				case "touch":
					old.AccessedAt = old.AccessedAt.Add(time.Hour)
					s.touchFacts([]Fact{old})
					done <- nil
				}
			}()
			<-started
			current, err := factInTx(tx, "a", old.ID)
			if err != nil {
				t.Fatal(err)
			}
			current.Confidence = "unverified"
			if err := s.persistFactTx(tx, tx.Bucket(bucketFacts).Bucket([]byte("a")), "a", current); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			got, ok := s.loadFact("a", old.ID)
			if !ok || got.Confidence != "unverified" {
				t.Fatalf("%s replayed stale confidence: %+v", op, got)
			}
			if op == "decay" && math.Abs(got.Weight-.5) > 1e-10 {
				t.Fatalf("decay weight=%v", got.Weight)
			}
		})
	}
}

func TestConfidenceLifecycleDoesNotResurrectRetiredFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	old := mustConfidenceFact(t, s, "a", "source", "verified")
	if err := s.Retire("a", old); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned("a", true, old); !errors.Is(err, ErrFactChanged) {
		t.Fatalf("pin retired: %v", err)
	}
	if _, err := s.decayFact("a", old.ID, old.AccessedAt.Add(time.Hour), math.Log(2)/24); err != nil {
		t.Fatal(err)
	}
	s.touchFacts([]Fact{old})
	got, _ := s.loadFact("a", old.ID)
	if !got.IsSuperseded() || got.Confidence != "verified" {
		t.Fatalf("lifecycle resurrected/relabeled: %+v", got)
	}
	// Clearing a historical pin remains permitted after retirement.
	if err := s.SetPinned("a", false, got); err != nil {
		t.Fatal(err)
	}
	unpinned, _ := s.loadFact("a", old.ID)
	if unpinned.Pinned || !unpinned.IsSuperseded() || unpinned.Confidence != "verified" {
		t.Fatalf("unpin changed retired confidence/lineage: %+v", unpinned)
	}
}

func TestConfidenceReindexPatchPreservesConcurrentMetadata(t *testing.T) {
	e := &confidenceBarrierEmbedder{text: "source", entered: make(chan struct{}, 1), release: make(chan struct{})}
	s, err := Open(StoreConfig{DataDir: t.TempDir(), VectorBackend: newFlakyVectorStore(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := mustConfidenceFact(t, s, "a", "source", "verified")
	s.embedder = e
	done := make(chan error, 1)
	go func() { done <- s.reindexFact(context.Background(), "a", &old, 2) }()
	<-e.entered
	current, _ := s.loadFact("a", old.ID)
	current.Confidence = "unverified"
	current.Pinned = true
	if err := s.UpdateFact("a", current); err != nil {
		t.Fatal(err)
	}
	close(e.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := s.loadFact("a", old.ID)
	if got.Confidence != "unverified" || !got.Pinned || len(got.Embedding) != 2 {
		t.Fatalf("reindex replayed stale metadata: %+v", got)
	}
}

func TestConfidenceExtractionProvenanceIndependentOfTextEquality(t *testing.T) {
	for _, tc := range []struct{ name, key, reply, want string }{
		{"offline", "", "", ExtractionFallback},
		{"generated identical text", "test-key", `["source"]`, ExtractionLLM},
		{"malformed falls back", "test-key", `not json`, ExtractionFallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.key != "" {
				anthropicServer(t, 200, anthropicTextMessage(tc.reply))
			}
			result, err := ExtractFactsWithProvenance(context.Background(), "source", &testConsolidateCfg{apiKey: tc.key, model: "claude-test"})
			if err != nil || result.Source != tc.want || len(result.Facts) != 1 || result.Facts[0] != "source" {
				t.Fatalf("extraction: %+v %v", result, err)
			}
		})
	}
}
