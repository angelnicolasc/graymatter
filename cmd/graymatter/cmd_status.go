package main

import (
	"encoding/json"
	"errors"
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
ledger, memory maintenance mode, review journal counts, and an injection-cost estimate.
It reads an existing store or daemon without opening a writer.

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

	store, mode, err := openStatusReader()
	if err != nil && !errors.Is(err, errDoctorNoDatabase) {
		return fmt.Errorf("open memory read-only: %w", err)
	}
	overview := &daemon.StoreOverviewResponse{
		MaintenanceMode: daemon.MaintenanceOnly,
		Agents:          []daemon.AgentSummary{},
		Review:          &memory.ReviewOverview{},
	}
	kgState := &daemon.KGStateResponse{}
	var tokSummary harness.TokenUsageSummary
	factsByAgent := make(map[string][]memory.Fact)
	if store != nil {
		defer func() { _ = store.Close() }()
		overview, err = store.StoreOverview()
		if err != nil {
			return fmt.Errorf("store overview: %w", err)
		}
		kgState, err = store.KGState()
		if err != nil {
			return fmt.Errorf("knowledge graph state: %w", err)
		}
		if tokSummary, err = store.TokenSummary(30); err != nil {
			tokSummary.Loaded = false
		}
		for _, a := range overview.Agents {
			facts, err := store.List(a.Agent)
			if err != nil {
				return fmt.Errorf("list facts for %q: %w", a.Agent, err)
			}
			factsByAgent[a.Agent] = facts
		}
	}
	view := statusView{Mode: mode, Overview: overview, KG: kgState, Tokens: tokSummary, Facts: factsByAgent}

	if jsonOut {
		pinned := 0
		for _, a := range overview.Agents {
			for _, f := range view.Facts[a.Agent] {
				if f.Pinned && !f.IsSuperseded() {
					pinned++
				}
			}
		}
		return encodeStatusJSON(out, mode, overview, kgState, tokSummary, pinned)
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
	if ov != nil {
		fmt.Fprintf(out, "MAINTENANCE %s\n", maintenanceModeLabel(ov.MaintenanceMode))
		fmt.Fprintf(out, "REVIEW     %s\n", reviewOverviewText(ov.Review))
	}

	if ov == nil || ov.TotalAgents == 0 {
		fmt.Fprintln(out, "STORE      empty. Get started:")
		fmt.Fprintln(out, "           graymatter init                     # wire your MCP clients")
		fmt.Fprintln(out, "           graymatter remember \"my-agent\" \"a fact\"")
		if kg != nil && (kg.Nodes != 0 || kg.Edges != 0 || kg.Provenance != nil) {
			writeStatusGraph(out, kg)
		}
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

	pinnedTotal := 0
	for _, a := range ov.Agents {
		for _, f := range view.Facts[a.Agent] {
			if f.Pinned && !f.IsSuperseded() {
				pinnedTotal++
			}
		}
	}

	fmt.Fprintf(out, "STORE      %d agents · %d live facts · %d superseded · %d pinned · avg weight %.2f\n",
		ov.TotalAgents, ov.TotalLiveFacts, ov.TotalTombstones, pinnedTotal, avgWeight)
	fmt.Fprintf(out, "           oldest fact %s · newest %s · pending vector ops: %d\n",
		relTime(oldest), relTime(newest), ov.PendingVectorOps)
	fmt.Fprintf(out, "           historical generated cycles %d · historical facts consumed %d\n",
		ov.Consolidations, ov.FactsConsumed)

	totalRecalls := 0
	for _, a := range ov.Agents {
		totalRecalls += a.Recalls
	}
	fmt.Fprintf(out, "RECALLS    %s · total %d\n", strings.Join(recallParts, " · "), totalRecalls)
	if totalRecalls == 0 {
		fmt.Fprintln(out, "           nothing recalled yet — restart your MCP client so the tools exist, and check the memory block in CLAUDE.md/AGENTS.md")
	}

	writeStatusGraph(out, kg)

	fmt.Fprintln(out, "TOKENS     last 30d — recorded by 'graymatter run' sessions only:")
	if tok.Loaded && tok.Requests > 0 {
		// Cache reads are part of the input side: the hit rate is
		// CacheRead / (Input + CacheRead), matching the harness ledger and
		// the TUI dashboard. Dividing by Input+Output printed >100% on
		// cache-heavy workloads.
		inputSide := tok.Input + tok.CacheRead
		fmt.Fprintf(out, "           in ~%dk · out ~%dk · cache-read %.0f%% · requests %d\n",
			tok.Input/1000, tok.Output/1000, pct(tok.CacheRead, inputSide), tok.Requests)
	} else {
		fmt.Fprintln(out, "           no harness runs recorded (MCP sessions are not measured)")
	}

	// The session-start hook injects the agent's top facts plus the shared
	// namespace's top conventions (the hooks' own budgets,
	// hookSessionStartAgentTopK / hookSessionStartSharedTopK), so estimate
	// that block rather than a generic top-8. __shared__ is never a hook
	// agent — no cwd maps to it — so it contributes the shared part of every
	// agent's estimate instead of a row of its own.
	sharedTk := estimateTopN(view.Facts[memory.SharedAgentID], hookSessionStartSharedTopK)
	minBlock, maxBlock, maxDump := 1<<30, 0, 0
	agentsCounted := 0
	for _, a := range ov.Agents {
		facts := view.Facts[a.Agent]
		live := make([]string, 0, len(facts))
		for _, f := range facts {
			if f.SupersededBy == "" {
				live = append(live, f.Text)
			}
		}
		if dump := tokens.Approx(strings.Join(live, "\n")); dump > maxDump {
			maxDump = dump
		}
		if a.Agent == memory.SharedAgentID {
			continue
		}
		tk := estimateTopN(facts, hookSessionStartAgentTopK) + sharedTk
		agentsCounted++
		if tk < minBlock {
			minBlock = tk
		}
		if tk > maxBlock {
			maxBlock = tk
		}
	}
	if agentsCounted == 0 && sharedTk > 0 {
		// A store holding only shared facts: every hook session injects the
		// shared block alone.
		minBlock, maxBlock, agentsCounted = sharedTk, sharedTk, 1
	}
	if ov.TotalLiveFacts > 0 && agentsCounted > 0 {
		fmt.Fprintf(out, "INJECTION  est. session-start block (top-%d agent + top-%d shared): ~%d–%d tk/agent vs full dump ~%d\n",
			hookSessionStartAgentTopK, hookSessionStartSharedTopK, minBlock, maxBlock, maxDump)
		fmt.Fprintf(out, "           estimates ~%.2f tok/word; the server cannot see your chat history.\n",
			tokens.PerWord)
	}
	return nil
}

func writeStatusGraph(out io.Writer, state *daemon.KGStateResponse) {
	if state == nil {
		state = &daemon.KGStateResponse{}
	}
	if state.AutoPopulate {
		fmt.Fprintln(out, "KG         auto-population ON")
	} else {
		fmt.Fprintln(out, "KG         auto-population OFF — enable: graymatter init --kg")
	}
	fmt.Fprintf(out, "           graph: %d nodes / %d edges\n", state.Nodes, state.Edges)
	if state.Provenance != nil {
		fmt.Fprintf(out, "           %s\n", graphProvenanceSummary(*state.Provenance))
	} else if state.Edges != 0 {
		fmt.Fprintln(out, "           provenance unavailable")
	}
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

func maintenanceModeLabel(mode string) string {
	if mode == daemon.MaintenanceOnly {
		return "maintenance only · generative compaction disabled"
	}
	return "unknown · update or restart the store daemon"
}

func encodeStatusJSON(out io.Writer, mode string, ov *daemon.StoreOverviewResponse, kg *daemon.KGStateResponse, tok harness.TokenUsageSummary, pinned int) error {
	summary := *ov
	if summary.MaintenanceMode == "" {
		summary.MaintenanceMode = "unknown"
	}
	payload := struct {
		Mode           string                        `json:"mode"`
		Store          *daemon.StoreOverviewResponse `json:"store"`
		KG             *daemon.KGStateResponse       `json:"kg"`
		Tokens30       *harness.TokenUsageSummary    `json:"tokens_30d"`
		Pinned         int                           `json:"pinned"`
		Consolidations int                           `json:"consolidations"`
		FactsConsumed  int                           `json:"facts_consumed"`
	}{Mode: mode, Store: &summary, KG: kg, Tokens30: &tok, Pinned: pinned,
		Consolidations: ov.Consolidations, FactsConsumed: ov.FactsConsumed}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
