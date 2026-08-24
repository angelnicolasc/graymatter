package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

// runDoctorGraph reports knowledge-graph analytics: hubs by degree, orphans,
// articulation points (Tarjan), and a declared connectivity ratio. Requires
// the store; every formula is printed with the numbers it computed from.
func runDoctorGraph(cmd *cobra.Command) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	nodes, err := store.KGNodes()
	if err != nil {
		return fmt.Errorf("list graph nodes: %w", err)
	}
	edges, err := store.KGEdges()
	if err != nil {
		return fmt.Errorf("list graph edges: %w", err)
	}

	rep := kg.Analyze(nodes, edges)
	rep.NodeCount = len(nodes)
	rep.EdgeCount = len(edges)

	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Knowledge-graph analytics\n\n")
	fmt.Printf("  nodes: %d Â· edges: %d Â· orphans: %d Â· connectivity ratio: %.3f\n",
		rep.NodeCount, rep.EdgeCount, rep.Orphans, rep.ConnectivityRatio)
	fmt.Println("  formulas: degree = undirected edges per node Â· ratio = unique pairs / NÂ·(Nâˆ’1)/2")
	fmt.Println()

	if len(rep.Hubs) > 0 {
		fmt.Println("  hubs (top degree):")
		for i, h := range rep.Hubs {
			fmt.Printf("    %d. %-28s degree %d\n", i+1, h.Label, h.Degree)
		}
		fmt.Println()
	}
	if len(rep.ArticulationPoints) > 0 {
		fmt.Printf("  articulation points (%d): removing any of these splits the graph\n", len(rep.ArticulationPoints))
		for _, ap := range rep.ArticulationPoints {
			fmt.Printf("    - %s\n", ap)
		}
		fmt.Println()
	}
	if len(rep.OrphanIDs) > 0 {
		fmt.Printf("  orphan entities (%d): no connections at all\n", rep.Orphans)
	}
	return nil
}
