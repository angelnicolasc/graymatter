package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

// runDoctorGraph reports knowledge-graph analytics: hubs by degree, orphans,
// articulation points (Tarjan), and a declared connectivity ratio. Requires
// the store; every formula is printed with the numbers it computed from.
//
// With --html it also writes the full self-contained visual render
// (`kg render`'s HTML page) next to the text report, so a doctor run doubles
// as a shareable graph artifact.
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

	htmlPath, _ := cmd.Flags().GetString("html")
	if htmlPath != "" {
		if len(nodes) == 0 {
			return fmt.Errorf("--html: the knowledge graph is empty; enable auto-population with `graymatter init --kg`")
		}
		f, err := os.Create(htmlPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", htmlPath, err)
		}
		if err := renderKGHTML(f, nodes, edges); err != nil {
			_ = f.Close()
			return fmt.Errorf("render %s: %w", htmlPath, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", htmlPath, err)
		}
	}

	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Knowledge-graph analytics\n\n")
	fmt.Printf("  nodes: %d · edges: %d · orphans: %d · connectivity ratio: %.3f\n",
		rep.NodeCount, rep.EdgeCount, rep.Orphans, rep.ConnectivityRatio)
	fmt.Println("  formulas: degree = undirected edges per node · ratio = unique pairs / N·(N−1)/2")
	if htmlPath != "" {
		fmt.Printf("  visual render written to %s (self-contained HTML, works offline)\n", htmlPath)
	}
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
