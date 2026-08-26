package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// failingEmbedder fails every call: the regression surface for degradation
// recording. Before this existed, a store configured with a broken backend
// was indistinguishable from an empty one.
type failingEmbedder struct{ err error }

func (f failingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, f.err
}
func (failingEmbedder) Dimensions() int { return 1024 }
func (failingEmbedder) Name() string    { return "failing-test" }

// staticEmbedder always succeeds with a fixed vector.
type staticEmbedder struct{}

func (staticEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}
func (staticEmbedder) Dimensions() int { return 3 }
func (staticEmbedder) Name() string    { return "static-test" }

func openEmbedHealthStore(t *testing.T, p embedding.Provider) *Store {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      p,
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestEmbedHealthRecordsDegradedWrites(t *testing.T) {
	s := openEmbedHealthStore(t, failingEmbedder{err: errors.New("boom: connection refused")})

	for i := 0; i < 3; i++ {
		if err := s.Put(context.Background(), "agent-a", "fact text"); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	h, err := s.EmbeddingHealth()
	if err != nil {
		t.Fatalf("EmbeddingHealth: %v", err)
	}
	if h.DegradedFacts != 3 {
		t.Errorf("DegradedFacts = %d, want 3", h.DegradedFacts)
	}
	if !strings.Contains(h.LastDegradError, "connection refused") {
		t.Errorf("LastDegradError = %q, want it to carry the cause", h.LastDegradError)
	}
	// Dims are the provider's declaration, recorded eagerly at Open — not
	// proof anything indexed. The failing stub declares 1024; the observed
	// truth lives in CountEmbeddings below.
	if h.EmbedDims != 1024 {
		t.Errorf("EmbedDims = %d, want 1024 (the failing provider's declared dims)", h.EmbedDims)
	}

	withVec, total, err := s.CountEmbeddings()
	if err != nil {
		t.Fatalf("CountEmbeddings: %v", err)
	}
	if withVec != 0 || total != 3 {
		t.Errorf("CountEmbeddings = (%d,%d), want (0,3)", withVec, total)
	}
}

func TestEmbedHealthKeywordProviderIsNotDegradation(t *testing.T) {
	// Keyword providers return (nil, nil): supported configuration per
	// ADR-005, never a failure. This pins the distinction the counter lives on.
	s := openEmbedHealthStore(t, embedding.NewKeyword())

	if err := s.Put(context.Background(), "agent-a", "keyword fact"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	h, err := s.EmbeddingHealth()
	if err != nil {
		t.Fatalf("EmbeddingHealth: %v", err)
	}
	if h.DegradedFacts != 0 {
		t.Errorf("DegradedFacts = %d, want 0: keyword-only is not degradation", h.DegradedFacts)
	}
}

func TestEmbedHealthyChannelReportsZeroDegraded(t *testing.T) {
	s := openEmbedHealthStore(t, staticEmbedder{})

	if err := s.Put(context.Background(), "agent-a", "vectorised"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	h, err := s.EmbeddingHealth()
	if err != nil {
		t.Fatalf("EmbeddingHealth: %v", err)
	}
	if h.DegradedFacts != 0 || h.EmbedDims != 3 || h.PendingVectors != 0 {
		t.Errorf("health = %+v, want clean healthy state (dims 3, nothing degraded or pending)", h)
	}

	withVec, total, err := s.CountEmbeddings()
	if err != nil {
		t.Fatalf("CountEmbeddings: %v", err)
	}
	if withVec != 1 || total != 1 {
		t.Errorf("CountEmbeddings = (%d,%d), want (1,1)", withVec, total)
	}
}

func TestEmbedHealthTombstonesExcludedFromCoverage(t *testing.T) {
	s := openEmbedHealthStore(t, failingEmbedder{err: errors.New("down")})

	if err := s.Put(context.Background(), "agent-a", "retired fact"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	facts, _ := s.List("agent-a")
	if len(facts) == 0 {
		t.Fatal("expected a stored fact")
	}
	// Retire via tombstone, exactly as memory_reflect forget does.
	facts[0].SupersededBy = SupersededByAgent
	if err := s.UpdateFact("agent-a", facts[0]); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	_, total, err := s.CountEmbeddings()
	if err != nil {
		t.Fatalf("CountEmbeddings: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d after retiring the only fact, want 0 (tombstones are not live)", total)
	}
}
