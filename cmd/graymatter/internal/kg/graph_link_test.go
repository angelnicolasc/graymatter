package kg

import "testing"

// TestLink_AutoUpsertsEndpointsOnEmptyBuckets pins P0.1: linking over empty
// buckets produces both endpoint nodes plus the edge, so an edge can never
// dangle. Before this fix, Link happily stored an edge whose endpoints did
// not exist — invisible to every traversal, alive in storage forever.
func TestLink_AutoUpsertsEndpointsOnEmptyBuckets(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	if err := g.Link(Edge{From: "maria-rodriguez", To: "pricing-project", Relation: "co_mentioned"}); err != nil {
		t.Fatalf("link over empty buckets: %v", err)
	}

	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 auto-upserted endpoints, got %d (%+v)", len(nodes), nodes)
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
		if n.EntityType != "unknown" {
			t.Errorf("placeholder %q has EntityType %q, want unknown", n.ID, n.EntityType)
		}
		if n.Label != n.ID {
			t.Errorf("placeholder %q has Label %q, want the ID verbatim", n.ID, n.Label)
		}
	}
	for _, id := range []string{"maria-rodriguez", "pricing-project"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("endpoint %q missing from nodes", id)
		}
	}

	neigh, _, err := g.Neighbors("maria-rodriguez", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(neigh) != 1 {
		t.Errorf("edge not traversable from From side: %d neighbours", len(neigh))
	}
}

// TestLink_ExistingEndpointsPreserved guards against overshoot: auto-upsert
// must never clobber real nodes that already carry their label and type.
func TestLink_ExistingEndpointsPreserved(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	if err := g.Upsert(Node{ID: "a", Label: "Alpha", EntityType: "person"}); err != nil {
		t.Fatal(err)
	}
	if err := g.Link(Edge{From: "a", To: "brand-new-node", Relation: "related_to"}); err != nil {
		t.Fatal(err)
	}

	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if got := byID["a"]; got.Label != "Alpha" || got.EntityType != "person" {
		t.Errorf("existing node overwritten: %+v", got)
	}
	if got := byID["brand-new-node"]; got.EntityType != "unknown" {
		t.Errorf("new endpoint not created as placeholder: %+v", got)
	}
}
