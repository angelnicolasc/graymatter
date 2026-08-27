package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// consolidateCmd runs one consolidation cycle for an agent. It is a normal
// CLI command (usable directly) and the entry point the session-end hook
// spawns detached: consolidation may proxy an LLM round-trip, which is why it
// runs outside the hook's time budget rather than inside it.
func consolidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "consolidate <agent-id>",
		Short: "Run one memory consolidation cycle for an agent",
		Long: `Consolidates the agent's memories: summarisation (when an LLM is
configured — Anthropic key or Ollama), decay application, and pruning, with
superseded originals kept as tombstone receipts.

Runs through the store daemon, so it applies the daemon's consolidation
policy and is safe alongside the TUI, MCP servers, and hooks. With no LLM
configured it applies decay and pruning only.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			if err := store.Consolidate(context.Background(), agentID); err != nil {
				return err
			}
			if !quiet {
				fmt.Printf("Consolidation complete for %q.\n", agentID)
			}
			return nil
		},
	}
}
