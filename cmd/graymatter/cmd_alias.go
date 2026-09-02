package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/spf13/cobra"
)

// aliasCmd is the CLI half of the alias vocabulary. The store surface, the RPC, the daemon and
// the MCP tool all carry aliases; without this an operator at a terminal could
// read the "record it with alias" line the weak-match block prints and have
// nowhere to record it. A suggestion that names an action the caller cannot
// take is worse than no suggestion.
func aliasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "alias <agent-id> <term> <equivalent> [equivalent...]",
		// Aliases carry the action the feedback block suggests: the weak-match
		// block says "record it with memory_alias" — an MCP tool name — and the
		// uninstructed-agent arm
		// measured what happens when that name does not resolve where the
		// caller stands — 98 calls, 0 aliases, a store that learns nothing.
		// From the CLI the block's instruction must resolve to a real
		// command, so the CLI accepts that name as an alias of
		// this one — and it reads the same constant the block formats, so
		// the two cannot drift apart by editing one of them.
		Aliases: []string{memory.FeedbackAction},
		Short:   "Teach the store that two vocabularies mean the same thing",
		Long: `Record that a term and one or more phrases are equivalent, so a search
using either side reaches the facts the other side matches.

An alias is a fact of kind "alias": it is never injected into a result, never
counted in the ranking, and never scored — it only expands the query. It is
revisable like any other fact, and a superseded alias stops expanding, so a
wrong one is corrected rather than lived with.

Single-token pairs expand both ways; a multi-word phrase expands only from the
term to the phrase, because splitting a phrase back into its words would drag
in every fact that happens to use one of them.`,
		Example: `  graymatter alias "backend" "message broker" "event bus"
  graymatter alias "clinic" "HIE" "health information exchange"`,
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID, term, equivalents := args[0], args[1], args[2:]
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := store.PutAlias(ctx, agentID, term, equivalents); err != nil {
				return fmt.Errorf("store alias: %w", err)
			}

			if jsonOut {
				data, _ := json.Marshal(map[string]any{
					"agent_id":    agentID,
					"term":        term,
					"equivalents": equivalents,
					"stored":      true,
				})
				fmt.Println(string(data))
				return nil
			}
			if !quiet {
				fmt.Printf("Alias stored: [%s] %q = %s\n", agentID, term, strings.Join(equivalents, ", "))
			}
			return nil
		},
	}
	cmd.AddCommand(aliasListCmd())
	return cmd
}
