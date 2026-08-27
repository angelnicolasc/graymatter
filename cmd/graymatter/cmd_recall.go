package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func recallCmd() *cobra.Command {
	var topK int
	var shared bool
	var all bool
	var explain bool

	cmd := &cobra.Command{
		Use:   "recall <agent-id> <query>",
		Short: "Retrieve relevant memories for an agent",
		Long: `Retrieve the most relevant memories for an agent, ranked by hybrid
recall (semantic + keyword + recency).

With --explain, each returned fact carries its receipt: the per-signal
ranks that produced its fused score, its stored weight and age, and its
provenance (fact ID, written-at instant, tombstone state). The same
ranking runs either way — explain only reads it out.`,
		Example: `  graymatter recall "sales-closer" "follow up Maria"
  graymatter recall "code-reviewer" "nil pointer" --top-k 5
  graymatter recall --shared "global preferences"
  graymatter recall --all "sales-closer" "Maria follow up"
  graymatter recall "sales-closer" "Maria" --explain --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if explain && (shared || all) {
				return fmt.Errorf("--explain supports agent-scoped recall only (--shared/--all merge namespaces whose merged entries have no single receipt)")
			}
			agentID, query := args[0], args[1]
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			// topK <= 0 means "store default" on every path.
			if explain {
				return runRecallExplain(cmd, store, agentID, query, topK)
			}

			var facts []string
			var scope string
			switch {
			case all:
				facts, err = store.RecallAll(ctx, agentID, query, topK)
				scope = "all"
			case shared:
				facts, err = store.RecallShared(ctx, query, topK)
				scope = "shared"
			default:
				facts, err = store.Recall(ctx, agentID, query, topK)
				scope = agentID
			}
			if err != nil {
				return err
			}

			if jsonOut {
				data, _ := json.Marshal(map[string]any{
					"agent_id": agentID,
					"scope":    scope,
					"query":    query,
					"facts":    facts,
					"count":    len(facts),
				})
				fmt.Println(string(data))
				return nil
			}

			if len(facts) == 0 {
				if !quiet {
					fmt.Printf("No memories found for agent %q matching %q.\n", agentID, query)
				}
				return nil
			}

			if !quiet {
				fmt.Printf("# Memory context [%s] / %q\n\n", scope, query)
			}
			fmt.Println(strings.Join(facts, "\n"))
			return nil
		},
	}
	cmd.Flags().IntVar(&topK, "top-k", 0, "maximum facts to return (default from config)")
	cmd.Flags().BoolVar(&shared, "shared", false, "recall from shared memory only")
	cmd.Flags().BoolVar(&all, "all", false, "recall from both agent and shared memory, merged")
	cmd.Flags().BoolVar(&explain, "explain", false, "return one receipt per fact: per-signal ranks, fused score, weight, age, provenance")
	return cmd
}

// runRecallExplain is the --explain path. The receipt payload is the stable
// contract documented in docs/api-stability.md ("Added in v0.17.0"); the
// human-readable rendering is a convenience on top of it.
func runRecallExplain(cmd *cobra.Command, store cliStore, agentID, query string, topK int) error {
	receipts, err := store.RecallExplain(context.Background(), agentID, query, topK)
	if err != nil {
		return err
	}

	if jsonOut {
		data, _ := json.Marshal(map[string]any{
			"agent_id": agentID,
			"scope":    agentID,
			"query":    query,
			"count":    len(receipts),
			"facts":    receipts,
		})
		fmt.Println(string(data))
		return nil
	}

	if len(receipts) == 0 {
		if !quiet {
			fmt.Printf("No memories found for agent %q matching %q.\n", agentID, query)
		}
		return nil
	}

	if !quiet {
		fmt.Printf("# Memory receipts [%s] / %q\n\n", agentID, query)
	}
	for i, r := range receipts {
		fmt.Printf("%d. %s\n", i+1, r.Text)
		fmt.Printf("   score %.4f (vector %d · keyword %d · recency %d · k %.0f) · weight %.3f · age %.1fd · written %s\n",
			r.Ranks.FusedScore, r.Ranks.VectorRank, r.Ranks.KeywordRank, r.Ranks.RecencyRank,
			r.Ranks.K, r.Weight, r.AgeDays, r.Provenance.WrittenAt.Format("2006-01-02"))
		fmt.Printf("   fact_id %s\n", r.Provenance.FactID)
		if len(r.KGLinks) > 0 {
			fmt.Printf("   kg_links %s\n", strings.Join(r.KGLinks, ", "))
		}
	}
	return nil
}
