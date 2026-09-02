package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func recallCmd() *cobra.Command {
	var topK int
	var shared bool
	var all bool
	var explain bool
	var extra []string

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
  graymatter recall "sales-closer" "Maria" --explain --json
  graymatter recall "backend" --query "TLS floor" --query "who owns billing" --query "release cadence"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// One call, many questions. Every extra recall an agent has to
			// issue is a round trip through its model, so a caller holding
			// several open questions pays in turns rather than in store time.
			// --query gathers them into one invocation, answered concurrently.
			if len(extra) > 0 {
				queries := append([]string{}, extra...)
				if len(args) == 2 {
					queries = append([]string{args[1]}, queries...)
				}
				return runRecallBatch(cmd, args[0], queries, topK)
			}
			if len(args) != 2 {
				return fmt.Errorf("a query is required (or pass --query one or more times)")
			}
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
			// Only the agent-scoped path carries the weak-match block: --shared
			// and --all merge namespaces, and a vocabulary hint drawn from a
			// merge would point at terms from a store the caller did not ask
			// about.
			var feedback string
			switch {
			case all:
				facts, err = store.RecallAll(ctx, agentID, query, topK)
				scope = "all"
			case shared:
				facts, err = store.RecallShared(ctx, query, topK)
				scope = "shared"
			default:
				// Identical facts in identical order to Recall, plus the block.
				facts, feedback, err = store.RecallDetailed(ctx, agentID, query, topK)
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
					"feedback": feedback,
				})
				fmt.Println(string(data))
				return nil
			}

			if len(facts) == 0 {
				if !quiet {
					fmt.Printf("No memories found for agent %q matching %q.\n", agentID, query)
				}
				if feedback != "" {
					fmt.Printf("\n%s\n", feedback)
				}
				return nil
			}

			if !quiet {
				fmt.Printf("# Memory context [%s] / %q\n\n", scope, query)
			}
			fmt.Println(strings.Join(facts, "\n"))
			// The block is where a caller learns its words missed. Printing the
			// facts and swallowing it leaves the caller guessing at exactly the
			// moment guessing is the expensive thing.
			if feedback != "" {
				fmt.Printf("\n%s\n", feedback)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&topK, "top-k", 0, "maximum facts to return (default from config)")
	cmd.Flags().BoolVar(&shared, "shared", false, "recall from shared memory only")
	cmd.Flags().BoolVar(&all, "all", false, "recall from both agent and shared memory, merged")
	cmd.Flags().BoolVar(&explain, "explain", false, "return one receipt per fact: per-signal ranks, fused score, weight, age, provenance")
	cmd.Flags().StringArrayVar(&extra, "query", nil, "an extra query to answer in the same call; repeat for several, answered concurrently")
	return cmd
}

// runRecallBatch answers several queries in one invocation.
//
// The saving is conversational, not computational. A recall takes about a
// tenth of a second; the expensive part of asking six questions one at a time
// is the six model round trips around them. Ranking per query is byte-identical
// to running the same query alone, so this is an interface change and not a
// second retrieval path.
//
// The merged block is what a caller reads: one deduplicated, best-first list,
// so a fact three questions share costs one slot in the context rather than
// three. --json additionally carries the per-query breakdown.
func runRecallBatch(cmd *cobra.Command, agentID string, queries []string, topK int) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	type row struct {
		Query string   `json:"query"`
		Facts []string `json:"facts"`
		Error string   `json:"error,omitempty"`
	}
	rows := make([]row, len(queries))
	limit := runtime.GOMAXPROCS(0)
	if limit > len(queries) {
		limit = len(queries)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i].Query = q
			facts, err := store.Recall(ctx, agentID, q, topK)
			if err != nil {
				// One bad query must not lose the answers to the others.
				rows[i].Error = err.Error()
				return
			}
			rows[i].Facts = facts
		}(i, q)
	}
	wg.Wait()

	batch := make([]memory.BatchResult, 0, len(rows))
	for _, r := range rows {
		batch = append(batch, memory.BatchResult{Query: r.Query, Facts: r.Facts})
	}
	merged := memory.MergedFacts(batch)

	if jsonOut {
		data, _ := json.Marshal(map[string]any{
			"agent_id":  agentID,
			"queries":   len(queries),
			"count":     len(merged),
			"merged":    merged,
			"per_query": rows,
		})
		fmt.Println(string(data))
		return nil
	}

	if len(merged) == 0 {
		if !quiet {
			fmt.Printf("No memories found for agent %q matching any of the %d queries.\n", agentID, len(queries))
		}
		return nil
	}
	// Grouped by query, not as one merged block.
	//
	// The merged block was the first shape this printed, and it measurably lost
	// answers: a caller batching thirty-five questions got two hundred facts in
	// one undifferentiated list and could no longer tell which fact answered
	// which question. Accuracy fell from 34/35 to 27/35 on the same store. The
	// dedup is still worth having — it is reported, and `--json` carries the
	// merged array — but the association between a question and its answer is
	// the thing the caller cannot reconstruct, so it is what the default
	// rendering preserves.
	if !quiet {
		fmt.Printf("# Memory context [%s] / %d queries, %d distinct facts\n", agentID, len(queries), len(merged))
	}
	for _, r := range rows {
		fmt.Printf("\n## %s\n", r.Query)
		switch {
		case r.Error != "":
			fmt.Printf("! failed: %s\n", r.Error)
		case len(r.Facts) == 0:
			fmt.Println("(nothing found)")
		default:
			fmt.Println(strings.Join(r.Facts, "\n"))
		}
	}
	return nil
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
		// A corrected value is worth more than the value alone: it says the
		// store held something else and that this replaced it. Without the
		// line, a revision is indistinguishable from a fact nobody ever
		// questioned.
		if n := len(r.Provenance.Supersedes); n > 0 {
			noun := "version"
			if n > 1 {
				noun = "versions"
			}
			fmt.Printf("   supersedes %d earlier %s: %s\n", n, noun,
				strings.Join(r.Provenance.Supersedes, ", "))
		}
		if len(r.KGLinks) > 0 {
			fmt.Printf("   kg_links %s\n", strings.Join(r.KGLinks, ", "))
		}
	}
	return nil
}
