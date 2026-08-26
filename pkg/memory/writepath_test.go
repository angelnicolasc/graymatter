package memory

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// The recall write-back used to spawn one goroutine holding one bbolt write
// transaction PER RETURNED FACT. These tests pin the batched contract: every
// returned fact is touched exactly once, unreturned facts are untouched, and
// a fact deleted mid-flight cannot be resurrected by its own access bump.

func openTouchStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRecallTouchesEveryReturnedFactOnce(t *testing.T) {
	s := openTouchStore(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		if err := s.Put(ctx, "a", strings.Repeat("rollback procedure note ", 1)+string(rune('a'+i))+" unique suffix"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Recall(ctx, "a", "rollback", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected recalls")
	}

	facts, _ := s.List("a")
	touchedCount := 0
	for _, f := range facts {
		isReturned := false
		for _, g := range got {
			if g == f.Text {
				isReturned = true
				break
			}
		}
		switch {
		case isReturned && f.AccessCount != 1:
			t.Errorf("returned fact touched %d times, want exactly 1", f.AccessCount)
		case isReturned:
			touchedCount++
		case !isReturned && f.AccessCount != 0:
			t.Errorf("unreturned fact was touched: %+v", f)
		}
	}
	if touchedCount != len(got) {
		t.Errorf("touched %d facts, recalled %d", touchedCount, len(got))
	}
}

func TestTouchFactsCannotResurrectDeletedFact(t *testing.T) {
	s := openTouchStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, "a", "soon forgotten"); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.List("a")
	victim := facts[0]
	victim.AccessCount++
	victim.AccessedAt = time.Now().UTC()

	// Delete between recall and the touch flush - the forget race.
	if err := s.Delete("a", victim.ID); err != nil {
		t.Fatal(err)
	}
	s.touchFacts([]Fact{victim})

	if remaining, _ := s.List("a"); len(remaining) != 0 {
		t.Fatalf("deleted fact resurrected by its own access bump: %d remain", len(remaining))
	}
}

// --- PutConfident -----------------------------------------------------------

// countingEmbedder counts Embed calls so the confident-write path can assert
// it embeds exactly once (the old List-scan version had no such guarantee to
// break, which was part of the problem).
type countingEmbedder struct{ calls atomic.Int64 }

func (c *countingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	n := c.calls.Add(1)
	return []float32{float32(n)}, nil
}
func (*countingEmbedder) Dimensions() int { return 2 }
func (*countingEmbedder) Name() string    { return "counting-test" }

func TestPutConfident_StampsWithoutFullScan(t *testing.T) {
	s, cleanup := openConfidentStore(t)
	defer cleanup()
	ctx := context.Background()

	// Seed background volume: the old implementation re-listed all of this on
	// every confident write.
	for i := 0; i < 200; i++ {
		if err := s.Put(ctx, "bulk", strings.Repeat("bulk fact padding ", 3)+time.Duration(i).String()); err != nil {
			t.Fatal(err)
		}
	}

	const target = "the verified decision of record"
	if err := s.Put(ctx, "confident", target); err != nil {
		t.Fatal(err)
	}
	before := s.embedCallsSnapshot()
	if before != 201 {
		t.Fatalf("precondition: expected 201 embeds, got %d", before)
	}

	if err := s.PutConfident(ctx, "confident", target, "verified"); err != nil {
		t.Fatalf("PutConfident: %v", err)
	}

	after := s.embedCallsSnapshot()
	if after != before+1 {
		t.Fatalf("PutConfident triggered %d extra embeds, want exactly 1", after-before)
	}

	facts, _ := s.List("confident")
	stamped := 0
	for _, f := range facts {
		if f.Text == target && f.Confidence == "verified" {
			stamped++
		}
	}
	if stamped != 1 {
		t.Fatalf("stamped %d facts with confidence, want exactly 1", stamped)
	}
}

func openConfidentStore(t *testing.T) (*Store, func()) {
	t.Helper()
	ce := &countingEmbedder{}
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      ce,
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s, func() { _ = s.Close() }
}

// embedCallsSnapshot reads the counter the store was opened with, or -1 when
// the store carries none (keeps the test honest about its own fixture).
func (s *Store) embedCallsSnapshot() int64 {
	if c, ok := s.embedder.(*countingEmbedder); ok {
		return c.calls.Load()
	}
	return -1
}
