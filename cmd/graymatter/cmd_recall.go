package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/lineageview"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func recallCmd() *cobra.Command {
	var topK int
	var shared bool
	var all bool
	var explain bool
	var extra []string
	var minimum string
	var weight float64

	cmd := &cobra.Command{
		Use:   "recall <agent-id> <query>",
		Short: "Retrieve relevant memories for an agent",
		Long: `Retrieve the most relevant memories for an agent, ranked by hybrid
recall (semantic + keyword + recency).

With --explain, each returned fact carries its receipt: the per-signal
ranks that produced its fused score, its stored weight and age, and its
provenance (fact ID, written-at instant, tombstone state). The same
ranking runs either way — explain only reads it out. Results affected by an
open conflict include its ID and source state.

--min-confidence filters before ranking. --confidence-weight applies the
bounded preference base RRF * (1 + weight*c), where c is 1 for verified,
0 for inferred and -1 for unverified. The opt-in default is zero; confidence
is a writer declaration, not automated verification.`,
		Example: `  graymatter recall "sales-closer" "follow up Maria"
  graymatter recall "code-reviewer" "nil pointer" --top-k 5
  graymatter recall --shared "global preferences"
  graymatter recall --all "sales-closer" "Maria follow up"
  graymatter recall "sales-closer" "Maria" --explain --json
  graymatter recall "backend" --query "TLS floor" --query "who owns billing" --query "release cadence"
  graymatter recall "backend" "TLS floor" --min-confidence verified --confidence-weight 0.1 --explain --json`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := cliRecallOptions(cmd, minimum, weight)
			if err != nil {
				return err
			}
			if explain && (shared || all || len(extra) > 0) {
				return fmt.Errorf("--explain supports a single agent-scoped query only")
			}
			if shared && all {
				return fmt.Errorf("--shared and --all are mutually exclusive")
			}
			settings := recallSettings{options: opts, shared: shared, all: all}
			// One call, many questions. Every extra recall an agent has to
			// issue is a round trip through its model, so a caller holding
			// several open questions pays in turns rather than in store time.
			// --query gathers them into one invocation, answered concurrently.
			if len(extra) > 0 {
				queries := append([]string{}, extra...)
				if len(args) == 2 {
					queries = append([]string{args[1]}, queries...)
				}
				return runRecallBatch(cmd, args[0], queries, topK, settings)
			}
			if shared && len(args) == 1 {
				args = []string{memory.SharedAgentID, args[0]}
			}
			if len(args) != 2 {
				return fmt.Errorf("a query is required (or pass --query one or more times)")
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
				return runRecallExplain(cmd, store, agentID, query, topK, opts)
			}

			var facts []string
			var scope string
			// Only the agent-scoped path carries the weak-match block: --shared
			// and --all merge namespaces, and a vocabulary hint drawn from a
			// merge would point at terms from a store the caller did not ask
			// about.
			var feedback string
			result, err := cliRecall(ctx, store, agentID, query, topK, settings)
			facts, feedback = result.Facts, result.Feedback
			scope = agentID
			if all {
				scope = "all"
			} else if shared {
				scope = "shared"
			}
			if err != nil {
				return err
			}

			issues, err := readSubjectIssues(ctx, store, recallSubjectNamespaces(agentID, settings)...)
			if err != nil {
				return fmt.Errorf("subject diagnostics: %w", err)
			}
			if jsonOut {
				out := map[string]any{
					"agent_id": agentID,
					"scope":    scope,
					"query":    query,
					"facts":    facts,
					"count":    len(facts),
					"feedback": feedback,
				}
				if result.Retrieval != nil {
					out["retrieval"] = result.Retrieval
				}
				if len(result.ReviewNotices) > 0 {
					out["conflict_notices"] = result.ReviewNotices
				}
				if issues != nil {
					out["subject_issues"] = issues
				}
				data, _ := json.Marshal(out)
				fmt.Println(string(data))
				return nil
			}
			if result.Retrieval != nil {
				fmt.Println(result.Retrieval.Text())
			}

			if len(facts) == 0 {
				if !quiet {
					fmt.Printf("No memories found for agent %q matching %q.\n", agentID, query)
				}
				if feedback != "" {
					fmt.Printf("\n%s\n", feedback)
				}
				printSubjectWarning(issues)
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
			if notice := memory.ReviewNoticesText(result.ReviewNotices); notice != "" {
				fmt.Printf("\n%s\n", notice)
			}
			printSubjectWarning(issues)
			return nil
		},
	}
	cmd.Flags().IntVar(&topK, "top-k", 0, "maximum facts to return (default from config)")
	cmd.Flags().BoolVar(&shared, "shared", false, "recall from shared memory only")
	cmd.Flags().BoolVar(&all, "all", false, "recall from both agent and shared memory, merged")
	cmd.Flags().BoolVar(&explain, "explain", false, "return one receipt per fact: per-signal ranks, fused score, weight, age, provenance")
	cmd.Flags().StringArrayVar(&extra, "query", nil, "an extra query to answer in the same call; repeat for several, answered concurrently")
	cmd.Flags().StringVar(&minimum, "min-confidence", "", "minimum effective confidence: unverified, inferred or verified; suppresses graph hints")
	cmd.Flags().Float64Var(&weight, "confidence-weight", memory.DefaultConfidenceWeight, "confidence preference in [0,0.5]; zero preserves an explicit filter")
	return cmd
}

func recallSubjectNamespaces(agentID string, settings recallSettings) []string {
	if settings.shared {
		return []string{memory.SharedAgentID}
	}
	if settings.all && agentID != memory.SharedAgentID {
		return []string{agentID, memory.SharedAgentID}
	}
	return []string{agentID}
}

func printSubjectWarning(issues *memory.SubjectIssues) {
	if warning := issues.Warning(); warning != "" {
		fmt.Println(warning)
	}
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
func runRecallBatch(cmd *cobra.Command, agentID string, queries []string, topK int, settingsArg ...recallSettings) error {
	var settings recallSettings
	if len(settingsArg) > 0 {
		settings = settingsArg[0]
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	opts, useOptions, err := cliBatchRecallOptions(store, settings.options)
	if err != nil {
		return err
	}
	// Fully omitted zero policy uses the compatibility endpoints, keeping one
	// zero policy even if the daemon restarts with a different default mid-batch.
	confidenceMetadata := useOptions
	if journal, ok := store.(interface{ ReviewJournalSupported() bool }); ok && journal.ReviewJournalSupported() && !useOptions {
		zero := 0.0
		opts.ConfidenceWeight = &zero
		useOptions = true
	}
	settings.options, settings.legacy = opts, !useOptions

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	type row struct {
		Query     string                    `json:"query"`
		Facts     []string                  `json:"facts"`
		Error     string                    `json:"error,omitempty"`
		Retrieval *memory.RetrievalMetadata `json:"retrieval,omitempty"`
		Notices   []memory.ReviewNotice     `json:"-"`
	}
	batch, batchError := cliRecallBatch(ctx, store, agentID, queries, topK, settings)
	if batchError != nil && len(batch) == 0 {
		batch = make([]memory.BatchResult, len(queries))
		for i, query := range queries {
			batch[i] = memory.BatchResult{Query: query, Err: batchError}
		}
	}
	rows := make([]row, len(batch))
	for i, result := range batch {
		rows[i].Query = result.Query
		if result.Err != nil {
			rows[i].Error = result.Err.Error()
			batch[i].Facts = nil
		} else {
			rows[i].Facts, rows[i].Notices = result.Facts, result.ReviewNotices
			if confidenceMetadata {
				rows[i].Retrieval = result.Retrieval
			}
		}
	}
	merged := memory.MergedFacts(batch)
	var retrieval *memory.RetrievalMetadata
	var failures []string
	var notices []memory.ReviewNotice
	for _, r := range rows {
		notices = memory.MergeReviewNotices(notices, r.Notices)
		if r.Retrieval != nil && (retrieval == nil || retrieval.VectorFallback == "") {
			retrieval = r.Retrieval
		}
		if r.Error != "" {
			failures = append(failures, r.Error)
		}
	}
	if len(rows) > 0 && len(failures) == len(rows) {
		batchError = fmt.Errorf("all recall queries failed: %s", strings.Join(failures, "; "))
	}
	scope := agentID
	if settings.all {
		scope = "all"
	} else if settings.shared {
		scope = "shared"
	}
	issues, err := readSubjectIssues(ctx, store, recallSubjectNamespaces(agentID, settings)...)
	if err != nil {
		return fmt.Errorf("subject diagnostics: %w", err)
	}

	if jsonOut {
		out := map[string]any{
			"agent_id":  agentID,
			"queries":   len(queries),
			"count":     len(merged),
			"merged":    merged,
			"per_query": rows,
		}
		if settings.all || settings.shared {
			out["scope"] = scope
		}
		if retrieval != nil {
			out["retrieval"] = retrieval
		}
		if len(notices) > 0 {
			out["conflict_notices"] = notices
		}
		if issues != nil {
			out["subject_issues"] = issues
		}
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
		return batchError
	}

	if len(merged) == 0 {
		if !quiet && len(failures) == 0 {
			switch {
			case settings.shared:
				fmt.Printf("No memories found in shared memory matching any of the %d queries.\n", len(queries))
			case settings.all:
				fmt.Printf("No memories found for agent %q or shared memory matching any of the %d queries.\n", agentID, len(queries))
			default:
				fmt.Printf("No memories found for agent %q matching any of the %d queries.\n", agentID, len(queries))
			}
		}
		if retrieval != nil {
			fmt.Println(retrieval.Text())
		}
		if len(failures) > 0 {
			fmt.Printf("%d queries failed: %s\n", len(failures), strings.Join(failures, "; "))
		}
		printSubjectWarning(issues)
		return batchError
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
		fmt.Printf("# Memory context [%s] / %d queries, %d distinct facts\n", scope, len(queries), len(merged))
	}
	for _, r := range rows {
		fmt.Printf("\n## %s\n", r.Query)
		if r.Retrieval != nil {
			fmt.Println(r.Retrieval.Text())
		}
		switch {
		case r.Error != "":
			fmt.Printf("! failed: %s\n", r.Error)
		case len(r.Facts) == 0:
			fmt.Println("(nothing found)")
		default:
			fmt.Println(strings.Join(r.Facts, "\n"))
		}
	}
	if notice := memory.ReviewNoticesText(notices); notice != "" {
		fmt.Printf("\n%s\n", notice)
	}
	printSubjectWarning(issues)
	return batchError
}

func cliRecallBatch(ctx context.Context, store cliStore, agentID string, queries []string, topK int, settings recallSettings) ([]memory.BatchResult, error) {
	if recaller, ok := store.(memory.ScopedBatchRecaller); ok {
		namespace := agentID
		if settings.shared {
			namespace = memory.SharedAgentID
		}
		var options *memory.RecallOptions
		if !settings.legacy {
			options = &settings.options
		}
		rows, err := recaller.BatchRecallScoped(ctx, namespace, queries, topK, settings.all, options)
		if !errors.Is(err, memory.ErrBatchRecallUnsupported) {
			return rows, err
		}
	}

	rows := make([]memory.BatchResult, len(queries))
	limit := min(runtime.GOMAXPROCS(0), len(queries))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, query := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result, err := cliRecall(ctx, store, agentID, query, topK, settings)
			rows[i] = memory.BatchResult{Query: query, Facts: result.Facts, Retrieval: result.Retrieval, ReviewNotices: result.ReviewNotices, Err: err}
		}()
	}
	wg.Wait()
	return rows, nil
}

// runRecallExplain is the --explain path. The receipt payload is the stable
// contract documented in docs/api-stability.md ("Added in v0.17.0"); the
// human-readable rendering is a convenience on top of it.
func runRecallExplain(cmd *cobra.Command, store cliStore, agentID, query string, topK int, options ...memory.RecallOptions) error {
	var opts memory.RecallOptions
	if len(options) > 0 {
		opts = options[0]
	}
	var receipts []memory.RecallReceipt
	var retrieval *memory.RetrievalMetadata
	var err error
	if useCLIConfidenceRecall(store, opts) {
		backend, ok := store.(rpc.ConfidenceRecaller)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		result, callErr := backend.RecallExplainWithOptions(cmd.Context(), agentID, query, topK, opts)
		receipts, retrieval, err = result.Receipts, result.Retrieval, callErr
	} else {
		receipts, err = store.RecallExplain(cmd.Context(), agentID, query, topK)
	}
	if err != nil {
		return err
	}

	issues, err := readSubjectIssues(cmd.Context(), store, agentID)
	if err != nil {
		return fmt.Errorf("subject diagnostics: %w", err)
	}
	if jsonOut {
		out := map[string]any{
			"agent_id": agentID,
			"scope":    agentID,
			"query":    query,
			"count":    len(receipts),
			"facts":    receipts,
		}
		if retrieval != nil {
			out["retrieval"] = retrieval
		}
		if issues != nil {
			out["subject_issues"] = issues
		}
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
		return nil
	}
	if retrieval != nil {
		fmt.Println(retrieval.Text())
	}

	if len(receipts) == 0 {
		if !quiet {
			fmt.Printf("No memories found for agent %q matching %q.\n", agentID, query)
		}
		printSubjectWarning(issues)
		return nil
	}

	if !quiet {
		fmt.Printf("# Memory receipts [%s] / %q\n\n", agentID, query)
	}
	var notices []memory.ReviewNotice
	for i, r := range receipts {
		notices = memory.MergeReviewNotices(notices, r.ReviewNotices)
		fmt.Printf("%d. %s\n", i+1, r.Text)
		fmt.Printf("   score %.4f (vector %d · keyword %d · recency %d · k %.0f) · weight %.3f · age %.1fd · written %s\n",
			r.Ranks.FusedScore, r.Ranks.VectorRank, r.Ranks.KeywordRank, r.Ranks.RecencyRank,
			r.Ranks.K, r.Weight, r.AgeDays, r.Provenance.WrittenAt.Format("2006-01-02"))
		fmt.Printf("   fact_id %s\n", r.Provenance.FactID)
		if r.Ranking != nil {
			fmt.Printf("   final score %.8f = base %.8f * factor %g; effective confidence %s, weight %g, policy %s\n", r.Ranking.FinalScore, r.Ranking.BaseScore, r.Ranking.Factor, r.Ranking.EffectiveConfidence, r.Ranking.ConfidenceWeight, r.Ranking.Policy)
		}
		for _, line := range lineageview.Lines(r.Provenance.LineageProjection) {
			fmt.Printf("   %s\n", line)
		}
		if len(r.Provenance.Supersedes) > 0 {
			fmt.Printf("   Supersedes IDs (compatibility field) · %s\n", strings.Join(r.Provenance.Supersedes, ", "))
		}
		if len(r.KGLinks) > 0 {
			fmt.Printf("   kg_links %s\n", strings.Join(r.KGLinks, ", "))
		}
	}
	if notice := memory.ReviewNoticesText(notices); notice != "" {
		fmt.Printf("\n%s\n", notice)
	}
	printSubjectWarning(issues)
	return nil
}
