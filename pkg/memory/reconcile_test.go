package memory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyVectorStore returns errors from AddDocument until calls reach okAfter,
// after which it succeeds and records every (collection, id) it ingested.
type flakyVectorStore struct {
	mu       sync.Mutex
	okAfter  int64
	calls    int64
	docs     map[string]map[string]bool // collection -> id -> exists
	failWith error
}

func newFlakyVectorStore(okAfter int) *flakyVectorStore {
	return &flakyVectorStore{
		okAfter:  int64(okAfter),
		docs:     make(map[string]map[string]bool),
		failWith: errors.New("flaky vector store: temporary failure"),
	}
}

func (f *flakyVectorStore) AddDocument(_ context.Context, collection, id, _ string, _ []float32, _ map[string]string) error {
	n := atomic.AddInt64(&f.calls, 1)
	if n <= f.okAfter {
		return f.failWith
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.docs[collection] == nil {
		f.docs[collection] = make(map[string]bool)
	}
	f.docs[collection][id] = true
	return nil
}

func (f *flakyVectorStore) Query(_ context.Context, _ string, _ []float32, _ int) ([]VectorResult, error) {
	return nil, nil
}

func (f *flakyVectorStore) EnsureCollection(_ string) error { return nil }
func (f *flakyVectorStore) Close() error                    { return nil }

func (f *flakyVectorStore) has(collection, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.docs[collection][id]
}

// TestPut_VectorFailureLeavesPendingMarker verifies that when the inline
// vector upsert fails after a successful bbolt write, a durable pending marker
// is left behind for the reconciler to drain.
func TestPut_VectorFailureLeavesPendingMarker(t *testing.T) {
	dir := t.TempDir()
	flaky := newFlakyVectorStore(1) // first AddDocument fails

	var hookCalls int32
	s, err := Open(StoreConfig{
		DataDir:                 dir,
		Embedder:                &goodProvider{},
		DecayHalfLife:           720 * time.Hour,
		VectorBackend:           flaky,
		VectorReconcileInterval: 0, // disable background loop for deterministic test
		OnVectorIndexError: func(agentID, factID string, err error) {
			atomic.AddInt32(&hookCalls, 1)
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Put(context.Background(), "agent-1", "first fact"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := s.PendingVectorCount(); got != 1 {
		t.Fatalf("PendingVectorCount after failed inline upsert: got %d, want 1", got)
	}
	if atomic.LoadInt32(&hookCalls) != 1 {
		t.Fatalf("OnVectorIndexError calls: got %d, want 1", hookCalls)
	}

	// Manually trigger reconciliation; flaky store should now succeed.
	s.reconcileVectors()

	if got := s.PendingVectorCount(); got != 0 {
		t.Fatalf("PendingVectorCount after reconcile: got %d, want 0", got)
	}
}

// TestPut_VectorSuccessClearsMarker verifies the happy path: a successful
// inline upsert leaves zero pending markers behind.
func TestPut_VectorSuccessClearsMarker(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:                 dir,
		Embedder:                &goodProvider{},
		DecayHalfLife:           720 * time.Hour,
		VectorReconcileInterval: 0,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Put(context.Background(), "agent-1", "first fact"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := s.PendingVectorCount(); got != 0 {
		t.Fatalf("PendingVectorCount on happy path: got %d, want 0", got)
	}
}

// TestReconcile_DropsMarkerForDeletedFact verifies that a pending marker for a
// fact that no longer exists in bbolt is silently dropped (no infinite retry).
func TestReconcile_DropsMarkerForDeletedFact(t *testing.T) {
	dir := t.TempDir()
	flaky := newFlakyVectorStore(1)
	s, err := Open(StoreConfig{
		DataDir:                 dir,
		Embedder:                &goodProvider{},
		DecayHalfLife:           720 * time.Hour,
		VectorBackend:           flaky,
		VectorReconcileInterval: 0,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Put(context.Background(), "agent-1", "doomed"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := s.PendingVectorCount(); got != 1 {
		t.Fatalf("PendingVectorCount: got %d, want 1", got)
	}
	facts, err := s.List("agent-1")
	if err != nil || len(facts) != 1 {
		t.Fatalf("List: got %d facts, err=%v", len(facts), err)
	}
	if err := s.Delete("agent-1", facts[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	s.reconcileVectors()

	if got := s.PendingVectorCount(); got != 0 {
		t.Fatalf("PendingVectorCount after reconcile of deleted fact: got %d, want 0", got)
	}
}
