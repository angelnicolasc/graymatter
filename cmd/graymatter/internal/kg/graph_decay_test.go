package kg

import (
	"math"
	"testing"
	"time"
)

// DecayGraph must be idempotent: weight recomputes from staleness, so two
// cycles in the same instant leave the same weight as one. This is the exact
// regression of the compounding-decay bug the fact side already fixed - the
// removed multiplicative implementation halved once per call.

func seedDecayGraph(t *testing.T) (*Graph, func()) {
	t.Helper()
	g, cleanup := openTestGraph(t)

	old := time.Now().Add(-720 * time.Hour) // exactly one half-life stale
	if err := g.Upsert(Node{ID: "person:maria", Label: "Maria", EntityType: "person", Weight: 1.0, LastSeen: old}); err != nil {
		t.Fatal(err)
	}
	if err := g.Link(Edge{From: "person:maria", To: "concept:x", Relation: "co_mentioned", Weight: 1.0, CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	return g, cleanup
}

func nodeWeight(t *testing.T, g *Graph, id string) float64 {
	t.Helper()
	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == id {
			return n.Weight
		}
	}
	t.Fatalf("node %s missing", id)
	return 0
}

func TestDecayGraph_IdempotentWithinInstant(t *testing.T) {
	g, cleanup := seedDecayGraph(t)
	defer cleanup()

	halfLife := 720 * time.Hour
	if err := g.DecayGraph(halfLife); err != nil {
		t.Fatalf("first decay: %v", err)
	}
	w1 := nodeWeight(t, g, "person:maria")

	if err := g.DecayGraph(halfLife); err != nil {
		t.Fatalf("second decay: %v", err)
	}
	w2 := nodeWeight(t, g, "person:maria")

	if math.Abs(w1-w2) > 1e-9 {
		t.Fatalf("decay is not idempotent: first %.6f, second %.6f - re-running cycles must not forget faster", w1, w2)
	}
	// One half-life stale halves the weight (min() keeps it from going lower).
	if math.Abs(w1-0.5) > 0.01 {
		t.Fatalf("weight after one half-life = %.4f, want ~0.5", w1)
	}
}

func TestDecayGraph_NeverResurrectsFreshWeight(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	// A node whose weight was manually driven low but whose LastSeen is
	// fresh: decay must not hand weight back.
	if err := g.Upsert(Node{ID: "concept:cold", Label: "cold", EntityType: "concept", Weight: 0.02}); err != nil {
		t.Fatal(err)
	}
	if err := g.DecayGraph(720 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if w := nodeWeight(t, g, "concept:cold"); w < 0.02-1e-9 {
		t.Fatalf("fresh-but-light node lost weight: %.4f", w)
	}
}

func TestDecayGraph_PrunesBelowFloorAndKeepsEdgesConsistent(t *testing.T) {
	g, cleanup := seedDecayGraph(t)
	defer cleanup()

	// Drive the node under the floor with an ancient LastSeen.
	if err := g.Upsert(Node{ID: "person:ghost", Label: "ghost", EntityType: "person", Weight: 0.02, LastSeen: time.Now().Add(-8760 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := g.Link(Edge{From: "person:ghost", To: "concept:y", Relation: "co_mentioned", Weight: 0.001, CreatedAt: time.Now().Add(-8760 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if err := g.DecayGraph(720 * time.Hour); err != nil {
		t.Fatal(err)
	}

	nodes, _ := g.AllNodes()
	for _, n := range nodes {
		if n.ID == "person:ghost" {
			t.Error("sub-floor node survived pruning")
		}
	}
	// The healthy node and its edge remain traversable.
	if w := nodeWeight(t, g, "person:maria"); w <= 0 {
		t.Error("healthy node wrongly pruned")
	}
}
