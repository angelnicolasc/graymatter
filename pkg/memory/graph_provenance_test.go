package memory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	bolt "go.etcd.io/bbolt"
)

var errGraphFixture = errors.New("graph fixture failure")
var bucketGraphFixture = []byte("test_graph_support")

type graphLifecycleWriter struct {
	db        *bolt.DB
	identity  string
	failWrite bool
	failDrop  bool
	failAck   bool
	crashDrop bool
	decays    int
}

func (*graphLifecycleWriter) UpsertNode(string, string, string) error     { return nil }
func (*graphLifecycleWriter) NeighborTexts(string, int) ([]string, error) { return nil, nil }
func (g *graphLifecycleWriter) GraphIdentity() (string, error)            { return g.identity, nil }
func (g *graphLifecycleWriter) OwnsGraphTx(tx *bolt.Tx) bool              { return tx.DB() == g.db }
func (g *graphLifecycleWriter) WriteGraphExtractionTx(tx *bolt.Tx, _ []EntityRef, _ []EntityLink, source GraphSupport) error {
	b, err := tx.CreateBucketIfNotExists(bucketGraphFixture)
	if err != nil {
		return err
	}
	if err := b.Put([]byte(source.Digest), []byte(source.Namespace)); err != nil {
		return err
	}
	if g.failWrite {
		return errGraphFixture
	}
	return nil
}

func (g *graphLifecycleWriter) RemoveGraphSupportTx(tx *bolt.Tx, source GraphSupport) error {
	if b := tx.Bucket(bucketGraphFixture); b != nil {
		if err := b.Delete([]byte(source.Digest)); err != nil {
			return err
		}
	}
	if g.crashDrop {
		os.Exit(71)
	}
	if g.failDrop {
		return errGraphFixture
	}
	return nil
}

func (g *graphLifecycleWriter) WriteGraphExtraction(refs []EntityRef, links []EntityLink, source GraphSupport) error {
	return g.db.Update(func(tx *bolt.Tx) error {
		return g.WriteGraphExtractionTx(tx, refs, links, source)
	})
}

func (g *graphLifecycleWriter) RemoveGraphSupport(source GraphSupport) error {
	err := g.db.Update(func(tx *bolt.Tx) error {
		return g.RemoveGraphSupportTx(tx, source)
	})
	if err == nil && g.failAck {
		return errGraphFixture
	}
	return err
}

func (g *graphLifecycleWriter) DecayGraph(time.Duration) error {
	g.decays++
	return nil
}

func (g *graphLifecycleWriter) supports(t *testing.T) map[string]string {
	t.Helper()
	supports := make(map[string]string)
	if err := g.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketGraphFixture); b != nil {
			return b.ForEach(func(k, v []byte) error {
				supports[string(k)] = string(v)
				return nil
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return supports
}

type graphLifecycleExtractor struct{}

func (graphLifecycleExtractor) ExtractIDs(string) ([]string, error) { return nil, nil }
func (graphLifecycleExtractor) ExtractTyped(string) ([]EntityRef, []EntityLink, error) {
	return []EntityRef{{ID: "project:test", Label: "test", EntityType: "project"}}, nil, nil
}

func openGraphLifecycleStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: embedding.NewKeyword(), CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func graphLifecycleFact(t *testing.T, s *Store) Fact {
	t.Helper()
	f, err := s.PutReturningFact(context.Background(), "provenance", "The service uses a graph.")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func graphLifecycleRevision(t *testing.T, s *Store, f Fact) string {
	t.Helper()
	var digest string
	if err := s.DB().View(func(tx *bolt.Tx) error {
		r, err := graphRevisionTx(tx, f.AgentID, f.ID)
		digest = r.Digest
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		t.Fatal("missing source revision")
	}
	return digest
}

func checkGraphLifecycleWatermark(t *testing.T, s *Store, f Fact, expected string) {
	t.Helper()
	if sig, _ := s.extractedSignature(f.AgentID, f.ID); sig != expected {
		t.Fatalf("watermark = %q, want %q", sig, expected)
	}
}

func TestGraphProvenanceK06SharedRollbackAndCrash(t *testing.T) {
	if dir := os.Getenv("GRAYMATTER_K06_CRASH_DIR"); dir != "" {
		s := openGraphLifecycleStore(t, dir)
		writer := &graphLifecycleWriter{db: s.DB(), crashDrop: true}
		s.SetKG(writer, graphLifecycleExtractor{})
		if err := s.Delete("provenance", os.Getenv("GRAYMATTER_K06_CRASH_FACT")); err != nil {
			t.Fatal(err)
		}
		t.Fatal("delete did not reach crash point")
	}

	ctx := context.Background()
	dir := t.TempDir()
	s := openGraphLifecycleStore(t, dir)
	t.Cleanup(func() { _ = s.Close() })
	writer := &graphLifecycleWriter{db: s.DB(), failWrite: true}
	s.SetKG(writer, graphLifecycleExtractor{})
	f := graphLifecycleFact(t, s)
	if err := s.Consolidate(ctx, f.AgentID, defaultTestCfg()); !errors.Is(err, errGraphFixture) {
		t.Fatalf("failed graph extraction = %v", err)
	}
	digest := graphLifecycleRevision(t, s, f)
	checkGraphLifecycleWatermark(t, s, f, "")
	if len(writer.supports(t)) != 0 {
		t.Fatal("failed extraction leaked graph support")
	}

	writer.failWrite = false
	if err := s.Consolidate(ctx, f.AgentID, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	checkGraphLifecycleWatermark(t, s, f, "revision:"+digest)
	writer.failDrop = true
	if err := s.Delete(f.AgentID, f.ID); !errors.Is(err, errGraphFixture) {
		t.Fatalf("failed graph cleanup = %v", err)
	}
	if _, ok := s.loadFact(f.AgentID, f.ID); !ok || writer.supports(t)[digest] != f.AgentID {
		t.Fatal("cleanup failure lost fact or graph support")
	}
	checkGraphLifecycleWatermark(t, s, f, "revision:"+digest)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	crashCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	child := exec.CommandContext(crashCtx, os.Args[0], "-test.run=^TestGraphProvenanceK06SharedRollbackAndCrash$")
	child.Env = append(os.Environ(), "GRAYMATTER_K06_CRASH_DIR="+dir, "GRAYMATTER_K06_CRASH_FACT="+f.ID)
	out, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 71 {
		t.Fatalf("crash exit = %v, output: %s", err, out)
	}

	reopened := openGraphLifecycleStore(t, dir)
	defer reopened.Close()
	writer = &graphLifecycleWriter{db: reopened.DB()}
	reopened.SetGraphMaintenance(writer)
	if _, ok := reopened.loadFact(f.AgentID, f.ID); !ok || writer.supports(t)[digest] != f.AgentID {
		t.Fatal("uncommitted cleanup survived the crash")
	}
	checkGraphLifecycleWatermark(t, reopened, f, "revision:"+digest)
	if err := reopened.Delete(f.AgentID, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.loadFact(f.AgentID, f.ID); ok || len(writer.supports(t)) != 0 {
		t.Fatal("cleanup retry left canonical or graph state")
	}
	checkGraphLifecycleWatermark(t, reopened, f, "")
	if err := reopened.Consolidate(ctx, f.AgentID, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	if writer.decays != 1 {
		t.Fatal("maintenance skipped graph decay after the last fact was deleted")
	}
}

func TestGraphProvenanceK06SeparateOutboxProtectsRestoredRevision(t *testing.T) {
	ctx := context.Background()
	factDir, graphPath := t.TempDir(), filepath.Join(t.TempDir(), "graph.db")
	s := openGraphLifecycleStore(t, factDir)
	t.Cleanup(func() { _ = s.Close() })
	openWriter := func() *graphLifecycleWriter {
		db, err := bolt.Open(graphPath, 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return &graphLifecycleWriter{db: db, identity: "original-graph"}
	}
	writer := openWriter()
	s.SetKG(writer, graphLifecycleExtractor{})
	f := graphLifecycleFact(t, s)
	if err := s.Consolidate(ctx, f.AgentID, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	first := graphLifecycleRevision(t, s, f)

	// A same-text change to alias and back creates new exact revisions.
	alias := f
	alias.Kind = KindAlias
	if err := s.UpdateFact(f.AgentID, alias); err != nil {
		t.Fatal(err)
	}
	var aliasSource GraphSupport
	if err := s.DB().Update(func(tx *bolt.Tx) error {
		var err error
		aliasSource, err = registerGraphRevisionTx(tx, alias, "external:"+writer.identity)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteGraphExtraction(nil, nil, aliasSource); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateFact(f.AgentID, f); err != nil {
		t.Fatal(err)
	}

	writer.failDrop = true
	if err := s.Consolidate(ctx, f.AgentID, defaultTestCfg()); !errors.Is(err, errGraphFixture) {
		t.Fatalf("failed outbox delivery = %v", err)
	}
	restored := graphLifecycleRevision(t, s, f)
	supports := writer.supports(t)
	if restored == first || restored == aliasSource.Digest || len(supports) != 3 ||
		supports[first] != f.AgentID || supports[aliasSource.Digest] != f.AgentID || supports[restored] != f.AgentID {
		t.Fatalf("unexpected supports before replay: %v", supports)
	}
	if err := s.DB().View(func(tx *bolt.Tx) error {
		if pending := GraphCleanupPendingTx(tx); pending != 2 {
			t.Fatalf("cleanup intents = %d, want 2", pending)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Model a crash or lost reply after an external cleanup commit, before
	// the canonical queue acknowledgement. Reopening must retry safely.
	writer.failDrop, writer.failAck = false, true
	if err := s.drainGraphCleanup(writer); !errors.Is(err, errGraphFixture) {
		t.Fatalf("lost cleanup acknowledgement = %v", err)
	}
	if supports := writer.supports(t); len(supports) != 2 || supports[restored] != f.AgentID {
		t.Fatalf("unexpected supports after unacknowledged commit: %v", supports)
	}
	otherDB, err := bolt.Open(filepath.Join(t.TempDir(), "other.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	other := &graphLifecycleWriter{db: otherDB, identity: "different-graph"}
	s.SetKG(other, graphLifecycleExtractor{})
	if err := s.Consolidate(ctx, f.AgentID, defaultTestCfg()); !errors.Is(err, errGraphTargetChanged) {
		t.Fatalf("replacement graph accepted pending delivery: %v", err)
	}
	if err := s.DB().View(func(tx *bolt.Tx) error {
		if pending := GraphCleanupPendingTx(tx); pending != 2 {
			t.Fatalf("replacement graph acknowledged cleanup intents: %d remain", pending)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(other.supports(t)) != 0 {
		t.Fatal("replacement graph received support")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.db.Close(); err != nil {
		t.Fatal(err)
	}

	s = openGraphLifecycleStore(t, factDir)
	writer = openWriter()
	s.SetKG(writer, graphLifecycleExtractor{})
	for i := 0; i < 2; i++ {
		if err := s.Consolidate(ctx, f.AgentID, defaultTestCfg()); err != nil {
			t.Fatal(err)
		}
		supports := writer.supports(t)
		if len(supports) != 1 || supports[restored] != f.AgentID {
			t.Fatalf("replay removed new support or left stale support: %v", supports)
		}
		checkGraphLifecycleWatermark(t, s, f, "revision:"+restored)
	}
}
