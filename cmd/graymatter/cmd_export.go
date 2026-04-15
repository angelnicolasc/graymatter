package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/export"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func exportCmd() *cobra.Command {
	var (
		format  string
		outDir  string
		agentID string
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export memories to human-readable files",
		Example: `  graymatter export --format obsidian --out ~/vault
  graymatter export --format json
  graymatter export --format markdown --agent sales-closer`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := graymatter.DefaultConfig()
			cfg.DataDir = dataDir

			mem, err := graymatter.NewWithConfig(cfg)
			if err != nil {
				return err
			}
			defer mem.Close()

			store := mem.Advanced()
			if store == nil {
				return fmt.Errorf("store not initialised")
			}

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

			from := "all agents"
			if agentID != "" {
				from = agentID
			}
			if !quiet {
				fmt.Printf("Exported %d facts for %s to %s (format: %s)\n", len(facts), from, outDir, format)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown, obsidian, json")
	cmd.Flags().StringVar(&outDir, "out", "", "output directory (default: .graymatter/export/<format>)")
	cmd.Flags().StringVar(&agentID, "agent", "", "export only this agent (default: all)")
	return cmd
}
