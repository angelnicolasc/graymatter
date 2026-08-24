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
		format       string
		outDir       string
		agentID      string
		includeGraph bool
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export memories to human-readable files",
		Example: `  graymatter export --format obsidian --out ~/vault
  graymatter export --format obsidian --out ~/vault --include-graph
  graymatter export --format json
  graymatter export --format markdown --agent sales-closer`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if includeGraph && format != "obsidian" {
				return fmt.Errorf("--include-graph requires --format obsidian (the graph renders as entity notes + canvas)")
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

			agents := []string{agentID}
			if agentID == "" {
				agents, err = store.ListAgents()
				if err != nil {
					return err
				}
			}

			var facts []memory.Fact
			for _, aid := range agents {
				f, err := store.List(aid)
				if err != nil {
					return err
				}
				facts = append(facts, f...)
			}

			if err := exporter.Export(facts, outDir); err != nil {
				return err
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
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown, obsidian, json")
	cmd.Flags().StringVar(&outDir, "out", "", "output directory (default: .graymatter/export/<format>)")
	cmd.Flags().StringVar(&agentID, "agent", "", "export only this agent (default: all)")
	cmd.Flags().BoolVar(&includeGraph, "include-graph", false,
		"also write knowledge-graph entities and canvas (requires --format obsidian)")
	return cmd
}
