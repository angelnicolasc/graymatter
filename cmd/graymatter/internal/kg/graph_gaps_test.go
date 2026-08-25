package kg

import (
	"testing"
)

// The link path carries two guarantees the audit called out: an edge must
// always be traversable (endpoints auto-created as placeholders, issue #24),
// and provenance must accumulate across facts without growing unbounded.

func TestLink_EmptyEndpointsError(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	if err := g.Link(Edge{To: "b", Relation: "r"}); err == nil {
		t.Error("empty From accepted")
	}
	if err := g.Link(Edge{From: "a", Relation: "r"}); err == nil {
		t.Error("empty To accepted")
	}
}

func TestLink_AutoCreatesPlaceholderEndpoints(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	if err := g.Link(Edge{From: "orphan-a", To: "orphan-b", Relation: "co_mentioned"}); err != nil {
		t.Fatalf("link: %v", err)
	}
	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, n := range nodes {
		seen[n.ID] = n.EntityType
	}
	for _, id := range []string{"orphan-a", "orphan-b"} {
		if seen[id] != "unknown" {
			t.Errorf("placeholder %q type = %q, want unknown; link-before-extract must stay traversable", id, seen[id])
		}
	}
}

func TestLink_SourcesMergeAcrossFactsAndCapAtMaxEdgeSources(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()
	pair := Edge{From: "a", To: "b", Relation: "co_mentioned"}

	// Three facts mention the pair; a duplicate re-mention adds nothing.
	for _, src := range []string{"fact-1", "fact-2", "fact-1", "fact-3"} {
		e := pair
		e.Sources = []string{src}
		if err := g.Link(e); err != nil {
			t.Fatal(err)
		}
	}
	edges, err := g.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1 deduplicated pair", len(edges))
	}
	if got := len(edges[0].Sources); got != 3 {
		t.Fatalf("sources = %v (%d), want exactly fact-1..3 merged once each", edges[0].Sources, got)
	}

	// A flood of further mentions caps the receipt list at maxEdgeSources,
	// keeping the oldest provenance rather than churning it.
	for i := 0; i < maxEdgeSources+4; i++ {
		e := pair
		e.Sources = []string{string(rune('A' + i))}
		if err := g.Link(e); err != nil {
			t.Fatal(err)
		}
	}
	edges, err = g.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges[0].Sources) > maxEdgeSources {
		t.Errorf("sources grew past the cap: %d", len(edges[0].Sources))
	}
	found := false
	for _, s := range edges[0].Sources {
		if s == "fact-1" {
			found = true // oldest receipts are the ones kept
		}
	}
	if !found {
		t.Error("capping dropped the oldest provenance")
	}
}

func TestNeighbors_ZeroDepthReturnsNothing(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()
	if nodes, edges, err := g.Neighbors("any", 0); nodes != nil || edges != nil || err != nil {
		t.Errorf("depth 0 = %v, %v, %v; want nothing, no error", nodes, edges, err)
	}
}
