package memory

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chromem "github.com/philippgille/chromem-go"
)

const (
	nativeRecoveryAgent       = "revision-recovery"
	nativeRecoveryOtherAgent  = "other-recovery"
	nativeRecoveryReplacement = "replacement signal"
)

type nativeRecoveryEmbedder struct{}

func (nativeRecoveryEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}
func (nativeRecoveryEmbedder) Name() string    { return "native-recovery-fixture" }
func (nativeRecoveryEmbedder) Dimensions() int { return 2 }

// Deliver the file but withhold acknowledgment. The store retains the intent
// naturally, then the fixture replaces the completed file with a torn write.
type interruptedNativeDelivery struct{ VectorStore }

func (v interruptedNativeDelivery) AddDocument(ctx context.Context, agent, id, text string, vector []float32, metadata map[string]string) error {
	if err := v.VectorStore.AddDocument(ctx, agent, id, text, vector, metadata); err != nil {
		return err
	}
	if text == nativeRecoveryReplacement {
		return errors.New("delivery acknowledgment interrupted")
	}
	return nil
}

type nativeRecoveryFixture struct {
	cfg                              StoreConfig
	source, replacement, peer, other Fact
	tornPath                         string
}

func makeNativeRecoveryFixture(t *testing.T) nativeRecoveryFixture {
	t.Helper()
	f := seedNativeRecoveryFixture(t, true)
	if err := os.Truncate(f.tornPath, 0); err != nil {
		t.Fatal(err)
	}
	return f
}

func seedNativeRecoveryFixture(t *testing.T, pending bool) nativeRecoveryFixture {
	t.Helper()
	cfg := StoreConfig{
		DataDir: t.TempDir(), Embedder: nativeRecoveryEmbedder{},
		CandidateRetrieval: true, StrictWrite: true,
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	source := mustConfidenceFact(t, s, nativeRecoveryAgent, "old signal", "verified")
	other := mustConfidenceFact(t, s, nativeRecoveryOtherAgent, "independent signal", "verified")
	wantPending := 0
	var peer Fact
	if pending {
		s.vectors = interruptedNativeDelivery{s.vectors}
		wantPending = 1
	} else {
		peer = mustConfidenceFact(t, s, nativeRecoveryAgent, "retained peer signal", "verified")
	}
	replacement, err := s.ReviseFactsWithOptions(ctx, nativeRecoveryAgent, nativeRecoveryReplacement, WriteOptions{}, source)
	if count := s.PendingVectorCount(); err != nil || replacement.ID == "" || count != wantPending {
		t.Fatalf("revision %+v, pending = %d (want %d): %v", replacement, count, wantPending, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	return nativeRecoveryFixture{
		cfg: cfg, source: source, replacement: replacement, peer: peer, other: other,
		tornPath: nativeRecoveryDocumentPath(t, cfg.DataDir, replacement.ID),
	}
}

func nativeRecoveryDocumentPath(t *testing.T, dataDir, id string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dataDir, "vectors", "*", "*.gob"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document chromem.Document
		if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&document); err == nil && document.ID == id {
			return path
		}
	}
	t.Fatalf("native document %q missing", id)
	return ""
}

func requireNativeRecoveryCanonical(t *testing.T, s *Store, f nativeRecoveryFixture) {
	t.Helper()
	source, ok := s.loadFact(nativeRecoveryAgent, f.source.ID)
	if !ok || source.SupersededBy != f.replacement.ID || source.Text != f.source.Text {
		t.Fatalf("source tombstone was lost: %+v", source)
	}
	for _, want := range []Fact{f.replacement, f.peer, f.other} {
		if want.ID == "" {
			continue
		}
		got, ok := s.loadFact(want.AgentID, want.ID)
		if !ok || got.IsSuperseded() || got.Text != want.Text || got.Confidence != want.Confidence {
			t.Fatalf("canonical row was lost or changed: %+v, want %+v", got, want)
		}
	}
}

func requireNativeRecoveryRanking(t *testing.T, s *Store, want Fact, rank int, fallback string) {
	t.Helper()
	result, err := s.RecallExplainWithOptions(context.Background(), want.AgentID, "signal", 8, RecallOptions{})
	if err != nil || len(result.Receipts) != 1 {
		t.Fatalf("recall: %+v, %v", result, err)
	}
	receipt := result.Receipts[0]
	if receipt.Provenance.FactID != want.ID || receipt.Ranks.VectorRank != rank {
		t.Fatalf("recall returned %+v, want %s at vector rank %d", receipt, want.ID, rank)
	}
	if fallback != "" {
		requireNativeRecoveryFallback(t, result.Retrieval, fallback)
	}
}

func requireNativeRecoveryFallback(t *testing.T, retrieval *RetrievalMetadata, fallback string) {
	t.Helper()
	// Read the additive field without requiring it when the fixture is
	// compiled against the old base for red validation.
	metadata, err := json.Marshal(retrieval)
	if err != nil || !strings.Contains(string(metadata), `"vector_fallback":"`+fallback+`"`) {
		t.Fatalf("fallback receipt = %s, want %q: %v", metadata, fallback, err)
	}
}

func TestNativeVectorRecoveryReplaysTornDeliveryAcrossNamespaces(t *testing.T) {
	f := makeNativeRecoveryFixture(t)
	relative, err := filepath.Rel(filepath.Join(f.cfg.DataDir, "vectors"), f.tornPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := Open(f.cfg)
		if err != nil {
			t.Fatalf("open %d with a durable replay intent: %v", i+1, err)
		}
		t.Cleanup(func() { _ = s.Close() })
		requireNativeRecoveryCanonical(t, s, f)
		if count := s.PendingVectorCount(); count != 0 {
			t.Fatalf("open %d has %d outstanding vectors", i+1, count)
		}
		requireNativeRecoveryRanking(t, s, f.replacement, 1, "")
		requireNativeRecoveryRanking(t, s, f.other, 1, "")
		for _, want := range []Fact{f.replacement, f.other} {
			rows, err := s.vectors.Query(context.Background(), want.AgentID, []float32{1, 0}, 8)
			if err != nil || len(rows) != 1 || rows[0].ID != want.ID {
				t.Fatalf("rebuilt native index for %s: %+v, %v", want.AgentID, rows, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		quarantines, err := filepath.Glob(filepath.Join(f.cfg.DataDir, "vectors.quarantine-*"))
		if err != nil || len(quarantines) != 1 {
			t.Fatalf("quarantined indexes after open %d: %v, %v", i+1, quarantines, err)
		}
		torn, err := os.Stat(filepath.Join(quarantines[0], "vectors", relative))
		if err != nil || torn.Size() != 0 {
			t.Fatalf("original damaged file was not preserved: %v, %v", torn, err)
		}
	}
}

func TestNativeVectorRecoveryReadOnlyUsesCanonicalFallback(t *testing.T) {
	f := makeNativeRecoveryFixture(t)
	cfg := f.cfg
	cfg.ReadOnly, cfg.StrictWrite = true, false
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("read-only canonical open with damaged derived index: %v", err)
	}
	defer s.Close()
	if !s.IsReadOnly() {
		t.Fatal("fixture opened a writable store")
	}
	requireNativeRecoveryCanonical(t, s, f)
	requireNativeRecoveryRanking(t, s, f.replacement, 0, "pending_vector_index")
	requireNativeRecoveryRanking(t, s, f.other, 0, "vector_index_unavailable")
	if count := s.PendingVectorCount(); count != 1 {
		t.Fatalf("read-only open changed the pending marker: %d", count)
	}
	quarantines, err := filepath.Glob(filepath.Join(f.cfg.DataDir, "vectors.quarantine-*"))
	if err != nil || len(quarantines) != 0 {
		t.Fatalf("read-only open moved derived data: %v, %v", quarantines, err)
	}
	if info, err := os.Stat(f.tornPath); err != nil || info.Size() != 0 {
		t.Fatalf("read-only open changed the damaged file: %v, %v", info, err)
	}
}

func TestNativeVectorRecoveryLeavesUnsafePathInPlace(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := makeNativeRecoveryFixture(t)
			index := filepath.Join(f.cfg.DataDir, "vectors")
			parked := filepath.Join(f.cfg.DataDir, "fixture-index")
			if err := os.Rename(index, parked); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				if err := os.WriteFile(index, []byte("preserve this file"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(parked, index); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			before, err := os.Lstat(index)
			if err != nil {
				t.Fatal(err)
			}
			s, err := Open(f.cfg)
			if err != nil {
				t.Fatalf("canonical service after unsafe vector path: %v", err)
			}
			defer s.Close()
			requireNativeRecoveryCanonical(t, s, f)
			requireNativeRecoveryRanking(t, s, f.replacement, 0, "pending_vector_index")
			if count := s.PendingVectorCount(); count != 1 {
				t.Fatalf("unsafe vector path changed the pending marker: %d", count)
			}
			after, err := os.Lstat(index)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("unsafe path moved or replaced: %v, %v", after, err)
			}
			quarantines, err := filepath.Glob(filepath.Join(f.cfg.DataDir, "vectors.quarantine-*"))
			if err != nil || len(quarantines) != 0 {
				t.Fatalf("unsafe path was quarantined: %v, %v", quarantines, err)
			}
		})
	}
}

func TestNativeVectorRecoveryDoesNotMaskNonpendingOpenFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vectors"), []byte("preserve this file"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(StoreConfig{DataDir: dir})
	if err == nil {
		defer s.Close()
		t.Fatal("unrelated native open failure was masked without a pending delivery")
	}
	if !strings.Contains(err.Error(), "open vector store") {
		t.Fatalf("native open failure = %v", err)
	}
}

func TestNativeVectorRecoveryAfterAcknowledgedDelivery(t *testing.T) {
	for _, damage := range []string{"torn", "missing"} {
		t.Run(damage, func(t *testing.T) {
			f := seedNativeRecoveryFixture(t, false)
			peerPath := nativeRecoveryDocumentPath(t, f.cfg.DataDir, f.peer.ID)
			stamp := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(peerPath, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "torn":
				if err := os.Truncate(f.tornPath, 0); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(f.tornPath); err != nil {
					t.Fatal(err)
				}
			}

			requireAcknowledgedVectorReadOnlyFallback(t, f)
			wantQuarantines := 0
			if damage == "torn" {
				wantQuarantines = 1
			}
			for i := 0; i < 2; i++ {
				s, err := Open(f.cfg)
				if err != nil {
					t.Fatalf("writable open %d after %s acknowledged delivery: %v", i+1, damage, err)
				}
				t.Cleanup(func() { _ = s.Close() })
				requireNativeRecoveryCanonical(t, s, f)
				if pending := s.PendingVectorCount(); pending != 0 {
					t.Fatalf("open %d left %d pending writes", i+1, pending)
				}
				requireActualNativeDocuments(t, s, f.replacement, f.peer, f.other)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				quarantines, err := filepath.Glob(filepath.Join(f.cfg.DataDir, "vectors.quarantine-*"))
				if err != nil || len(quarantines) != wantQuarantines {
					t.Fatalf("open %d quarantine count: %v, %v, want %d", i+1, quarantines, err, wantQuarantines)
				}
				if damage == "missing" {
					if peer, err := os.Stat(peerPath); err != nil || !peer.ModTime().Equal(stamp) {
						t.Fatalf("missing-file repair rewrote a healthy document: %v, %v", peer, err)
					}
				}
			}
		})
	}
}

func requireAcknowledgedVectorReadOnlyFallback(t *testing.T, f nativeRecoveryFixture) {
	t.Helper()
	canonical := filepath.Join(f.cfg.DataDir, "gray.db")
	before, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.cfg
	cfg.ReadOnly, cfg.StrictWrite = true, false
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("read-only open with lost acknowledged delivery: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	requireNativeRecoveryCanonical(t, s, f)
	if pending := s.PendingVectorCount(); pending != 0 {
		t.Fatalf("read-only open added %d pending writes", pending)
	}
	health, err := s.EmbeddingHealth()
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(health)
	if err != nil || !strings.Contains(string(state), `"vector_search_degraded":true`) {
		t.Fatalf("read-only native index loss health = %s, %v", state, err)
	}
	result, err := s.RecallExplainWithOptions(context.Background(), nativeRecoveryAgent, "signal", 8, RecallOptions{})
	if err != nil || len(result.Receipts) != 2 {
		t.Fatalf("canonical read-only recall: %+v, %v", result, err)
	}
	for _, receipt := range result.Receipts {
		if id := receipt.Provenance.FactID; (id != f.replacement.ID && id != f.peer.ID) || receipt.Ranks.VectorRank != 0 {
			t.Fatalf("unsafe read-only receipt: %+v", receipt)
		}
	}
	requireNativeRecoveryFallback(t, result.Retrieval, "vector_index_unavailable")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(canonical)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read-only recovery changed canonical database: %v", err)
	}
}

func requireActualNativeDocuments(t *testing.T, s *Store, facts ...Fact) {
	t.Helper()
	vectors, ok := s.vectors.(*chromemVectorStore)
	if !ok {
		t.Fatalf("writable repair did not restore the native backend: %T", s.vectors)
	}
	for _, want := range facts {
		collection := vectors.db.GetCollection(want.AgentID, nil)
		if collection == nil {
			t.Fatalf("native collection %q was not restored", want.AgentID)
		}
		doc, err := collection.GetByID(context.Background(), want.ID)
		if err != nil || doc.ID != want.ID || doc.Content != want.Text {
			t.Fatalf("native document %q was not restored: %+v, %v", want.ID, doc, err)
		}
	}
}
