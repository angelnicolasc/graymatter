package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/export"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func exportCmd() *cobra.Command {
	var (
		format         string
		outDir         string
		agentID        string
		includeGraph   bool
		includeLineage bool
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export memories to human-readable files",
		Example: `  graymatter export --format obsidian --out ~/vault
  graymatter export --format obsidian --out ~/vault --include-graph
  graymatter export --format json
  graymatter export --format json --include-lineage
  graymatter export --format markdown --agent sales-closer`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if includeGraph && format != "obsidian" {
				return fmt.Errorf("--include-graph requires --format obsidian (the graph renders as entity notes + canvas)")
			}
			if includeLineage && format != "json" {
				return fmt.Errorf("--include-lineage requires --format json")
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			exporter, err := export.New(export.Format(format))
			if err != nil {
				return err
			}

			if outDir == "" {
				outDir = filepath.Join(dataDir, "export", format)
			}

			if includeLineage {
				lineage, ok := store.(lineageExporter)
				if !ok {
					return fmt.Errorf("complete lineage export is unavailable on this store")
				}
				archive, err := lineage.ExportLineage(cmd.Context(), agentID)
				if err != nil {
					return fmt.Errorf("collect lineage archive: %w", err)
				}
				if err := export.WriteLineageArchive(archive, outDir); err != nil {
					return fmt.Errorf("write lineage archive: %w", err)
				}
				if warning := archive.SubjectIssues.Warning(); warning != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), warning)
				}
				if !quiet {
					fmt.Printf("Exported %d fact rows and %d evidence versions to %s\n",
						len(archive.Facts), len(archive.Evidence), filepath.Join(outDir, "memory-lineage.json"))
				}
				return nil
			}

			agents := []string{agentID}
			if agentID == "" {
				agents, err = store.ListAgents()
				if err != nil {
					return err
				}
			}

			var facts []memory.Fact
			var issues memory.SubjectIssues
			for _, aid := range agents {
				f, err := store.List(aid)
				if err != nil {
					return err
				}
				report, err := readSubjectIssues(cmd.Context(), store, aid)
				if err != nil {
					return fmt.Errorf("subject diagnostics: %w", err)
				}
				if report != nil {
					issues.Total += report.Total
					issues.Subjects = append(issues.Subjects, report.Subjects[:min(len(report.Subjects), 3-len(issues.Subjects))]...)
					// A fact projection of an affected namespace presents
					// live facts. Historical tombstones remain in the
					// explicit lineage archive.
					live := f[:0]
					for _, fact := range f {
						if !fact.IsSuperseded() {
							live = append(live, fact)
						}
					}
					f = live
				}
				facts = append(facts, f...)
			}

			if err := exporter.Export(facts, outDir); err != nil {
				return err
			}
			if warning := issues.Warning(); warning != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), warning)
			}

			graphNote := ""
			if includeGraph {
				if err := store.ExportGraphObsidian(outDir); err != nil {
					return fmt.Errorf("export knowledge graph: %w", err)
				}
				if err := linkFactNotesToEntities(outDir, facts); err != nil {
					return fmt.Errorf("link facts to entities: %w", err)
				}
				graphNote = " + knowledge graph (entities, canvas, fact links)"
			}

			from := "all agents"
			if agentID != "" {
				from = agentID
			}
			if !quiet {
				fmt.Printf("Exported %d facts for %s to %s (format: %s)%s\n", len(facts), from, outDir, format, graphNote)
				fmt.Println("Lineage scope · fact projections. Use --format json --include-lineage for retained evidence.")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown, obsidian, json (fact projections unless --include-lineage is set)")
	cmd.Flags().StringVar(&outDir, "out", "", "output directory (default: .graymatter/export/<format>)")
	cmd.Flags().StringVar(&agentID, "agent", "", "export only this agent (default: all)")
	cmd.Flags().BoolVar(&includeGraph, "include-graph", false,
		"also write knowledge-graph entities and canvas (requires --format obsidian)")
	cmd.Flags().BoolVar(&includeLineage, "include-lineage", false,
		"write a versioned audit archive with retained snapshots and relationships (requires --format json)")
	return cmd
}
