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
	// Fixed clock: idempotency is a property of the arithmetic, not of how
	// fast the machine moves between two calls.
	now := time.Now()
	if err := g.DecayGraphAt(now, halfLife); err != nil {
		t.Fatalf("first decay: %v", err)
	}
	w1 := nodeWeight(t, g, "person:maria")

	if err := g.DecayGraphAt(now, halfLife); err != nil {
		t.Fatalf("second decay: %v", err)
	}
	w2 := nodeWeight(t, g, "person:maria")

	if w1 != w2 {
		t.Fatalf("decay is not idempotent at a fixed instant: first %.12f, second %.12f", w1, w2)
	}
	// One half-life stale halves the weight (min() keeps it from going lower).
	if math.Abs(w1-0.5) > 0.01 {
		t.Fatalf("weight after one half-life = %.4f, want ~0.5", w1)
	}

	// The wall-clock wrapper stays honest: real elapsed time may decay
	// further but must never resurrect weight (min() rule).
	if err := g.DecayGraph(halfLife); err != nil {
		t.Fatal(err)
	}
	w3 := nodeWeight(t, g, "person:maria")
	if w3 > w1+1e-9 {
		t.Fatalf("weight rose across real elapsed time: %.6f -> %.6f", w1, w3)
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

	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	stale := now.Add(-8760 * time.Hour)
	source := SourceSupport{Namespace: "agent", FactID: "fact", Digest: "generation-1"}
	if err := g.Upsert(Node{ID: "person:ghost", Label: "ghost", EntityType: "person", Weight: 0.02, LastSeen: stale}); err != nil {
		t.Fatal(err)
	}
	if err := g.Upsert(Node{ID: "concept:orphan", Label: "orphan", EntityType: "concept", Weight: 0.02, LastSeen: stale}); err != nil {
		t.Fatal(err)
	}
	if err := g.LinkDerived(Edge{From: "person:ghost", To: "concept:y", Relation: "co_mentioned", Weight: 0.001, CreatedAt: stale}, source); err != nil {
		t.Fatal(err)
	}

	if err := g.DecayGraphAt(now, 720*time.Hour); err != nil {
		t.Fatal(err)
	}
	nodes, edges, err := g.Neighbors("person:ghost", 1)
	if err != nil || len(nodes) != 1 || len(edges) != 1 || edges[0].Weight >= 0.01 {
		t.Fatalf("low-weight supported edge must remain traversable: %v, %v, %v", nodes, edges, err)
	}
	requireNodePresence(t, g, "person:ghost", true)
	requireNodePresence(t, g, "concept:orphan", false)

	if err := g.Link(Edge{From: "person:other", To: "concept:y", Relation: "manual", CreatedAt: stale}); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveSupport(source); err != nil {
		t.Fatal(err)
	}
	requireNodePresence(t, g, "person:ghost", true)
	if err := g.DecayGraphAt(now, 720*time.Hour); err != nil {
		t.Fatal(err)
	}
	requireNodePresence(t, g, "person:ghost", false)
	requireNodePresence(t, g, "concept:y", true)
}

func requireNodePresence(t *testing.T, g *Graph, id string, want bool) {
	t.Helper()
	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.ID == id {
			if !want {
				t.Fatalf("orphan node %q survived decay", id)
			}
			return
		}
	}
	if want {
		t.Fatalf("node %q was pruned", id)
	}
}
