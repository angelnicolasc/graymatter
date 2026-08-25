package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// doctor --health audits the store itself with deterministic rules — what
// `bench` does for published numbers, pointed at the user's own memory.
// Every rule reads only store contents, including its own timestamps, and
// never the wall clock, so the same store produces byte-identical output on
// every run. That determinism is the contract; a health check whose verdict
// drifts between runs trains people to ignore it.

const (
	dumpBurstWindow    = time.Hour // write burst width for the dumping rule
	dumpBurstMinWrites = 8         // writes inside the window to qualify as a burst
	dumpBurstMaxAvgLen = 50        // average rune length below which a burst counts as thin
	nearPruneWeight    = 0.05      // weight under which a live fact is near prune
	supersedeWarnRatio = 0.40      // tombstones / total above this warns
	supersedeFailRatio = 0.70      // ... above this fails
	duplicateWarnRatio = 0.10      // duplicated members / live above this warns
	duplicateFailRatio = 0.25      // ... above this fails
	maxListedItems     = 10        // cap on listed offenders per finding; counts stay complete
)

var criticalKeywords = []string{"must", "always", "never", "decision", "policy"}

type healthFinding struct {
	Rule   string   `json:"rule"`
	Status string   `json:"status"` // ok | info | warn | fail
	Detail string   `json:"detail"`
	Items  []string `json:"items,omitempty"`
}

type healthAgentReport struct {
	Agent      string          `json:"agent"`
	LiveFacts  int             `json:"live_facts"`
	Tombstones int             `json:"tombstones"`
	Findings   []healthFinding `json:"findings"`
}

type healthReport struct {
	Agents  []healthAgentReport `json:"agents"`
	Verdict string              `json:"verdict"`
}

func runDoctorHealth(cmd *cobra.Command) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	overview, err := store.StoreOverview()
	if err != nil {
		return fmt.Errorf("store overview: %w", err)
	}

	report := healthReport{Verdict: "ok"}
	for _, a := range overview.Agents {
		facts, ferr := store.List(a.Agent)
		if ferr != nil {
			return fmt.Errorf("list facts for %q: %w", a.Agent, ferr)
		}
		rep := auditAgentHealth(a.Agent, facts)
		for _, f := range rep.Findings {
			if severity(f.Status) > severity(report.Verdict) {
				report.Verdict = f.Status
			}
		}
		report.Agents = append(report.Agents, rep)
	}
	sort.Slice(report.Agents, func(i, j int) bool { return report.Agents[i].Agent < report.Agents[j].Agent })

	out := cmd.OutOrStdout()
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderHealth(out, report)
	return nil
}

func severity(s string) int {
	switch s {
	case "ok", "":
		return 0
	case "info":
		return 1
	case "warn":
		return 2
	default:
		return 3
	}
}

// auditAgentHealth runs every rule over one agent's facts. Findings are
// emitted in fixed rule order and item lists sorted, so the report depends
// only on the store's bytes.
func auditAgentHealth(agent string, facts []memory.Fact) healthAgentReport {
	live := make([]memory.Fact, 0, len(facts))
	tomb := 0
	for _, f := range facts {
		if f.IsSuperseded() {
			tomb++
		} else {
			live = append(live, f)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })

	rep := healthAgentReport{Agent: agent, LiveFacts: len(live), Tombstones: tomb}
	rep.Findings = append(rep.Findings,
		ruleSupersedeLoop(tomb, len(live)),
		ruleDumping(live),
		ruleNearPruneCritical(live),
		ruleDuplicates(live),
	)
	return rep
}

// ruleSupersedeLoop flags a correction rate so high it means the agent keeps
// rewriting its own knowledge — usually a prompt problem or two agents
// fighting over one namespace. Recent-only detection would need the wall
// clock, breaking the stable-output contract; the lifetime ratio catches the
// chronic loops, which are the actionable kind.
func ruleSupersedeLoop(tomb, live int) healthFinding {
	f := healthFinding{Rule: "supersede-loop", Status: "ok"}
	total := tomb + live
	if total == 0 {
		f.Detail = "no facts"
		return f
	}
	ratio := float64(tomb) / float64(total)
	switch {
	case ratio >= supersedeFailRatio:
		f.Status = "fail"
		f.Detail = fmt.Sprintf("%.0f%% of the store is tombstones (%d of %d) — a correction loop, not memory", ratio*100, tomb, total)
	case ratio >= supersedeWarnRatio:
		f.Status = "warn"
		f.Detail = fmt.Sprintf("%.0f%% tombstones (%d of %d) — check whether updates contradict themselves repeatedly", ratio*100, tomb, total)
	default:
		f.Detail = fmt.Sprintf("%d tombstone(s) of %d fact(s)", tomb, total)
	}
	return f
}

type dumpBurst struct {
	count int
	start time.Time
	end   time.Time
	sum   int // summed rune lengths across the window's facts
}

// ruleDumping catches the dumping-ground anti-pattern at rest: bursts of many
// thin facts written back-to-back. A burst is ≥ dumpBurstMinWrites live facts
// inside dumpBurstWindow whose average text length stays under
// dumpBurstMaxAvgLen runes. Windows are built from CreatedAt values stored in
// the facts themselves, never from now().
func ruleDumping(live []memory.Fact) healthFinding {
	f := healthFinding{Rule: "dumping", Status: "ok", Detail: "no thin write bursts"}
	if len(live) < dumpBurstMinWrites {
		return f
	}

	type write struct {
		at  time.Time
		len int
	}
	ws := make([]write, 0, len(live))
	for _, x := range live {
		ws = append(ws, write{at: x.CreatedAt, len: len([]rune(x.Text))})
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].at.Before(ws[j].at) })

	var worst *dumpBurst
	for i := 0; i < len(ws); i++ {
		b := &dumpBurst{start: ws[i].at, count: 1, sum: ws[i].len}
		for j := i + 1; j < len(ws); j++ {
			if ws[j].at.Sub(b.start) > dumpBurstWindow {
				break
			}
			b.count++
			b.sum += ws[j].len
			b.end = ws[j].at
		}
		if b.count < dumpBurstMinWrites || b.avgLen() >= dumpBurstMaxAvgLen {
			continue
		}
		if worst == nil || b.count > worst.count {
			worst = b
		}
		i += b.count - 1 // non-overlapping windows: skip past this burst
	}
	if worst == nil {
		return f
	}

	f.Status = "warn"
	f.Detail = fmt.Sprintf("burst of %d thin facts %s → %s (avg %d chars)",
		worst.count,
		worst.start.UTC().Format(time.RFC3339),
		worst.end.UTC().Format(time.RFC3339),
		worst.avgLen())
	return f
}

func (b *dumpBurst) avgLen() int {
	if b.count == 0 {
		return 0
	}
	return b.sum / b.count
}

// ruleNearPruneCritical connects W5 to W1 (ADR-010): a live fact about to be
// collected whose wording says it was meant to be permanent. It never fails —
// decay doing its job is correct behaviour — but each hit is worth a pin or
// an explicit supersede.
func ruleNearPruneCritical(live []memory.Fact) healthFinding {
	f := healthFinding{Rule: "near-prune-critical", Status: "ok", Detail: "no critical-looking facts near prune"}
	var hits []string
	for _, x := range live {
		if x.Weight >= nearPruneWeight {
			continue
		}
		text := strings.ToLower(x.Text)
		for _, kw := range criticalKeywords {
			if strings.Contains(text, kw) {
				hits = append(hits, fmt.Sprintf("%s weight=%.3f %q", x.ID, x.Weight, truncateItem(x.Text)))
				break
			}
		}
	}
	sort.Strings(hits)
	if len(hits) == 0 {
		return f
	}
	f.Status = "warn"
	f.Detail = fmt.Sprintf("%d fact(s) look permanent but are near prune (weight < %.2f) — consider pinning them", len(hits), nearPruneWeight)
	f.Items = hits
	if len(f.Items) > maxListedItems {
		f.Items = f.Items[:maxListedItems]
	}
	return f
}

var punctuationRe = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
var spacesRe = regexp.MustCompile(`\s+`)

// ruleDuplicates: near-exact duplicates waste injection budget and make
// corrections ambiguous. Normalisation folds case, punctuation and spacing;
// groups of ≥2 identical normalised texts among live facts count.
func ruleDuplicates(live []memory.Fact) healthFinding {
	groups := map[string][]string{}
	for _, x := range live {
		key := spacesRe.ReplaceAllString(punctuationRe.ReplaceAllString(strings.ToLower(x.Text), ""), " ")
		groups[key] = append(groups[key], x.ID)
	}
	var dupMembers int
	var items []string
	for key, ids := range groups {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		dupMembers += len(ids)
		items = append(items, fmt.Sprintf("%dx %q [%s]", len(ids), truncateItem(key), strings.Join(ids, ", ")))
	}
	sort.Strings(items)

	f := healthFinding{Rule: "duplicates"}
	if len(items) == 0 {
		f.Status = "ok"
		f.Detail = "no duplicates"
		return f
	}
	ratio := float64(dupMembers) / float64(len(live))
	switch {
	case ratio >= duplicateFailRatio:
		f.Status = "fail"
	case ratio >= duplicateWarnRatio:
		f.Status = "warn"
	default:
		f.Status = "info"
	}
	f.Detail = fmt.Sprintf("%d fact(s) in %d duplicate group(s) (%.0f%% of live)", dupMembers, len(items), ratio*100)
	f.Items = items
	if len(f.Items) > maxListedItems {
		f.Items = f.Items[:maxListedItems]
	}
	return f
}

func truncateItem(s string) string {
	runes := []rune(s)
	if len(runes) <= 80 {
		return s
	}
	return string(runes[:77]) + "..."
}

func renderHealth(out io.Writer, report healthReport) {
	fmt.Fprintf(out, "\nStore health · verdict %s\n", strings.ToUpper(report.Verdict))
	if len(report.Agents) == 0 {
		fmt.Fprintln(out, "  empty store — nothing to audit")
		return
	}
	marks := map[string]string{"ok": " ok ", "info": "info", "warn": "WARN", "fail": "FAIL"}
	for _, a := range report.Agents {
		fmt.Fprintf(out, "\n%s — %d live / %d superseded\n", a.Agent, a.LiveFacts, a.Tombstones)
		for _, fd := range a.Findings {
			fmt.Fprintf(out, "  [%s] %-20s %s\n", marks[fd.Status], fd.Rule, fd.Detail)
			for _, it := range fd.Items {
				fmt.Fprintf(out, "         - %s\n", it)
			}
		}
	}
	fmt.Fprintln(out)
}
