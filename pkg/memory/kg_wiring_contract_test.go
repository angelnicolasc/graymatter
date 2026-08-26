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

// --- v0.12.0 wiring: typed extraction creates edges; recall budget caps ----

type typedFake struct{ plainCalls int }

func (e *typedFake) ExtractIDs(text string) ([]string, error) {
	e.plainCalls++
	return []string{"entity-a", "entity-b"}, nil
}

func (e *typedFake) ExtractTyped(text string) ([]EntityRef, []EntityLink, error) {
	return []EntityRef{
			{ID: "entity-a", Label: "Entity A", EntityType: "project"},
			{ID: "entity-b", Label: "Entity B", EntityType: "person"},
		}, []EntityLink{
			{From: "entity-a", To: "entity-b", Relation: "co_mentioned"},
		}, nil
}

type edgeWritingGraph struct {
	upserts []EntityRef
	links   []EntityLink
}

func (g *edgeWritingGraph) UpsertNode(id, label, entityType string) error {
	g.upserts = append(g.upserts, EntityRef{ID: id, Label: label, EntityType: entityType})
	return nil
}

func (g *edgeWritingGraph) LinkEdges(links []EntityLink, sourceFactID string) error {
	for _, l := range links {
		l.Sources = []string{sourceFactID}
		g.links = append(g.links, l)
	}
	return nil
}

func (g *edgeWritingGraph) NeighborTexts(nodeID string, depth int) ([]string, error) {
	return []string{"n1", "n2", "n3", "n4", "n5"}, nil
}

// TestConsolidate_TypedPath_CreatesTypedNodesAndEdges pins the P2 core: with
// a capability-typed extractor wired, consolidation preserves labels and
// entity types AND produces the co-mention edges that make enrichment
// traversable. This is the test whose absence let issue #24 ship.
func TestConsolidate_TypedPath_CreatesTypedNodesAndEdges(t *testing.T) {
	s := newKGWiringStore(t)
	g := &edgeWritingGraph{}
	ex := &typedFake{}
	s.SetKG(g, ex)

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.Put(ctx, "kg-agent", fmt.Sprintf("Fact %d naming two entities", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Consolidate(ctx, "kg-agent", &testConsolidateCfg{threshold: 100, halfLife: 720 * time.Hour}); err != nil {
		t.Fatal(err)
	}

	if len(g.upserts) != 4 { // 2 facts x 2 refs
		t.Fatalf("upserts = %d, want 4", len(g.upserts))
	}
	for _, u := range g.upserts {
		if u.EntityType == "" || u.Label == "" {
			t.Errorf("typed path lost fidelity: %+v", u)
		}
	}
	if len(g.links) != 2 { // one co_mentioned pair per fact
		t.Fatalf("links = %d, want 2", len(g.links))
	}
	if g.links[0].Relation != "co_mentioned" || g.links[0].From != "entity-a" || g.links[0].To != "entity-b" {
		t.Errorf("unexpected link: %+v", g.links[0])
	}
}

// TestRecall_KGNeighborBudgetCapsAtThree pins ADR-003 condition 2's budget:
// with the graph wired and a hub returning five neighbours, Recall appends
// exactly three - enrichment is a hint with a fixed budget, not a second
// result set.
func TestRecall_KGNeighborBudgetCapsAtThree(t *testing.T) {
	s := newKGWiringStore(t)
	s.SetKG(&edgeWritingGraph{}, &idsOnlyExtractor{})

	ctx := context.Background()
	if err := s.Put(ctx, "kg-agent", "the single seed fact"); err != nil {
		t.Fatal(err)
	}

	res, err := s.Recall(ctx, "kg-agent", "anything at all", 8)
	if err != nil {
		t.Fatal(err)
	}
	appended := len(res) - 1
	if appended != 3 {
		t.Fatalf("enrichment appended %d neighbours, want capped 3 (result len %d)", appended, len(res))
	}
}

type idsOnlyExtractor struct{}

func (e *idsOnlyExtractor) ExtractIDs(text string) ([]string, error) {
	return []string{"hub-entity"}, nil
}

func TestPutConfident_ValidatesAndPersists(t *testing.T) {
	s := newKGWiringStore(t)
	ctx := context.Background()

	if err := s.PutConfident(ctx, "c-agent", "a verified claim", "bogus"); err == nil {
		t.Fatal("invalid confidence accepted")
	}
	if err := s.PutConfident(ctx, "c-agent", "a verified claim", "verified"); err != nil {
		t.Fatal(err)
	}

	stored, _ := s.List("c-agent")
	if len(stored) != 1 || stored[0].Confidence != "verified" {
		t.Fatalf("confidence not persisted: %+v", stored)
	}
}

// decayRecordingKG implements GraphAccessor plus the optional GraphDecayer
// capability, and counts decay invocations.
type decayRecordingKG struct {
	recordingKG
	decayCalls []time.Duration
}

func (g *decayRecordingKG) DecayGraph(halfLife time.Duration) error {
	g.decayCalls = append(g.decayCalls, halfLife)
	return nil
}

// A graph that carries the capability gets exactly one decay pass per
// consolidation cycle, with the configured half-life. This is the executable
// end of the Step 5 wiring; the removed dead DecayGraph never had a caller.
func TestConsolidate_DecaysCapableGraphsOncePerCycle(t *testing.T) {
	s := newKGWiringStore(t)
	ctx := context.Background()
	kg := &decayRecordingKG{}
	ex := &recordingExtractor{}
	s.SetKG(kg, ex)

	for i := 0; i < 25; i++ {
		if err := s.Put(ctx, "decay-agent", fmt.Sprintf("Maria Rodriguez discussed entity topic %d in depth", i)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &testConsolidateCfg{llm: "", halfLife: 48 * time.Hour, threshold: 20}
	if err := s.Consolidate(ctx, "decay-agent", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(kg.decayCalls) != 1 {
		t.Fatalf("decay calls = %d, want exactly 1 per cycle", len(kg.decayCalls))
	}
	if kg.decayCalls[0] != 48*time.Hour {
		t.Errorf("half-life passed = %v, want the configured 48h", kg.decayCalls[0])
	}
}
