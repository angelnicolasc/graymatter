package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const vectorRevisionAgent = "revision-vector"

type revisionVectorProvider struct{}

func (revisionVectorProvider) Embed(_ context.Context, text string) ([]float32, error) {
	if strings.HasPrefix(text, "replacement") {
		return []float32{0.6, 0.8}, nil
	}
	return []float32{1, 0}, nil
}
func (revisionVectorProvider) Dimensions() int { return 2 }
func (revisionVectorProvider) Name() string    { return "revision-vector-fixture" }

type revisionVectorBackend struct {
	*chromemVectorStore
	beforeAdd   func(context.Context, string) error
	beforeQuery func()
	queryCalls  atomic.Int32
}

func (b *revisionVectorBackend) AddDocument(ctx context.Context, agent, id, content string, emb []float32, metadata map[string]string) error {
	if b.beforeAdd != nil {
		if err := b.beforeAdd(ctx, content); err != nil {
			return err
		}
	}
	return b.chromemVectorStore.AddDocument(ctx, agent, id, content, emb, metadata)
}

func (b *revisionVectorBackend) Query(ctx context.Context, agent string, emb []float32, n int) ([]VectorResult, error) {
	b.queryCalls.Add(1)
	if b.beforeQuery != nil {
		b.beforeQuery()
	}
	return b.chromemVectorStore.Query(ctx, agent, emb, n)
}

func (b *revisionVectorBackend) QueryEligible(ctx context.Context, agent string, emb []float32, n int, ids map[string]bool) ([]VectorResult, bool, error) {
	b.queryCalls.Add(1)
	return b.chromemVectorStore.QueryEligible(ctx, agent, emb, n, ids)
}

type staleRevisionVectorBackend struct {
	VectorStore
	retiredID string
}

func (b staleRevisionVectorBackend) QueryEligible(context.Context, string, []float32, int, map[string]bool) ([]VectorResult, bool, error) {
	return []VectorResult{{ID: b.retiredID, Similarity: 1}}, true, nil
}

func openRevisionVectorStore(t *testing.T, dir string, indexed bool, beforeAdd func(context.Context, string) error, beforeQuery func()) (*Store, *revisionVectorBackend) {
	t.Helper()
	vectors, err := newChromemVectorStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := &revisionVectorBackend{chromemVectorStore: vectors, beforeAdd: beforeAdd, beforeQuery: beforeQuery}
	s, err := Open(StoreConfig{
		DataDir: dir, Embedder: revisionVectorProvider{}, VectorBackend: backend,
		CandidateRetrieval: indexed, StrictWrite: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, backend
}

func seedRevisionVectorSources(t *testing.T, s *Store) []Fact {
	t.Helper()
	var sources []Fact
	for i := 0; i < 3; i++ {
		f, err := s.PutWithOptionsReturningFact(context.Background(), vectorRevisionAgent, fmt.Sprintf("obsolete signal %d", i), WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, f)
	}
	return sources
}

func requireRevisionVectorRanking(t *testing.T, s *Store, replacementID string, vectorRank int, fallback string) {
	t.Helper()
	result, err := s.RecallExplainWithOptions(context.Background(), vectorRevisionAgent, "signal", 1, RecallOptions{})
	if err != nil || len(result.Receipts) != 1 {
		t.Fatalf("recall: %+v, %v", result, err)
	}
	got := result.Receipts[0]
	if got.Provenance.FactID != replacementID || got.Ranks.VectorRank != vectorRank {
		t.Fatalf("recall selected %+v, want replacement %q at vector rank %d", got, replacementID, vectorRank)
	}
	if fallback == "" {
		if result.Retrieval != nil {
			t.Fatalf("healthy legacy receipt unexpectedly added policy metadata: %+v", result.Retrieval)
		}
	} else if result.Retrieval == nil || result.Retrieval.VectorFallback != fallback {
		t.Fatalf("retrieval receipt = %+v, want fallback %q", result.Retrieval, fallback)
	}
}

func TestRevisionVectorDeliveryFailureAndRecovery(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			dir := t.TempDir()
			var fail atomic.Bool
			fail.Store(true)
			beforeAdd := func(_ context.Context, text string) error {
				if fail.Load() && strings.HasPrefix(text, "replacement") {
					return errors.New("external vector upsert unavailable")
				}
				return nil
			}
			s, backend := openRevisionVectorStore(t, dir, indexed, beforeAdd, nil)
			sources := seedRevisionVectorSources(t, s)
			replacement, err := s.ReviseFactsWithOptions(context.Background(), vectorRevisionAgent, "replacement signal", WriteOptions{}, sources...)
			if err != nil || replacement.ID == "" {
				t.Fatalf("committed revision = %+v, %v", replacement, err)
			}
			if health, err := s.EmbeddingHealth(); err != nil || health.PendingVectors != 1 || !health.VectorSearchDegraded {
				t.Fatalf("failed delivery health = %+v, %v", health, err)
			}
			requireRevisionVectorRanking(t, s, replacement.ID, 0, vectorIndexPending)
			rows, err := s.BatchRecallWithOptions(context.Background(), vectorRevisionAgent, []string{"signal", "replacement"}, 1, RecallOptions{})
			if err != nil || len(rows) != 2 {
				t.Fatalf("batch fallback = %+v, %v", rows, err)
			}
			for _, row := range rows {
				if row.Err != nil || len(row.Facts) != 1 || row.Facts[0] != replacement.Text ||
					row.Retrieval == nil || row.Retrieval.VectorFallback != vectorIndexPending {
					t.Fatalf("batch lost pending-vector fallback: %+v", row)
				}
			}
			if backend.queryCalls.Load() != 0 {
				t.Fatal("recall queried external vectors despite a known gap")
			}
			s.reconcileVectors()
			if got := s.PendingVectorCount(); got != 1 {
				t.Fatalf("failed retry dropped pending marker: %d", got)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			fail.Store(false)
			reopened, newBackend := openRevisionVectorStore(t, dir, indexed, beforeAdd, nil)
			if health, err := reopened.EmbeddingHealth(); err != nil || health.PendingVectors != 0 || health.VectorSearchDegraded {
				t.Fatalf("recovery health = %+v, %v", health, err)
			}
			unfiltered, err := newBackend.chromemVectorStore.Query(context.Background(), vectorRevisionAgent, []float32{1, 0}, 2)
			if err != nil || len(unfiltered) != 2 {
				t.Fatalf("stale-vector pressure fixture: %+v, %v", unfiltered, err)
			}
			for _, got := range unfiltered {
				if got.ID == replacement.ID {
					t.Fatal("fixture did not crowd the replacement out of the raw vector budget")
				}
			}
			requireRevisionVectorRanking(t, reopened, replacement.ID, 1, "")
			reopened.reconcileVectors()
			docs, err := newBackend.chromemVectorStore.Query(context.Background(), vectorRevisionAgent, []float32{1, 0}, 20)
			if err != nil || len(docs) != len(sources)+1 {
				t.Fatalf("reconciliation did not preserve one vector per ID: %+v, %v", docs, err)
			}
			// Hide the optional capability on an otherwise identical backend.
			// Canonical embeddings still provide a complete cosine ranking.
			reopened.vectors = struct{ VectorStore }{newBackend}
			requireRevisionVectorRanking(t, reopened, replacement.ID, 1, "")

			// An eligible backend that falsely claims exhaustion while
			// returning a retired ID cannot omit the canonical replacement.
			reopened.vectors = staleRevisionVectorBackend{newBackend, sources[0].ID}
			requireRevisionVectorRanking(t, reopened, replacement.ID, 1, "")
		})
	}
}

func TestRevisionVectorHistoricalTombstonesAndOrphansExcludeRetiredBudget(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			s, backend := openRevisionVectorStore(t, t.TempDir(), indexed, nil, nil)
			sources := seedRevisionVectorSources(t, s)
			replacement, err := s.PutWithOptionsReturningFact(context.Background(), vectorRevisionAgent, "replacement signal", WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range sources {
				source.SupersededBy = replacement.ID
				if err := s.UpdateFact(vectorRevisionAgent, source); err != nil {
					t.Fatal(err)
				}
			}
			_, vectorState, err := s.listLiteSnapshot(context.Background(), vectorRevisionAgent)
			if err != nil || vectorState.revision != 0 || vectorState.pending {
				t.Fatalf("historical fixture unexpectedly recorded a revision epoch: %+v, %v", vectorState, err)
			}
			raw, err := backend.chromemVectorStore.Query(context.Background(), vectorRevisionAgent, []float32{1, 0}, 2)
			if err != nil || len(raw) != 2 || raw[0].ID == replacement.ID || raw[1].ID == replacement.ID {
				t.Fatalf("historical fixture did not crowd the replacement out: %+v, %v", raw, err)
			}
			requireRevisionVectorRanking(t, s, replacement.ID, 1, "")

			// Historical pruning collected the canonical tombstones
			// without removing the corresponding external documents.
			for _, source := range sources {
				if err := s.Delete(vectorRevisionAgent, source.ID); err != nil {
					t.Fatal(err)
				}
			}
			remaining, state, err := s.listLiteSnapshot(context.Background(), vectorRevisionAgent)
			if err != nil || len(remaining) != 1 || state.revision != 0 || state.pending {
				t.Fatalf("historical orphan fixture = %+v, %+v, %v", remaining, state, err)
			}
			embedder := &confidenceQueryEmbedder{}
			s.embedder = embedder
			requireRevisionVectorRanking(t, s, replacement.ID, 1, "")
			if calls := embedder.calls.Load(); calls != 1 {
				t.Fatalf("orphan fallback embedded the same query %d times", calls)
			}
		})
	}
}

func TestRevisionVectorSnapshotDetectsCompletedQueueCycle(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			reached, release := make(chan struct{}), make(chan struct{})
			var first sync.Once
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			s, _ := openRevisionVectorStore(t, t.TempDir(), indexed, nil, func() {
				first.Do(func() {
					close(reached)
					<-release
				})
			})
			sources := seedRevisionVectorSources(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			type result struct {
				value RecallExplainResult
				err   error
			}
			done := make(chan result, 1)
			go func() {
				value, err := s.RecallExplainWithOptions(ctx, vectorRevisionAgent, "signal", 10, RecallOptions{})
				done <- result{value, err}
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("recall did not reach the external vector query")
			}
			replacement, err := s.ReviseFactsWithOptions(ctx, vectorRevisionAgent, "replacement signal", WriteOptions{}, sources...)
			if err != nil || s.PendingVectorCount() != 0 {
				t.Fatalf("revision did not complete its queue cycle during recall: %+v, %v", replacement, err)
			}
			unblock()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal(got.err)
				}
				if indexed {
					if len(got.value.Receipts) != 1 || got.value.Receipts[0].Provenance.FactID != replacement.ID {
						t.Fatalf("indexed fallback mixed revision states: %+v", got.value)
					}
				} else {
					if len(got.value.Receipts) != len(sources) || got.value.Retrieval == nil || got.value.Retrieval.VectorFallback != vectorIndexChanged {
						t.Fatalf("scan accepted a vector population from another snapshot: %+v", got.value)
					}
					for _, receipt := range got.value.Receipts {
						if receipt.Ranks.VectorRank != 0 || receipt.Provenance.FactID == replacement.ID {
							t.Fatalf("scan mixed revision states: %+v", receipt)
						}
					}
				}
			case <-ctx.Done():
				t.Fatal("recall did not finish after releasing its provider")
			}
		})
	}
}

func TestRevisionVectorOutboxSurvivesCrashBeforeSend(t *testing.T) {
	const childDir = "GRAYMATTER_REVISION_VECTOR_CRASH_DIR"
	const exitBeforeVector = 74
	if dir := os.Getenv(childDir); dir != "" {
		s, _ := openRevisionVectorStore(t, dir, true, func(_ context.Context, text string) error {
			if text == "replacement signal" {
				os.Exit(exitBeforeVector)
			}
			return nil
		}, nil)
		sources, err := s.List(vectorRevisionAgent)
		if err != nil || len(sources) != 3 {
			t.Fatalf("load sources: %+v, %v", sources, err)
		}
		_, err = s.ReviseFactsWithOptions(context.Background(), vectorRevisionAgent, "replacement signal", WriteOptions{}, sources...)
		t.Fatalf("did not crash at external send boundary: %v", err)
	}
	dir := t.TempDir()
	s, _ := openRevisionVectorStore(t, dir, true, nil, nil)
	sources := seedRevisionVectorSources(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRevisionVectorOutboxSurvivesCrashBeforeSend$")
	cmd.Env = append(os.Environ(), childDir+"="+dir)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitBeforeVector {
		t.Fatalf("crash subprocess: %v, %s", err, output)
	}

	var hold atomic.Bool
	hold.Store(true)
	reopened, backend := openRevisionVectorStore(t, dir, true, func(_ context.Context, text string) error {
		if hold.Load() && text == "replacement signal" {
			return errors.New("paused recovery")
		}
		return nil
	}, nil)
	facts, err := reopened.List(vectorRevisionAgent)
	if err != nil || len(facts) != len(sources)+1 {
		t.Fatalf("reopened canonical state: %+v, %v", facts, err)
	}
	replacement := facts[0]
	if replacement.Text != "replacement signal" || replacement.IsSuperseded() || reopened.PendingVectorCount() != 1 {
		t.Fatalf("crash did not leave an intact revision and outbox: %+v", facts)
	}
	for _, f := range facts[1:] {
		if f.SupersededBy != replacement.ID {
			t.Fatalf("source remained live after crash: %+v", f)
		}
	}
	raw, err := backend.chromemVectorStore.Query(ctx, vectorRevisionAgent, []float32{1, 0}, 10)
	if err != nil || len(raw) != len(sources) {
		t.Fatalf("replacement escaped the pre-send crash: %+v, %v", raw, err)
	}
	requireRevisionVectorRanking(t, reopened, replacement.ID, 0, vectorIndexPending)
	hold.Store(false)
	reopened.reconcileVectors()
	if got := reopened.PendingVectorCount(); got != 0 {
		t.Fatalf("recovery left %d pending writes", got)
	}
	requireRevisionVectorRanking(t, reopened, replacement.ID, 1, "")
}
