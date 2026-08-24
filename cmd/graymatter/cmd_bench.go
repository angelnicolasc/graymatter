package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/internal/bench"
	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// runBenchStore measures the caller's actual memory. Everything is read-only
// except under --probe-recall; see its flag help for why that one writes.
func runBenchStore(cmd *cobra.Command, onlyAgent string, probeRecall bool) error {
	out := cmd.OutOrStdout()

	store, err := openStore()
	if err != nil {
		return fmt.Errorf("open memory: %w", err)
	}
	defer func() { _ = store.Close() }()

	agents, err := store.ListAgents()
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	if onlyAgent != "" && !containsString(agents, onlyAgent) {
		return fmt.Errorf("no such agent %q (known: %s)", onlyAgent, strings.Join(agents, ", "))
	}

	rows := make([]storeAgentRow, 0, len(agents))
	for _, a := range agents {
		if onlyAgent != "" && a != onlyAgent {
			continue
		}
		facts, err := store.List(a)
		if err != nil {
			return fmt.Errorf("list facts for %q: %w", a, err)
		}
		rows = append(rows, measureStoreAgent(a, facts))
	}

	var probe *storeProbeResult
	if probeRecall {
		if probe, err = probeRecalls(store, rows); err != nil {
			return fmt.Errorf("probe recalls: %w", err)
		}
	}

	mode := "daemon"
	if _, direct := store.(*directStore); direct {
		mode = "in-process"
	}
	if jsonOut {
		return encodeStoreBenchJSON(out, dataDir, mode, rows, probe)
	}
	return renderStoreBench(out, dataDir, mode, rows, probe)
}

// storeProbeResult summarises --probe-recall over every measured agent.
type storeProbeResult struct {
	Queries   int     `json:"queries"`
	AvgTokens float64 `json:"avg_tokens"`
	MaxTokens int     `json:"max_tokens"`
}

const probeSamplesPerAgent = 20

// probeRecalls issues real recalls against sampled queries and reports what
// they actually cost. Queries are fact texts themselves: biased toward easy
// hits by design — this measures injection cost, not retrieval quality.
func probeRecalls(store cliStore, rows []storeAgentRow) (*storeProbeResult, error) {
	res := &storeProbeResult{}
	total := 0
	for _, r := range rows {
		facts, err := store.List(r.Agent)
		if err != nil {
			return nil, err
		}
		live := make([]string, 0, len(facts))
		for _, f := range facts {
			if f.SupersededBy == "" {
				live = append(live, f.Text)
			}
		}
		n := probeSamplesPerAgent
		if len(live) < n {
			n = len(live)
		}
		for i := 0; i < n; i++ {
			recalled, err := store.Recall(context.Background(), r.Agent, live[i], 8)
			if err != nil {
				continue // one dead sample must not sink the measurement
			}
			tk := tokens.Approx(strings.Join(recalled, "\n"))
			total += tk
			res.Queries++
			if tk > res.MaxTokens {
				res.MaxTokens = tk
			}
		}
	}
	if res.Queries > 0 {
		res.AvgTokens = float64(total) / float64(res.Queries)
	}
	return res, nil
}

func renderStoreBench(out io.Writer, dir, mode string, rows []storeAgentRow, probe *storeProbeResult) error {
	totalFacts := 0
	for _, r := range rows {
		totalFacts += r.LiveFacts
	}
	fmt.Fprintf(out, "\nStore: %s (%s)\n", dir, mode)
	fmt.Fprintf(out, "agents: %d · live facts: %d\n\n", len(rows), totalFacts)

	if len(rows) == 0 || totalFacts == 0 {
		fmt.Fprintln(out, "No live facts to measure yet. Store something first:")
		fmt.Fprintf(out, "  graymatter remember \"my-agent\" \"an observation worth keeping\"\n")
		return nil
	}

	fmt.Fprintf(out, "%-16s  %-12s  %-12s  %-14s  %s\n",
		"Agent", "Live", "Full dump", "Sliding-8", "Est. top-8")
	fmt.Fprintln(out, strings.Repeat("─", 74))
	for _, r := range rows {
		fmt.Fprintf(out, "%-16s  ~%-11d  ~%-11d  ~%-11d  ~%d\n",
			truncate(r.Agent, 16), r.LiveFacts, r.FullTokens, r.Sliding8Tokens, r.Top8WeightedTok)
	}
	fmt.Fprintln(out, strings.Repeat("─", 74))
	fmt.Fprintln(out, "Token figures are estimates (~1.33 per word). The sliding window is the")
	fmt.Fprintln(out, "strongest baseline GrayMatter replaces; the full dump is what unbounded")
	fmt.Fprintln(out, "history costs. Neither baseline checks relevance.")
	if probe != nil && probe.Queries > 0 {
		fmt.Fprintf(out, "\nProbe: %d real recalls · avg %.0f tokens · max %d.\n",
			probe.Queries, probe.AvgTokens, probe.MaxTokens)
		fmt.Fprintln(out, "Note: probes bumped access counters (recency bookkeeping).")
	} else if probe != nil {
		fmt.Fprintln(out, "\nProbe produced no samples (stores too small).")
	}
	return nil
}

func encodeStoreBenchJSON(out io.Writer, dir, mode string, rows []storeAgentRow, probe *storeProbeResult) error {
	payload := struct {
		Suite    string          `json:"suite"`
		DataDir  string          `json:"data_dir"`
		Mode     string          `json:"mode"`
		Agents   []storeAgentRow `json:"agents"`
		Probe    *storeProbeResult `json:"probe,omitempty"`
	}{Suite: "store", DataDir: dir, Mode: mode, Agents: rows, Probe: probe}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func benchCmd() *cobra.Command {
	var (
		onStore     bool
		onlyAgent   string
		probeRecall bool
	)
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run the published measurement suites",
		Long: `Run GrayMatter's published benchmarks and print what they measure today.

Two modes:

Default — the synthetic suite: deterministic, keyword embedder, fixed corpus,
no LLM, no network. The same numbers come out on every machine; these are the
suites that gate README.md and docs/benchmarks.md in CI.

--store — your memory: reads the actual store (through the daemon when one is
running) and reports what a recall would cost today against your own facts,
next to the baselines it replaces. Read-only except under --probe-recall,
which issues real recalls to measure them and says so.

What the token-count suite does NOT measure: relevance. A system returning
eight facts at random would score the same reduction. See docs/benchmarks.md
for the retrieval-quality suite and its results.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if onStore {
				return runBenchStore(cmd, onlyAgent, probeRecall)
			}
			return runBench(cmd)
		},
	}
	cmd.Flags().BoolVar(&onStore, "store", false,
		"measure the caller's own store instead of the synthetic suite")
	cmd.Flags().StringVar(&onlyAgent, "agent", "",
		"with --store: limit the measurement to this agent")
	cmd.Flags().BoolVar(&probeRecall, "probe-recall", false,
		"with --store: issue sample recalls (bumps access counters for recency bookkeeping)")
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

// --- --store mode -------------------------------------------------------------

// storeAgentRow is the per-agent measurement of the caller's own memory.
type storeAgentRow struct {
	Agent           string `json:"agent"`
	LiveFacts       int    `json:"live_facts"`
	FullTokens      int    `json:"full_dump_tokens"`
	Sliding8Tokens  int    `json:"sliding_8_tokens"`
	Top8WeightedTok int    `json:"estimated_top8_tokens"`
}

// measureStoreAgent reads one agent's live facts and computes, without a
// single Recall call: what dumping everything would cost, what the last-8
// sliding window costs, and what GrayMatter's top-8-by-weight injection is
// estimated to cost. Tombstones are excluded throughout — they never reach a
// prompt.
func measureStoreAgent(agent string, facts []memory.Fact) storeAgentRow {
	live := make([]string, 0, len(facts))
	for _, f := range facts {
		if f.SupersededBy == "" {
			live = append(live, f.Text)
		}
	}
	row := storeAgentRow{Agent: agent, LiveFacts: len(live)}
	if len(live) == 0 {
		return row
	}
	row.FullTokens = tokens.Approx(strings.Join(live, "\n"))

	window := live
	if len(window) > 8 { // facts arrive newest-first; the window keeps the 8 most recent
		window = window[:8]
	}
	row.Sliding8Tokens = tokens.Approx(strings.Join(window, "\n"))

	byWeight := make([]memory.Fact, len(facts))
	copy(byWeight, facts)
	sort.SliceStable(byWeight, func(i, j int) bool { return byWeight[i].Weight > byWeight[j].Weight })
	top := make([]string, 0, 8)
	for _, f := range byWeight {
		if f.SupersededBy != "" {
			continue
		}
		top = append(top, f.Text)
		if len(top) == 8 {
			break
		}
	}
	row.Top8WeightedTok = tokens.Approx(strings.Join(top, "\n"))
	return row
}
