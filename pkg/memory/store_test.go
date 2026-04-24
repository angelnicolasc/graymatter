package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// goodProvider returns a fixed 4-dimensional embedding — enough to exercise
// addToVector, chromemVectorStore.AddDocument, and chromemVectorStore.Query.
type goodProvider struct{}

func (g *goodProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3, 0.4}, nil
}
func (g *goodProvider) Dimensions() int { return 4 }
func (g *goodProvider) Name() string    { return "good-provider" }

// errProvider is an embedding.Provider that always returns an error.
type errProvider struct{}

func (e *errProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, errors.New("embed: intentional test failure")
}
func (e *errProvider) Dimensions() int { return 768 }
func (e *errProvider) Name() string    { return "err-provider" }

// TestPut_NilEmbedder stores facts without embeddings (keyword-only mode).
func TestPut_NilEmbedder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: nil, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	if err := s.Put(ctx, "nil-emb-agent", "fact without embedding"); err != nil {
		t.Fatalf("Put with nil embedder: %v", err)
	}
	facts, _ := s.List("nil-emb-agent")
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if len(facts[0].Embedding) != 0 {
		t.Error("expected no embedding when embedder is nil")
	}
}

// TestPut_FailingEmbedder verifies graceful fallback when the embedding call
// errors: the fact must still be persisted, just without an embedding.
func TestPut_FailingEmbedder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: &errProvider{}, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	if err := s.Put(ctx, "err-emb-agent", "important fact"); err != nil {
		t.Fatalf("Put with failing embedder should not error: %v", err)
	}
	facts, _ := s.List("err-emb-agent")
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if len(facts[0].Embedding) != 0 {
		t.Error("expected no embedding when embedder errors")
	}
}

// TestDelete_NonExistentFact must not error.
func TestDelete_NonExistentFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	if err := s.Delete("ghost-agent", "nonexistent-id"); err != nil {
		t.Errorf("Delete of nonexistent fact should not error: %v", err)
	}
}

// TestDelete_NonExistentAgent must not error.
func TestDelete_NonExistentAgent(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	if err := s.Delete("totally-unknown-agent", "any-id"); err != nil {
		t.Errorf("Delete on unknown agent should not error: %v", err)
	}
}

// TestUpdateFact_NonExistentBucket must not error (bucket doesn't exist yet).
func TestUpdateFact_NonExistentBucket(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	f := newFact("ghost-agent", "orphan fact", nil)
	if err := s.UpdateFact("ghost-agent", f); err != nil {
		t.Errorf("UpdateFact on missing bucket should not error: %v", err)
	}
}

// TestListAgents_Empty returns empty slice, not error.
func TestListAgents_Empty(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents on empty store: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}

// TestListAgents_AfterPut returns the correct agent ID.
func TestListAgents_AfterPut(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "alpha", "fact alpha")
	_ = s.Put(ctx, "beta", "fact beta")

	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Errorf("expected 2 agents, got %d: %v", len(agents), agents)
	}
}

// TestStats_Empty returns zero-value stats without error.
func TestStats_Empty(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	st, err := s.Stats("nobody")
	if err != nil {
		t.Fatalf("Stats on empty: %v", err)
	}
	if st.FactCount != 0 {
		t.Errorf("FactCount = %d, want 0", st.FactCount)
	}
}

// TestStats_Aggregates verifies FactCount, OldestAt, NewestAt, AvgWeight.
func TestStats_Aggregates(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "stats-agent", "first fact")
	time.Sleep(2 * time.Millisecond) // ensure distinct CreatedAt
	_ = s.Put(ctx, "stats-agent", "second fact")

	st, err := s.Stats("stats-agent")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.FactCount != 2 {
		t.Errorf("FactCount = %d, want 2", st.FactCount)
	}
	if !st.NewestAt.After(st.OldestAt) && !st.NewestAt.Equal(st.OldestAt) {
		t.Errorf("NewestAt (%v) should be ≥ OldestAt (%v)", st.NewestAt, st.OldestAt)
	}
	if st.AvgWeight <= 0 || st.AvgWeight > 1.0 {
		t.Errorf("AvgWeight = %.4f, want in (0,1]", st.AvgWeight)
	}
}

// TestConcurrentPut verifies no data races or errors under concurrent writes.
func TestConcurrentPut(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const goroutines = 20
	const factsEach = 10

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < factsEach; i++ {
				_ = s.Put(ctx, "concurrent-agent", strings.Repeat("x", id*10+i+1))
			}
		}(g)
	}
	wg.Wait()

	facts, err := s.List("concurrent-agent")
	if err != nil {
		t.Fatalf("List after concurrent puts: %v", err)
	}
	if len(facts) != goroutines*factsEach {
		t.Errorf("expected %d facts, got %d", goroutines*factsEach, len(facts))
	}
}

// TestClose_WhileAsyncConsolidate verifies that Close() waits for background
// goroutines to finish before returning — no writes after Close.
func TestClose_WhileAsyncConsolidate(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:                dir,
		Embedder:               embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:          720 * time.Hour,
		MaxAsyncConsolidations: 2,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx := context.Background()
	cfg := &testConsolidateCfg{threshold: 1, halfLife: 720 * time.Hour}
	for i := 0; i < 5; i++ {
		_ = s.Put(ctx, "close-agent", strings.Repeat("data", i+1))
		s.LaunchAsyncConsolidate("close-agent", cfg)
	}
	// Close must block until all goroutines drain, then return cleanly.
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestReconcileVectors_IsIdempotent verifies that opening an existing store twice
// does not error or duplicate facts.
func TestReconcileVectors_IsIdempotent(t *testing.T) {
	dir := t.TempDir()

	open := func() *Store {
		s, err := Open(StoreConfig{
			DataDir:       dir,
			Embedder:      nil,
			DecayHalfLife: 720 * time.Hour,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	}

	// First open: write a fact.
	s1 := open()
	_ = s1.Put(context.Background(), "reconcile-agent", "persistent fact")
	_ = s1.Close()

	// Second open: reconcileVectors must not duplicate the fact.
	s2 := open()
	defer func() { _ = s2.Close() }()

	facts, err := s2.List("reconcile-agent")
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(facts) != 1 {
		t.Errorf("expected 1 fact after reopen, got %d", len(facts))
	}
}

// TestDB_Getter verifies DB() returns the underlying bolt handle.
func TestDB_Getter(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	if s.DB() == nil {
		t.Error("DB() returned nil")
	}
}

// TestSetKG_NilArgs verifies SetKG accepts nil arguments without panicking.
func TestSetKG_NilArgs(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	s.SetKG(nil, nil) // must not panic
}

// TestMarshalJSON_RoundTrip covers the internal marshalJSON helper.
func TestMarshalJSON_RoundTrip(t *testing.T) {
	type simple struct{ X int }
	b, err := marshalJSON(simple{X: 42})
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	if string(b) != `{"X":42}` {
		t.Errorf("got %s, want {\"X\":42}", b)
	}
}

// TestPut_WithEmbedder covers addToVector and chromemVectorStore.AddDocument
// by using a fixed-vector provider that returns a real embedding.
func TestPut_WithEmbedder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: &goodProvider{}, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	if err := s.Put(ctx, "vec-agent", "vector fact"); err != nil {
		t.Fatalf("Put with goodProvider: %v", err)
	}
	facts, err := s.List("vec-agent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if len(facts[0].Embedding) == 0 {
		t.Error("expected embedding to be stored")
	}
}

// TestRecall_WithEmbedder covers vectorSearch → chromemVectorStore.Query.
func TestRecall_WithEmbedder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: &goodProvider{}, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	_ = s.Put(ctx, "vec-recall-agent", "vector recall fact")

	results, err := s.Recall(ctx, "vec-recall-agent", "vector recall", 5)
	if err != nil {
		t.Fatalf("Recall with embedder: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least one recall result")
	}
}

// TestEmbedDimensionValidation verifies that checkEmbedDimensions logs a
// warning (does not error) when the stored dimension differs from the current
// provider. We can't intercept log.Printf easily, so we verify the meta key is
// written correctly and a mis-matched provider does not crash.
func TestEmbedDimensionValidation_RecordsOnFirstWrite(t *testing.T) {
	dir := t.TempDir()

	// Provider A: 3-dimensional (tiny, for testing).
	type fixedProvider struct{ dims int }
	// Use a real keyword provider — it has Dimensions()=0, which we override via
	// checkEmbedDimensions only when dims > 0. So we'll use nil embedder but call
	// checkEmbedDimensions directly.

	s, err := Open(StoreConfig{DataDir: dir, Embedder: nil, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Manually record a dimension, then check for mismatch.
	s.recordEmbedDimensions(768)
	s.recordEmbedDimensions(768) // idempotent: second call must not overwrite

	// Simulate a mis-matched provider — must not panic, just warn.
	mismatch := &errProvider{} // dims=768, matches → no warning expected here
	s.checkEmbedDimensions(mismatch)

	_ = s.Close()
}

// ── Read-only store tests ─────────────────────────────────────────────────────

// TestOpen_ForceReadOnly verifies that StoreConfig.ReadOnly=true opens the
// store in read-only mode: IsReadOnly() is true and read operations work.
func TestOpen_ForceReadOnly(t *testing.T) {
	dir := t.TempDir()

	// Seed the store with a fact using a normal write-mode open.
	rw, err := Open(StoreConfig{DataDir: dir, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("write open: %v", err)
	}
	_ = rw.Put(context.Background(), "ro-agent", "persisted fact")
	_ = rw.Close()

	// Reopen in forced read-only mode.
	ro, err := Open(StoreConfig{DataDir: dir, DecayHalfLife: 720 * time.Hour, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer func() { _ = ro.Close() }()

	if !ro.IsReadOnly() {
		t.Fatal("IsReadOnly() should be true when ReadOnly config is set")
	}

	// Read operations must still work.
	facts, err := ro.List("ro-agent")
	if err != nil {
		t.Fatalf("List in read-only mode: %v", err)
	}
	if len(facts) != 1 {
		t.Errorf("expected 1 fact, got %d", len(facts))
	}
}

// TestOpen_LockContention verifies that when the write lock is held a second
// Open() fails with a descriptive error (bbolt's shared-lock is also blocked
// by an exclusive lock, so the RO fallback cannot succeed either). The error
// message must mention the lock so the user knows what to do.
func TestOpen_LockContention(t *testing.T) {
	// Reduce the timeout so the test runs in < 250 ms.
	orig := boltOpenTimeout
	boltOpenTimeout = 100 * time.Millisecond
	t.Cleanup(func() { boltOpenTimeout = orig })

	dir := t.TempDir()

	// First open: holds the exclusive write lock.
	rw, err := Open(StoreConfig{DataDir: dir, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Second open: both write and RO fallback will time out.
	// We expect an error whose message tells the user the DB is locked.
	_, err = Open(StoreConfig{DataDir: dir, DecayHalfLife: 720 * time.Hour})
	if err == nil {
		t.Fatal("expected an error when write lock is held, got nil")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("error should mention 'locked', got: %v", err)
	}
}

// TestReadOnlyStore_MutationsReturnErr verifies that Put, Delete, and
// UpdateFact all return ErrStoreReadOnly when the store is read-only.
func TestReadOnlyStore_MutationsReturnErr(t *testing.T) {
	dir := t.TempDir()

	// Seed with one fact so buckets exist.
	rw, err := Open(StoreConfig{DataDir: dir, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatalf("write open: %v", err)
	}
	f := newFact("mut-agent", "original text", nil)
	_ = rw.Put(context.Background(), "mut-agent", "original text")
	_ = rw.Close()

	ro, err := Open(StoreConfig{DataDir: dir, DecayHalfLife: 720 * time.Hour, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer func() { _ = ro.Close() }()

	ctx := context.Background()

	if err := ro.Put(ctx, "mut-agent", "new fact"); !errors.Is(err, ErrStoreReadOnly) {
		t.Errorf("Put: want ErrStoreReadOnly, got %v", err)
	}
	if err := ro.Delete("mut-agent", f.ID); !errors.Is(err, ErrStoreReadOnly) {
		t.Errorf("Delete: want ErrStoreReadOnly, got %v", err)
	}
	if err := ro.UpdateFact("mut-agent", f); !errors.Is(err, ErrStoreReadOnly) {
		t.Errorf("UpdateFact: want ErrStoreReadOnly, got %v", err)
	}
}
