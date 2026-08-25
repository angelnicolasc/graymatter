package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// writeFileForTest turns path into a regular file, so anything expecting to
// create a directory there fails.
func writeFileForTest(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("occupied"), 0o600)
}

// Coverage-gap tests for the store's failure surfaces. Every test here pins a
// branch that production can reach — lock contention, embedder failures,
// vector-store outages, crash leftovers — where the wrong behaviour is silent
// data loss or a stuck pending queue rather than a loud error.

// failingVectorStore is a VectorStore whose AddDocument always fails: the
// stand-in for an Ollama outage mid-write.
type failingVectorStore struct{ calls int }

func (f *failingVectorStore) AddDocument(context.Context, string, string, string, []float32, map[string]string) error {
	f.calls++
	return errors.New("vector backend down")
}
func (f *failingVectorStore) Query(context.Context, string, []float32, int) ([]VectorResult, error) {
	return nil, nil
}
func (f *failingVectorStore) EnsureCollection(string) error { return nil }
func (f *failingVectorStore) Close() error                  { return nil }

// countingVectorStore succeeds and counts, for the happy-reconcile path.
type countingVectorStore struct{ adds int }

func (c *countingVectorStore) AddDocument(context.Context, string, string, string, []float32, map[string]string) error {
	c.adds++
	return nil
}
func (c *countingVectorStore) Query(context.Context, string, []float32, int) ([]VectorResult, error) {
	return nil, nil
}
func (c *countingVectorStore) EnsureCollection(string) error { return nil }
func (c *countingVectorStore) Close() error                  { return nil }

// stubEmbedder returns a fixed vector, or an error when fail is set.
type stubEmbedder struct {
	vec  []float32
	fail bool
}

func (e *stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	if e.fail {
		return nil, errors.New("embedder offline")
	}
	return e.vec, nil
}
func (e *stubEmbedder) Dimensions() int { return len(e.vec) }
func (e *stubEmbedder) Name() string    { return "stub" }

// plantPendingMarker writes a pending-vector marker by hand, simulating the
// crash window Put's durability model describes: bbolt committed, vector did
// not.
func plantPendingMarker(t *testing.T, s *Store, agentID, factID string) {
	t.Helper()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		pb, err := tx.Bucket(bucketPendingVector).CreateBucketIfNotExists([]byte(agentID))
		if err != nil {
			return err
		}
		return pb.Put([]byte(factID), []byte{1})
	}); err != nil {
		t.Fatalf("plant marker: %v", err)
	}
}

func TestOpen_StrictWriteRefusesLockedStore(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(StoreConfig{DataDir: dir, StrictWrite: true})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = first.Close() }()

	_, err = Open(StoreConfig{DataDir: dir, StrictWrite: true})
	if err == nil {
		t.Fatal("second strict-write open against a held lock must fail")
	}
	if !strings.Contains(err.Error(), "strict-write") {
		t.Errorf("error should name the strict-write refusal: %v", err)
	}
}

func TestOpen_LockedStoreFallsBackReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file locks are exclusive: a held write lock blocks the " +
			"read-only fallback too. The graceful-degradation path is POSIX lock semantics.")
	}
	dir := t.TempDir()
	first, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("graceful fallback open: %v", err)
	}
	defer func() { _ = second.Close() }()
	if !second.IsReadOnly() {
		t.Error("fallback open should be read-only")
	}
	if err := second.Put(context.Background(), "a", "x"); !errors.Is(err, ErrStoreReadOnly) {
		t.Errorf("Put on read-only fallback = %v, want ErrStoreReadOnly", err)
	}
}

func TestOpen_ReadOnlyEmptyDatabaseBehavesConsistently(t *testing.T) {
	// A read-only open against a directory whose gray.db does not exist yet,
	// or exists empty (no writer ever ran): platforms differ on whether bbolt
	// refuses the file or opens it, but neither outcome may panic, and an
	// opened store must report honest emptiness instead of crashing on the
	// missing buckets bucket-creating writers would have made.
	s, err := Open(StoreConfig{DataDir: t.TempDir(), ReadOnly: true})
	if err != nil {
		return // platform refused a read-only open of a missing file: fine
	}
	defer func() { _ = s.Close() }()

	if !s.IsReadOnly() {
		t.Error("forced read-only open reported writable")
	}
	agents, err := s.ListAgents()
	if err != nil || len(agents) != 0 {
		t.Errorf("ListAgents = %v, %v; want empty, nil", agents, err)
	}
	facts, err := s.List("anyone")
	if err != nil || len(facts) != 0 {
		t.Errorf("List = %v, %v; want empty, nil", facts, err)
	}
	st, err := s.Stats("anyone")
	if err != nil || st.FactCount != 0 {
		t.Errorf("Stats = %+v, %v; want zero facts", st, err)
	}
	if err := s.Put(context.Background(), "anyone", "x"); !errors.Is(err, ErrStoreReadOnly) {
		t.Errorf("Put = %v, want ErrStoreReadOnly", err)
	}
}

func TestOpen_VectorBackendFailureSurfaces(t *testing.T) {
	t.Skip("unreachable by configuration alone: bbolt opens the same DataDir " +
		"before the vector store, so any path that breaks chromem's directory " +
		"breaks bolt first. The defensive error return stays for library " +
		"callers injecting exotic backends.")
}

func TestOpen_BoltFailureSurfaces(t *testing.T) {
	// gray.db as a directory: bolt.Open fails immediately with a
	// non-timeout error, which must surface rather than degrade silently.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "gray.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(StoreConfig{DataDir: dir}); err == nil {
		t.Fatal("opening a database whose file is a directory must fail")
	}
}

func TestPutConfident_ValidatesConfidence(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	err := s.PutConfident(context.Background(), "a", "fact", "definitely")
	if err == nil || !strings.Contains(err.Error(), "verified|inferred|unverified") {
		t.Errorf("err = %v, want confidence vocabulary error", err)
	}
}

func TestPutConfident_AttachesMarkerToFreshFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()
	if err := s.Put(ctx, "conf", "a verified fact"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutConfident(ctx, "conf", "second opinion", "verified"); err != nil {
		t.Fatalf("PutConfident: %v", err)
	}
	facts, _ := s.List("conf")
	for _, f := range facts {
		want := ""
		if f.Text == "second opinion" {
			want = "verified"
		}
		if f.Confidence != want {
			t.Errorf("fact %q Confidence = %q, want %q", f.Text, f.Confidence, want)
		}
	}
}

func TestPut_EmbedderFailureKeepsFactKeywordOnly(t *testing.T) {
	dir := t.TempDir()
	var putID string
	s, err := Open(StoreConfig{
		DataDir:  dir,
		Embedder: &stubEmbedder{fail: true},
		OnPut:    func(_, id string, _ time.Duration) { putID = id },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Put(context.Background(), "kw", "survives without vectors"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if putID == "" {
		t.Error("OnPut hook never fired")
	}
	if n := s.PendingVectorCount(); n != 0 {
		t.Errorf("pending = %d, want 0: a failed embedding leaves nothing to reconcile", n)
	}
}

func TestPut_VectorFailureRecordsPendingAndReportsHook(t *testing.T) {
	dir := t.TempDir()
	hooks := 0
	vs := &failingVectorStore{}
	s, err := Open(StoreConfig{
		DataDir:            dir,
		Embedder:           &stubEmbedder{vec: []float32{1, 2}},
		VectorBackend:      vs,
		OnVectorIndexError: func(agent, fact string, err error) { hooks++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Put(context.Background(), "vec-fail", "durable in bolt, missing in vectors"); err != nil {
		t.Fatalf("Put must succeed even when the vector write fails: %v", err)
	}
	if hooks == 0 {
		t.Error("OnVectorIndexError never fired")
	}
	if n := s.PendingVectorCount(); n != 1 {
		t.Errorf("pending = %d, want 1 (the reconciler's job now)", n)
	}
	if vs.calls == 0 {
		t.Error("vector backend was never attempted")
	}
}

func TestReconcileVectors_DropsMarkerForDeletedFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	plantPendingMarker(t, s, "gone", "no-such-fact")

	s.reconcileVectors()

	if n := s.PendingVectorCount(); n != 0 {
		t.Errorf("marker for a deleted fact survived reconcile: pending=%d", n)
	}
}

func TestReconcileVectors_SkipsFactWithoutEmbedding(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()
	if err := s.Put(ctx, "kw-only", "keyword fact without embedding"); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.List("kw-only")
	plantPendingMarker(t, s, "kw-only", facts[0].ID)

	s.reconcileVectors()

	if n := s.PendingVectorCount(); n != 0 {
		t.Errorf("embedding-less marker survived reconcile: pending=%d", n)
	}
}

func TestReconcileVectors_RetriesFailedUpsertAndClearsOnSuccess(t *testing.T) {
	ctx := context.Background()

	// Failing backend: hook fires, marker stays for the next attempt.
	vs := &failingVectorStore{}
	hooked := 0
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		Embedder:           &stubEmbedder{vec: []float32{1, 2}},
		VectorBackend:      vs,
		OnVectorIndexError: func(string, string, error) { hooked++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "re", "has embedding"); err != nil {
		t.Fatal(err)
	}
	if n := s.PendingVectorCount(); n != 1 {
		t.Fatalf("precondition: Put left pending=%d, want 1", n)
	}
	s.reconcileVectors()
	if hooked == 0 {
		t.Error("failing upsert reported nothing during reconcile")
	}
	if n := s.PendingVectorCount(); n != 1 {
		t.Errorf("pending after failed reconcile = %d, want 1", n)
	}
	_ = s.Close()

	// Healthy backend: same scenario drains the queue.
	ok := &countingVectorStore{}
	s2, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      &stubEmbedder{vec: []float32{1, 2}},
		VectorBackend: ok,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.Put(ctx, "re", "has embedding too"); err != nil {
		t.Fatal(err)
	}
	s2.reconcileVectors()
	if ok.adds == 0 {
		t.Error("healthy backend was never called")
	}
	if n := s2.PendingVectorCount(); n != 0 {
		t.Errorf("pending after successful reconcile = %d, want 0", n)
	}
}

func TestVectorReconcileLoop_RetriesOnCadenceUntilShutdown(t *testing.T) {
	vs := &failingVectorStore{}
	hooked := make(chan struct{}, 8)
	s, err := Open(StoreConfig{
		DataDir:                 t.TempDir(),
		Embedder:                &stubEmbedder{vec: []float32{1, 2}},
		VectorBackend:           vs,
		VectorReconcileInterval: 30 * time.Millisecond,
		OnVectorIndexError:      func(string, string, error) { hooked <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Put leaves the pending marker behind (the backend refuses), so the
	// cadence tick — not the inline attempt — must be what reports next.
	if err := s.Put(ctx, "loop", "planted after open, picked up by the tick"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-hooked:
	case <-time.After(3 * time.Second):
		t.Fatal("background reconcile loop never retried the pending marker")
	}
	// Shutdown path: Close must stop the loop without hanging (wg.Wait).
	if err := s.Close(); err != nil {
		t.Fatalf("Close with live loop: %v", err)
	}
}

func mustListFirst(t *testing.T, s *Store, agent string) Fact {
	t.Helper()
	facts, err := s.List(agent)
	if err != nil || len(facts) == 0 {
		t.Fatalf("List %s: %v (%d facts)", agent, err, len(facts))
	}
	return facts[0]
}

func TestDelete_MissingAgentBucketIsNoop(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	if err := s.Delete("ghost-agent", "ghost-id"); err != nil {
		t.Errorf("Delete against an unknown agent returned %v, want nil", err)
	}
}

func TestList_SkipsCorruptEntries(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()
	if err := s.Put(ctx, "corrupt", "valid fact survives"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts).Bucket([]byte("corrupt"))
		return b.Put([]byte("BADCORRUPT"), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}
	facts, err := s.List("corrupt")
	if err != nil {
		t.Fatalf("List with a corrupt entry: %v", err)
	}
	if len(facts) != 1 || facts[0].Text != "valid fact survives" {
		t.Errorf("corrupt entry was not skipped cleanly: %+v", facts)
	}
}

func TestUpdateFact_NeverResurrectsDeletedFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()
	if err := s.Put(ctx, "resurrect", "deleted while a recall writeback flew"); err != nil {
		t.Fatal(err)
	}
	f := mustListFirst(t, s, "resurrect")
	if err := s.Delete("resurrect", f.ID); err != nil {
		t.Fatal(err)
	}
	f.Weight = 0.99 // the stale writeback arriving late
	if err := s.UpdateFact("resurrect", f); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}
	got, _ := s.List("resurrect")
	if len(got) != 0 {
		t.Error("late writeback resurrected a deleted fact")
	}
}

// A hand-built Store over a bucket-less bbolt exercises the defensive
// nil-bucket branches that Open-created stores can never reach: the buckets
// always exist there.
func TestBucketlessStore_DefensiveBranches(t *testing.T) {
	raw := openTestBolt(t, t.TempDir())
	s := &Store{db: raw, now: time.Now}

	if got := s.PendingVectorCount(); got != 0 {
		t.Errorf("PendingVectorCount = %d, want 0", got)
	}
	if m := s.snapshotPendingVectors(); len(m) != 0 {
		t.Errorf("snapshotPendingVectors = %v, want empty", m)
	}
	s.clearPendingVector("a", "b") // must not panic
	if err := s.markExtracted("a", "b", "sig"); err == nil {
		t.Error("markExtracted without the bucket should error")
	}
	if _, ok := s.extractedSignature("a", "b"); ok {
		t.Error("extractedSignature without the bucket should report not-found")
	}
	cycles, facts := ReadConsolidationCounters(raw)
	if cycles != 0 || facts != 0 {
		t.Errorf("counters without meta bucket = (%d,%d), want zeros", cycles, facts)
	}
	if err := recordConsolidation(raw, 3); err == nil {
		t.Error("recordConsolidation without meta bucket should error")
	}
}

func TestRecordConsolidation_CorruptCounterErrors(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put([]byte(metaKeyFactsConsolidated), []byte("not-a-number"))
	}); err != nil {
		t.Fatal(err)
	}
	err := recordConsolidation(s.db, 5)
	if err == nil || !strings.Contains(err.Error(), "meta key") {
		t.Errorf("err = %v, want corrupt-counter error naming the key", err)
	}
	// The healthy counter must not have been bumped by the aborted tx.
	if v := s.db.View(func(tx *bolt.Tx) error {
		got := tx.Bucket(bucketMeta).Get([]byte(metaKeyConsolidations))
		if got != nil {
			t.Errorf("consolidations advanced despite abort: %s", got)
		}
		return nil
	}); v != nil {
		t.Fatal(v)
	}
}
