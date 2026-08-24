package kg

import "testing"

func TestAnalyze_HubsOrphansAndArticulation(t *testing.T) {
	// Chain: a - b - c  plus isolated d, and hub h connected to x,y,z.
	nodes := []Node{
		{ID: "a", Label: "A"}, {ID: "b", Label: "B"}, {ID: "c", Label: "C"},
		{ID: "d", Label: "D"},
		{ID: "h", Label: "Hub"}, {ID: "x", Label: "X"}, {ID: "y", Label: "Y"}, {ID: "z", Label: "Z"},
	}
	var edges []Edge
	link := func(from, to string) {
		edges = append(edges, Edge{From: from, To: to, Relation: "related_to"})
	}
	link("a", "b")
	link("b", "c")
	link("h", "x")
	link("h", "y")
	link("h", "z")

	rep := Analyze(nodes, edges)

	if rep.NodeCount != 8 || rep.EdgeCount != 5 {
		t.Fatalf("counts wrong: %+v", rep)
	}
	if len(rep.Hubs) == 0 || rep.Hubs[0].ID != "h" || rep.Hubs[0].Degree != 3 {
		t.Errorf("hub mismatch: %+v", rep.Hubs)
	}
	if rep.Orphans != 1 || len(rep.OrphanIDs) != 1 || rep.OrphanIDs[0] != "d" {
		t.Errorf("orphan detection wrong: %+v", rep.OrphanIDs)
	}
	foundB := false
	for _, ap := range rep.ArticulationPoints {
		if ap == "b" {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("b should be an articulation point of the chain: %v", rep.ArticulationPoints)
	}
	for _, ap := range rep.ArticulationPoints {
		if ap == "h" {
			t.Errorf("leaf-hub h must not be an articulation point: %v", rep.ArticulationPoints)
		}
	}
}

func TestAnalyze_EmptyGraph(t *testing.T) {
	rep := Analyze(nil, nil)
	if rep.NodeCount != 0 || rep.Orphans != 0 || len(rep.Hubs) != 0 {
		t.Fatalf("empty graph report wrong: %+v", rep)
	}
}

func TestAnalyze_DuplicateEdgesCountedOnceForRatio(t *testing.T) {
	a := []Node{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}
	edges := []Edge{
		{From: "a", To: "b", Relation: "related_to"},
		{From: "a", To: "b", Relation: "co_mentioned"}, // same pair, different relation
	}
	rep := Analyze(a, edges)
	// Ratio uses unique undirected pairs: 1 / 1 possible = 1.0.
	if rep.ConnectivityRatio != 1.0 {
		t.Errorf("ratio = %.2f, want 1.0 (duplicate pair counted once)", rep.ConnectivityRatio)
	}
}
