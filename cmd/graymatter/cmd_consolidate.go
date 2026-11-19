package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// consolidateCmd runs one maintenance cycle for an agent. The session-end
// hook runs the same command detached to keep maintenance out of its budget.
func consolidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "consolidate <agent-id>",
		Short: "Run memory maintenance for an agent",
		Long: `Applies decay and pruning, and extracts knowledge-graph entities when
auto-population is enabled. Generative compaction is disabled, including
when Anthropic or Ollama is configured.

Uses the store owner's maintenance policy. The daemon coordinates access
with the TUI, MCP servers, and hooks.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			if err := store.Consolidate(cmd.Context(), agentID); err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Maintenance complete for %q. Generative compaction is disabled.\n", agentID)
			}
			return nil
		},
	}
}
