package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The wiring contract for the knowledge graph, pinned at the engine boundary.
//
// Shipped builds never auto-wire extraction: SetKG is an explicit call a
// library owner makes. Issue #24 existed because that contract lived only in
// prose — nothing would have caught the cable being connected (or
// disconnected) silently. These tests are the executable form of the prose.

type recordingKG struct {
	upserts       []string
	neighborCalls int
}

func (g *recordingKG) UpsertNode(id, label, entityType string) error {
	g.upserts = append(g.upserts, id)
	return nil
}

func (g *recordingKG) NeighborTexts(nodeID string, depth int) ([]string, error) {
	g.neighborCalls++
	return nil, nil
}

type recordingExtractor struct{ calls int }

func (e *recordingExtractor) ExtractIDs(text string) ([]string, error) {
	e.calls++
	return []string{"entity-a", "entity-b"}, nil
}

func newKGWiringStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestShippedDefaultsNeverAutoWireKG: a default-built store runs full
// consolidation cycles with graph and extractor still nil. If anyone adds
// silent auto-wiring to Open/NewWithConfig/Consolidate, this fails.
func TestShippedDefaultsNeverAutoWireKG(t *testing.T) {
	s := newKGWiringStore(t)
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		if err := s.Put(ctx, "kg-agent", fmt.Sprintf("Maria Rodriguez discussed entity topic %d in depth", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Consolidate(ctx, "kg-agent", &testConsolidateCfg{threshold: 100, halfLife: 720 * time.Hour}); err != nil {
		t.Fatal(err)
	}

	if s.graph != nil || s.extractor != nil {
		t.Fatalf("shipped stack auto-wired the graph: graph=%v extractor=%v",
			s.graph != nil, s.extractor != nil)
	}
}

// TestExplicitSetKG_DrivesNodeUpserts_ButNoEdgePathExists pins both halves of
// today's manual-wiring reality:
//
//   - explicit SetKG makes consolidation upsert nodes per extracted entity;
//   - the engine has NO path that creates edges — GraphAccessor exposes no
//     Link, so D1 (isolated nodes) is structural, not accidental.
//
// When P2.2 consumes extractor edges, this second assertion must be replaced
// by real edge-flow tests; deleting it silently would repeat issue #24.
func TestExplicitSetKG_DrivesNodeUpserts_ButNoEdgePathExists(t *testing.T) {
	s := newKGWiringStore(t)
	ctx := context.Background()

	kg := &recordingKG{}
	ex := &recordingExtractor{}
	s.SetKG(kg, ex)

	for i := 0; i < 3; i++ {
		if err := s.Put(ctx, "kg-agent", fmt.Sprintf("Fact %d mentioning two entities worth linking", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Consolidate(ctx, "kg-agent", &testConsolidateCfg{threshold: 100, halfLife: 720 * time.Hour}); err != nil {
		t.Fatal(err)
	}

	if len(kg.upserts) == 0 {
		t.Fatal("SetKG wired but consolidation produced no node upserts")
	}
	if ex.calls == 0 {
		t.Error("extractor never invoked during consolidation")
	}
	for _, u := range kg.upserts {
		if u != "entity-a" && u != "entity-b" {
			t.Errorf("unexpected node id %q from extractor stub", u)
		}
	}
}
