package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

func graphProvenanceSummary(report kg.ProvenanceReport) string {
	summary := fmt.Sprintf("provenance %d complete, %d incomplete, %d pending",
		report.Complete, report.Incomplete, report.Pending)
	if len(report.Reasons) != 0 {
		reasons := make([]string, 0, len(report.Reasons))
		for _, reason := range slices.Sorted(maps.Keys(report.Reasons)) {
			reasons = append(reasons, fmt.Sprintf("%s=%d", reason, report.Reasons[reason]))
		}
		summary += " (" + strings.Join(reasons, ", ") + ")"
	}
	if report.CleanupPending > 0 {
		summary += fmt.Sprintf(" · %d cleanup events pending", report.CleanupPending)
	}
	if report.MigrationVersion == 0 {
		return summary + " · inventory migration has not started"
	}
	state := "in progress"
	if report.MigrationComplete {
		state = "scan complete"
	}
	return fmt.Sprintf("%s · inventory v%d %s, %d edges scanned",
		summary, report.MigrationVersion, state, report.MigrationExamined)
}

// runDoctorGraph reports knowledge-graph analytics: hubs by degree, orphans,
// articulation points (Tarjan), and a declared connectivity ratio. Requires
// the store; every formula is printed with the numbers it computed from.
//
// With --html it also writes the full self-contained visual render
// (`kg render`'s HTML page) next to the text report, so a doctor run doubles
// as a shareable graph artifact.
func runDoctorGraph(cmd *cobra.Command) error {
	graph, err := inspectGraph(dataDir)
	if err != nil {
		return fmt.Errorf("inspect graph: %w", err)
	}
	nodes := graph.Nodes
	edges := make([]kg.Edge, 0, len(graph.Edges))
	for _, item := range graph.Edges {
		edges = append(edges, item.Edge)
	}
	rep := kg.Analyze(nodes, edges)

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
		return enc.Encode(struct {
			kg.GraphReport
			Provenance *kg.ProvenanceReport `json:"provenance,omitempty"`
		}{GraphReport: rep, Provenance: graph.Provenance})
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Knowledge-graph analytics\n\n")
	fmt.Fprintf(out, "  nodes: %d · edges: %d · orphans: %d · connectivity ratio: %.3f\n",
		rep.NodeCount, rep.EdgeCount, rep.Orphans, rep.ConnectivityRatio)
	fmt.Fprintln(out, "  formulas: degree = undirected edges per node · ratio = unique pairs / N·(N−1)/2")
	if graph.Provenance != nil {
		fmt.Fprintf(out, "  %s\n", graphProvenanceSummary(*graph.Provenance))
	} else if rep.EdgeCount != 0 {
		fmt.Fprintln(out, "  provenance unavailable")
	}
	if htmlPath != "" {
		fmt.Fprintf(out, "  visual render written to %s (self-contained HTML, works offline)\n", htmlPath)
	}
	fmt.Fprintln(out)

	if len(rep.Hubs) > 0 {
		fmt.Fprintln(out, "  hubs (top degree):")
		for i, h := range rep.Hubs {
			fmt.Fprintf(out, "    %d. %-28s degree %d\n", i+1, h.Label, h.Degree)
		}
		fmt.Fprintln(out)
	}
	if len(rep.ArticulationPoints) > 0 {
		fmt.Fprintf(out, "  articulation points (%d): removing any of these splits the graph\n", len(rep.ArticulationPoints))
		for _, ap := range rep.ArticulationPoints {
			fmt.Fprintf(out, "    - %s\n", ap)
		}
		fmt.Fprintln(out)
	}
	if len(rep.OrphanIDs) > 0 {
		fmt.Fprintf(out, "  orphan entities (%d): no connections at all\n", rep.Orphans)
	}
	return nil
}
