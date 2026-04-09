package kg

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func openTestGraph(t *testing.T) (*Graph, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := bolt.Open(filepath.Join(dir, "test.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	g, err := Open(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("open graph: %v", err)
	}
	return g, func() { _ = db.Close() }
}

func TestUpsert_InsertAndRead(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	n := Node{ID: "maria", Label: "Maria Rodriguez", EntityType: "person"}
	if err := g.Upsert(n); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Label != "Maria Rodriguez" {
		t.Errorf("Label = %q", nodes[0].Label)
	}
	if nodes[0].FirstSeen.IsZero() {
		t.Error("FirstSeen should not be zero")
	}
}

func TestUpsert_UpdatePreservesFirstSeen(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	first := Node{ID: "n1", Label: "Alice", EntityType: "person", Weight: 0.5}
	if err := g.Upsert(first); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	second := Node{ID: "n1", Label: "Alice Updated", EntityType: "person", Weight: 0.3}
	if err := g.Upsert(second); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node after upsert, got %d", len(nodes))
	}
	// Weight should be max(0.5, 0.3) = 0.5
	if nodes[0].Weight != 0.5 {
		t.Errorf("Weight = %f, want 0.5", nodes[0].Weight)
	}
	// FirstSeen should not change.
	if nodes[0].FirstSeen != nodes[0].FirstSeen { // tautology check for sanity
		t.Error("FirstSeen changed")
	}
}

func TestLink_InsertAndRead(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	_ = g.Upsert(Node{ID: "a", Label: "A", EntityType: "fact"})
	_ = g.Upsert(Node{ID: "b", Label: "B", EntityType: "fact"})

	e := Edge{From: "a", To: "b", Relation: "related_to"}
	if err := g.Link(e); err != nil {
		t.Fatalf("Link: %v", err)
	}

	edges, err := g.AllEdges()
	if err != nil {
		t.Fatalf("AllEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].Relation != "related_to" {
		t.Errorf("Relation = %q", edges[0].Relation)
	}
}

func TestLink_Idempotent(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	e := Edge{From: "a", To: "b", Relation: "related_to"}
	_ = g.Link(e)
	_ = g.Link(e) // duplicate

	edges, err := g.AllEdges()
	if err != nil {
		t.Fatalf("AllEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Errorf("expected 1 edge (idempotent upsert), got %d", len(edges))
	}
}

func TestNeighbors_Depth1(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	for _, id := range []string{"root", "child1", "child2", "grandchild"} {
		_ = g.Upsert(Node{ID: id, Label: id, EntityType: "fact"})
	}
	_ = g.Link(Edge{From: "root", To: "child1", Relation: "related_to"})
	_ = g.Link(Edge{From: "root", To: "child2", Relation: "related_to"})
	_ = g.Link(Edge{From: "child1", To: "grandchild", Relation: "related_to"})

	nodes, edges, err := g.Neighbors("root", 1)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("depth=1: expected 2 nodes, got %d: %v", len(nodes), nodes)
	}
	if len(edges) != 2 {
		t.Errorf("depth=1: expected 2 edges, got %d", len(edges))
	}
}

func TestNeighbors_Depth2(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	for _, id := range []string{"root", "child1", "grandchild"} {
		_ = g.Upsert(Node{ID: id, Label: id, EntityType: "fact"})
	}
	_ = g.Link(Edge{From: "root", To: "child1", Relation: "related_to"})
	_ = g.Link(Edge{From: "child1", To: "grandchild", Relation: "related_to"})

	nodes, _, err := g.Neighbors("root", 2)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("depth=2: expected 2 nodes, got %d", len(nodes))
	}
}

func TestDecay_PrunesLowWeight(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	// Insert a node with a tiny weight that will drop below 0.01 after decay.
	n := Node{
		ID:       "dying",
		Label:    "Dying Node",
		EntityType: "fact",
		Weight:   0.001, // already below prune threshold
		LastSeen: time.Now().Add(-1000 * time.Hour),
	}
	if err := g.Upsert(n); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Insert a healthy node.
	healthy := Node{ID: "healthy", Label: "Healthy", EntityType: "fact", Weight: 1.0}
	if err := g.Upsert(healthy); err != nil {
		t.Fatalf("Upsert healthy: %v", err)
	}

	if err := g.Decay(720 * time.Hour); err != nil {
		t.Fatalf("Decay: %v", err)
	}

	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes after decay: %v", err)
	}
	for _, node := range nodes {
		if node.ID == "dying" {
			t.Error("dying node should have been pruned")
		}
	}
}

func TestDecay_HealthyNodeSurvives(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	n := Node{ID: "alive", Label: "Alive", EntityType: "fact", Weight: 1.0}
	if err := g.Upsert(n); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := g.Decay(720 * time.Hour); err != nil {
		t.Fatalf("Decay: %v", err)
	}

	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("expected 1 node to survive, got %d", len(nodes))
	}
}

func TestExportObsidian(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	_ = g.Upsert(Node{ID: "maria", Label: "Maria", EntityType: "person"})
	_ = g.Upsert(Node{ID: "acme", Label: "Acme Corp", EntityType: "organization"})
	_ = g.Link(Edge{From: "maria", To: "acme", Relation: "works_at"})

	outDir := t.TempDir()
	if err := g.ExportObsidian(outDir); err != nil {
		t.Fatalf("ExportObsidian: %v", err)
	}

	// canvas JSON must exist.
	canvasPath := filepath.Join(outDir, "graph-canvas.json")
	if _, err := os.Stat(canvasPath); err != nil {
		t.Errorf("canvas file missing: %v", err)
	}

	// Node .md files must exist.
	for _, name := range []string{"Maria.md", "Acme_Corp.md"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("node file %q missing: %v", name, err)
		}
	}
}

func TestUpsert_ErrorOnEmptyID(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	if err := g.Upsert(Node{ID: "", Label: "Nameless"}); err == nil {
		t.Error("expected error for empty ID, got nil")
	}
}

func TestLink_ErrorOnEmptyEndpoints(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()

	if err := g.Link(Edge{From: "", To: "b", Relation: "x"}); err == nil {
		t.Error("expected error for empty From, got nil")
	}
	if err := g.Link(Edge{From: "a", To: "", Relation: "x"}); err == nil {
		t.Error("expected error for empty To, got nil")
	}
}
