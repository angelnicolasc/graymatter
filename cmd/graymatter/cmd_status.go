package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "One-screen answer to: is my memory actually being used?",
		Long: `Print what GrayMatter knows, what it has been doing, and what a recall
costs today — facts, recalls per agent, knowledge-graph state, the token
ledger, and an injection-cost estimate against your own store.

The token ledger covers 'graymatter run' sessions only; MCP sessions run
entirely inside your editor and are never measured anywhere. Injection
figures are estimates (~1.33 tokens/word), because an MCP server cannot see
your chat history to compare against.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd)
		},
	}
}

func runStatus(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	store, err := openStore()
	if err != nil {
		return fmt.Errorf("open memory: %w", err)
	}
	defer func() { _ = store.Close() }()

	overview, err := store.StoreOverview()
	if err != nil {
		return fmt.Errorf("store overview: %w", err)
	}
	kgState, err := store.KGState()
	if err != nil {
		return fmt.Errorf("knowledge graph state: %w", err)
	}
	tokSummary, tokErr := store.TokenSummary(30)
	if tokErr != nil {
		tokSummary.Loaded = false // degrade loudly rather than half-printing
	}

	mode := "via daemon"
	if _, direct := store.(*directStore); direct {
		mode = "in-process"
	}

	view := statusView{Mode: mode, Overview: overview, KG: kgState, Tokens: tokSummary, Facts: map[string][]memory.Fact{}}
	for _, a := range overview.Agents {
		facts, ferr := store.List(a.Agent)
		if ferr != nil {
			return fmt.Errorf("list facts for %q: %w", a.Agent, ferr)
		}
		view.Facts[a.Agent] = facts
	}

	if jsonOut {
		return encodeStatusJSON(out, mode, overview, kgState, tokSummary)
	}
	return renderStatus(out, view)
}

// statusFacts carries the per-agent fact lists the injection estimate needs.
// Fetched once in runStatus so rendering stays a pure function of its inputs.
type statusView struct {
	Mode     string
	Overview *daemon.StoreOverviewResponse
	KG       *daemon.KGStateResponse
	Tokens   harness.TokenUsageSummary
	Facts    map[string][]memory.Fact // agent -> all facts (live + tombstones)
}

func renderStatus(out io.Writer, view statusView) error {
	ov, kg, tok := view.Overview, view.KG, view.Tokens
	mode := view.Mode
	fmt.Fprintf(out, "\nGrayMatter status · %s\n\n", mode)

	if ov == nil || ov.TotalAgents == 0 {
		fmt.Fprintln(out, "STORE      empty. Get started:")
		fmt.Fprintln(out, "           graymatter init                     # wire your MCP clients")
		fmt.Fprintln(out, "           graymatter remember \"my-agent\" \"a fact\"")
		return nil
	}

	newest, oldest := time.Time{}, time.Time{}
	totalWeighted := 0.0
	recallParts := make([]string, 0, len(ov.Agents))
	for _, a := range ov.Agents {
		totalWeighted += a.AvgWeight * float64(a.LiveFacts)
		recallParts = append(recallParts, fmt.Sprintf("%s %d", a.Agent, a.Recalls))
		if a.NewestAt.After(newest) {
			newest = a.NewestAt
		}
		if !a.OldestAt.IsZero() && (oldest.IsZero() || a.OldestAt.Before(oldest)) {
			oldest = a.OldestAt
		}
	}

	avgWeight := 0.0
	if ov.TotalLiveFacts > 0 {
		avgWeight = totalWeighted / float64(ov.TotalLiveFacts)
	}

	fmt.Fprintf(out, "STORE      %d agents · %d live facts · %d superseded · avg weight %.2f\n",
		ov.TotalAgents, ov.TotalLiveFacts, ov.TotalTombstones, avgWeight)
	fmt.Fprintf(out, "           oldest fact %s · newest %s · pending vector ops: %d\n",
		relTime(oldest), relTime(newest), ov.PendingVectorOps)

	totalRecalls := 0
	for _, a := range ov.Agents {
		totalRecalls += a.Recalls
	}
	fmt.Fprintf(out, "RECALLS    %s · total %d\n", strings.Join(recallParts, " · "), totalRecalls)
	if totalRecalls == 0 {
		fmt.Fprintln(out, "           nothing recalled yet — restart your MCP client so the tools exist, and check the memory block in CLAUDE.md/AGENTS.md")
	}

	state, hint := "OFF", ""
	if kg != nil && kg.AutoPopulate {
		state = "ON"
	} else {
		hint = " — enable: graymatter init --kg"
	}
	nodes, edges := 0, 0
	if kg != nil {
		nodes, edges = kg.Nodes, kg.Edges
	}
	fmt.Fprintf(out, "KG         auto-population %s%s\n", state, hint)
	fmt.Fprintf(out, "           graph: %d nodes / %d edges\n", nodes, edges)

	fmt.Fprintln(out, "TOKENS     last 30d — recorded by 'graymatter run' sessions only:")
	if tok.Loaded && tok.Requests > 0 {
		inOut := tok.Input + tok.Output
		fmt.Fprintf(out, "           in ~%dk · out ~%dk · cache-read %.0f%% · requests %d\n",
			tok.Input/1000, tok.Output/1000, pct(tok.CacheRead, inOut), tok.Requests)
	} else {
		fmt.Fprintln(out, "           no harness runs recorded (MCP sessions are not measured)")
	}

	minTop8, maxTop8, maxDump := 1<<30, 0, 0
	for _, a := range ov.Agents {
		facts := view.Facts[a.Agent]
		tk := estimateTop8Tokens(facts)
		if tk < minTop8 {
			minTop8 = tk
		}
		if tk > maxTop8 {
			maxTop8 = tk
		}
		live := make([]string, 0, len(facts))
		for _, f := range facts {
			if f.SupersededBy == "" {
				live = append(live, f.Text)
			}
		}
		if dump := tokens.Approx(strings.Join(live, "\n")); dump > maxDump {
			maxDump = dump
		}
	}
	if ov.TotalLiveFacts > 0 {
		fmt.Fprintf(out, "INJECTION  est. top-8 recall cost now: ~%d–%d tk/agent vs full dump ~%d\n",
			minTop8, maxTop8, maxDump)
		fmt.Fprintf(out, "           estimates ~%.2f tok/word; the server cannot see your chat history.\n",
			tokens.PerWord)
	}
	return nil
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func pct(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

func encodeStatusJSON(out io.Writer, mode string, ov *daemon.StoreOverviewResponse, kg *daemon.KGStateResponse, tok harness.TokenUsageSummary) error {
	payload := struct {
		Mode     string                        `json:"mode"`
		Store    *daemon.StoreOverviewResponse `json:"store"`
		KG       *daemon.KGStateResponse       `json:"kg"`
		Tokens30 *harness.TokenUsageSummary    `json:"tokens_30d"`
	}{Mode: mode, Store: ov, KG: kg, Tokens30: &tok}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
