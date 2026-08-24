package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/internal/bench"
	"github.com/angelnicolasc/graymatter/internal/tokens"
)

func benchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run the published measurement suites",
		Long: `Run GrayMatter's published benchmarks and print what they measure today.

The synthetic suite is deterministic — keyword embedder, fixed corpus, no LLM,
no network — so the same numbers come out on every machine. These are the same
suites that gate README.md and docs/benchmarks.md in CI; running them here
audits the published claims without cloning the repository or having Go.

What the token-count suite does NOT measure: relevance. A system returning
eight facts at random would score the same reduction. See docs/benchmarks.md
for the retrieval-quality suite and its results.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBench(cmd)
		},
	}
	return cmd
}

func runBench(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	start := time.Now()
	results, err := bench.RunTokenCount()
	if err != nil {
		return fmt.Errorf("token-count suite: %w", err)
	}
	elapsed := time.Since(start)

	if jsonOut {
		type row struct {
			Sessions     int     `json:"sessions"`
			FullTokens   int     `json:"full_tokens"`
			RecallTokens int     `json:"recall_tokens"`
			ReductionPct float64 `json:"reduction_pct"`
		}
		payload := struct {
			Suite         string  `json:"suite"`
			Query         string  `json:"query"`
			TopK          int     `json:"top_k"`
			TokensPerWord float64 `json:"tokens_per_word"`
			DurationMS    int64   `json:"duration_ms"`
			Results       []row   `json:"results"`
		}{Suite: "token-count", Query: bench.TokenQuery, TopK: bench.TokenTopK,
			TokensPerWord: tokens.PerWord, DurationMS: elapsed.Milliseconds()}
		for _, r := range results {
			payload.Results = append(payload.Results, row{
				Sessions: r.Sessions, FullTokens: r.FullTokens,
				RecallTokens: r.RecallTokens, ReductionPct: r.Reduction,
			})
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	fmt.Fprint(out, bench.RenderTokenReport(results, elapsed))
	return nil
}
