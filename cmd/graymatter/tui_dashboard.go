package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
)

// ── Dashboard model ──────────────────────────────────────────────────────────
//
// dashboardData holds pre-computed aggregates for the Stats tab. It is
// recomputed in full on every load (cheap for typical fact counts <10k) and
// never mutated while being rendered.

type agentAgg struct {
	ID          string
	Facts       int
	Recalls     int     // Σ AccessCount across the agent's facts
	AvgWeight   float64 // mean weight, used for secondary sort
	StorageB    int64   // Σ len(Text) + len(Embedding)*4
	NewestAt    time.Time
	LastAccess  time.Time
}

type dashboardData struct {
	Loaded bool

	// Global rollups.
	AgentsN    int
	FactsN     int
	RecallsN   int
	StorageB   int64   // bytes
	HealthPct  float64 // fraction of facts with Weight > 0.5, in [0,1]
	AvgWeight  float64 // corpus-wide average weight
	OldestAt   time.Time
	NewestAt   time.Time

	// Per-agent, sorted by fact count desc.
	Agents []agentAgg

	// Weight bucket counts: >0.8, 0.5–0.8, 0.2–0.5, <0.2
	WeightBuckets [4]int

	// Last-30-day facts-created histogram, oldest → newest.
	DailyCreated [30]int
	DailyStart   time.Time // date of DailyCreated[0]

	// Token-cost ledger for the last 30 days (harness runs only). Loaded
	// lazily from the `token_usage` bbolt bucket; Tokens.Loaded is false
	// when the bucket is empty — in which case the panel renders an
	// empty-state card instead of fabricating numbers.
	Tokens harness.TokenUsageSummary
}

type dashboardLoadedMsg struct{ data dashboardData }
type dashboardTickMsg struct{}

// dashboardTick fires every 5s while the TUI is open; cheap enough to run
// always so that switching to the Stats tab always shows fresh data. The
// loader returns a message that the main Update handler re-dispatches.
func dashboardTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return dashboardTickMsg{}
	})
}

// loadDashboard builds a dashboardData snapshot from the store. It iterates
// all agents → all facts, O(N) over total facts. Embedding size is inferred
// from the embedding slice length × 4 (float32).
func (m tuiModel) loadDashboard() tea.Cmd {
	store := m.store
	return func() tea.Msg {
		d := dashboardData{Loaded: true}
		if store == nil {
			return dashboardLoadedMsg{d}
		}

		agents, err := store.ListAgents()
		if err != nil {
			return dashboardLoadedMsg{d}
		}
		d.AgentsN = len(agents)

		// Build a 30-day window ending today (UTC day boundaries).
		today := time.Now().UTC().Truncate(24 * time.Hour)
		d.DailyStart = today.AddDate(0, 0, -29)

		var weightSum float64
		var weightCount int
		var healthyN int

		for _, a := range agents {
			facts, err := store.List(a)
			if err != nil || len(facts) == 0 {
				continue
			}
			agg := agentAgg{ID: a, Facts: len(facts)}

			var agentWSum float64
			for _, f := range facts {
				// Storage: text bytes + embedding bytes (float32 = 4 B).
				agg.StorageB += int64(len(f.Text) + len(f.Embedding)*4)

				// Recalls proxy: sum of per-fact access counts.
				agg.Recalls += f.AccessCount

				// Weight corpus stats.
				weightSum += f.Weight
				weightCount++
				agentWSum += f.Weight

				if f.Weight > 0.5 {
					healthyN++
				}

				// Weight buckets.
				switch {
				case f.Weight > 0.8:
					d.WeightBuckets[0]++
				case f.Weight > 0.5:
					d.WeightBuckets[1]++
				case f.Weight > 0.2:
					d.WeightBuckets[2]++
				default:
					d.WeightBuckets[3]++
				}

				if f.CreatedAt.After(agg.NewestAt) {
					agg.NewestAt = f.CreatedAt
				}
				if f.AccessedAt.After(agg.LastAccess) {
					agg.LastAccess = f.AccessedAt
				}
				if d.OldestAt.IsZero() || f.CreatedAt.Before(d.OldestAt) {
					d.OldestAt = f.CreatedAt
				}
				if f.CreatedAt.After(d.NewestAt) {
					d.NewestAt = f.CreatedAt
				}

				// Daily histogram bucket.
				day := f.CreatedAt.UTC().Truncate(24 * time.Hour)
				idx := int(day.Sub(d.DailyStart).Hours() / 24)
				if idx >= 0 && idx < len(d.DailyCreated) {
					d.DailyCreated[idx]++
				}
			}
			if agg.Facts > 0 {
				agg.AvgWeight = agentWSum / float64(agg.Facts)
			}

			d.FactsN += agg.Facts
			d.RecallsN += agg.Recalls
			d.StorageB += agg.StorageB
			d.Agents = append(d.Agents, agg)
		}

		if weightCount > 0 {
			d.AvgWeight = weightSum / float64(weightCount)
			d.HealthPct = float64(healthyN) / float64(weightCount)
		}

		sort.SliceStable(d.Agents, func(i, j int) bool {
			if d.Agents[i].Facts != d.Agents[j].Facts {
				return d.Agents[i].Facts > d.Agents[j].Facts
			}
			return d.Agents[i].ID < d.Agents[j].ID
		})

		// Token usage is a cheap bucket scan (≤ agents × models × days rows).
		// Never block the dashboard on it — if the bucket is missing or the
		// query fails, the panel degrades to an empty-state card.
		if ts, err := store.TokenSummary(30); err == nil {
			d.Tokens = ts
		}

		return dashboardLoadedMsg{d}
	}
}

// ── Rendering ────────────────────────────────────────────────────────────────

// renderDashboard lays out the Stats tab as a multi-panel dashboard filling
// the available width/height. Falls back to an empty-state message when the
// store has no facts.
func (m tuiModel) renderDashboard(h int) string {
	w := m.width - 2
	if w < 40 {
		w = 40
	}
	if h < 10 {
		h = 10
	}

	d := m.dashboard

	if !d.Loaded {
		return stylePanel.Width(w).Height(h).Render(
			styleDimText.Render("  Loading observability data…"),
		)
	}
	if d.FactsN == 0 {
		empty := lipgloss.JoinVertical(lipgloss.Left,
			styleSubText.Render(""),
			styleSubText.Render("  No memories stored yet."),
			styleSubText.Render("  Run `graymatter remember` or let an agent write first."),
		)
		return panelBox("GrayMatter · Observability", w, empty)
	}

	// Row 1 — KPI strip.
	kpiRow := m.renderKPIRow(d, w)

	// Row 2 — Agents panel (left) | [Token Cost (top) + Weight Distribution
	// (bottom)] (right). Token Cost gets the hero slot in the right column:
	// it's the metric a new reader cares about most (is this expensive?) and
	// the cache-hit rate inside it is the virality hook.
	//
	// Width math (for symmetry with the full-width Activity panel below):
	//   each panelBox renders as `width + 2` cols (border).
	//   row2 outer = (leftW+2) + 1 (gutter) + (rightW+2) = leftW + rightW + 5
	//   activity outer = w + 2
	//   ⇒ leftW + rightW = w − 3 to make borders line up perfectly.
	leftW := w * 3 / 5
	rightW := w - leftW - 3
	if rightW < 26 {
		rightW = 26
		leftW = w - rightW - 3
	}
	agents := m.renderAgentsPanel(d, leftW)
	tokens := m.renderTokenPanel(d, rightW)

	// Height alignment: render the natural-height panels first, then pad
	// Weight Distribution downward so the right column matches the Agents
	// panel exactly (single grid baseline at the bottom). lipgloss.Height
	// counts border rows, so the arithmetic is direct.
	agentsH := lipgloss.Height(agents)
	tokensH := lipgloss.Height(tokens)
	weightH := agentsH - tokensH
	if weightH < 6 {
		weightH = 6 // sanity floor: at least title + 1 row + footer + borders
	}
	weights := m.renderWeightPanel(d, rightW, weightH)
	rightCol := lipgloss.JoinVertical(lipgloss.Left, tokens, weights)
	row2 := lipgloss.JoinHorizontal(lipgloss.Top, agents, " ", rightCol)

	// Row 3 — Activity sparkline panel (full width).
	activity := m.renderActivityPanel(d, w)

	body := lipgloss.JoinVertical(lipgloss.Left, kpiRow, "", row2, "", activity)
	return body
}

// renderKPIRow renders the 4-tile CodeBurn-style KPI strip.
//
// Width math (matches Activity panel border for vertical alignment):
//   each kpiBlock renders as `tileW + 2` cols (border).
//   row outer = 4*(tileW+2) + 3 gutters = 4*tileW + 11
//   activity outer = width + 2
//   ⇒ tileW = (width − 9) / 4 so all four borders share a column with
//     the borders of the Agents/Activity panels below.
func (m tuiModel) renderKPIRow(d dashboardData, width int) string {
	tileW := (width - 9) / 4
	if tileW < 14 {
		tileW = 14
	}

	healthColor := colorMint
	switch {
	case d.HealthPct < 0.4:
		healthColor = colorRose
	case d.HealthPct < 0.7:
		healthColor = colorAmber
	}

	facts := kpiBlock("FACTS",
		formatCompact(d.FactsN),
		fmt.Sprintf("across %d agents", d.AgentsN),
		colorCyan, tileW)

	cost := kpiBlock("MEMORY COST",
		formatBytes(d.StorageB),
		"text + embeddings",
		colorPurple, tileW)

	recalls := kpiBlock("RECALLS",
		formatCompact(d.RecallsN),
		"Σ access count",
		colorAmber, tileW)

	health := kpiBlock("HEALTH",
		fmt.Sprintf("%.0f%%", d.HealthPct*100),
		"facts · weight > 0.5",
		healthColor, tileW)

	return lipgloss.JoinHorizontal(lipgloss.Top, facts, " ", cost, " ", recalls, " ", health)
}

// renderAgentsPanel renders the "Inventory vs Activity" panel: per agent,
// two stacked bars — fact count (cyan) and recall count (amber). Both series
// share a single global scale so cross-agent and cross-series comparison is
// visually honest. Values are right-aligned past the bar, and a faint dotted
// rule separates consecutive agents.
func (m tuiModel) renderAgentsPanel(d dashboardData, width int) string {
	if width < 20 {
		width = 20
	}

	// Figure out column widths inside the panel (width-2 for borders, -2 for padding).
	inner := width - 4

	// Single global max across both series so a recall of 48 and a fact of 7
	// render at honest relative sizes (44% vs 15%, not 100% vs 35%).
	globalMax := 0
	maxName := 0
	for _, a := range d.Agents {
		if a.Facts > globalMax {
			globalMax = a.Facts
		}
		if a.Recalls > globalMax {
			globalMax = a.Recalls
		}
		if n := lipgloss.Width(a.ID); n > maxName {
			maxName = n
		}
	}
	if maxName > 18 {
		maxName = 18
	}

	// Reserve: name | bar | value-right.
	valueW := 5
	barW := inner - maxName - valueW - 2
	if barW < 8 {
		barW = 8
	}

	var rows []string
	limit := len(d.Agents)
	if limit > 8 {
		limit = 8
	}
	for i := 0; i < limit; i++ {
		a := d.Agents[i]
		name := a.ID
		if lipgloss.Width(name) > maxName {
			name = name[:maxName-1] + "…"
		}

		nameCell := styleSubText.Render(padRight(name, maxName))
		blank := strings.Repeat(" ", maxName)

		factsBar := hbarSlim(float64(a.Facts), float64(globalMax), barW, colorCyan)
		factsVal := lipgloss.NewStyle().Foreground(colorCyan).Render(
			padLeft(formatCompact(a.Facts), valueW))
		rows = append(rows, nameCell+" "+factsBar+" "+factsVal)

		recallsBar := hbarSlim(float64(a.Recalls), float64(globalMax), barW, colorAmber)
		recallsVal := lipgloss.NewStyle().Foreground(colorAmber).Render(
			padLeft(formatCompact(a.Recalls), valueW))
		rows = append(rows, blank+" "+recallsBar+" "+recallsVal)

		// Dotted separator between agents — guides the eye without the
		// density of a blank row alone.
		if i < limit-1 {
			sepLen := inner
			if sepLen < 0 {
				sepLen = 0
			}
			sep := styleHelpSep.Render(strings.Repeat("·", sepLen))
			rows = append(rows, sep)
		}
	}

	legend := styleDimText.Render(
		"  " + lipgloss.NewStyle().Foreground(colorCyan).Render("▄") +
			" facts    " +
			lipgloss.NewStyle().Foreground(colorAmber).Render("▄") +
			" recalls",
	)
	rows = append(rows, "", legend)

	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return panelBox("Agents · Inventory vs Activity", width, body)
}

// renderWeightPanel renders the weight-distribution histogram + corpus
// oldest/newest timestamps. outerH (≤0 = natural) lets the caller force
// the panel to a target height so it stacks flush against a taller sibling.
func (m tuiModel) renderWeightPanel(d dashboardData, width, outerH int) string {
	if width < 20 {
		width = 20
	}
	inner := width - 4
	labelW := 8
	pctW := 5
	barW := inner - labelW - pctW - 3
	if barW < 6 {
		barW = 6
	}

	labels := []string{"> 0.8", "0.5-0.8", "0.2-0.5", "< 0.2"}
	colors := []lipgloss.Color{colorMint, colorCyan, colorAmber, colorRose}

	maxCount := 0
	for _, v := range d.WeightBuckets {
		if v > maxCount {
			maxCount = v
		}
	}
	total := d.FactsN

	var rows []string
	for i, lbl := range labels {
		count := d.WeightBuckets[i]
		pct := 0.0
		if total > 0 {
			pct = float64(count) / float64(total) * 100
		}
		labelCell := styleSubText.Render(padRight(lbl, labelW))
		bar := hbar(float64(count), float64(maxCount), barW, colors[i])
		pctCell := lipgloss.NewStyle().Foreground(colors[i]).Render(
			padRight(fmt.Sprintf("%.0f%%", pct), pctW))
		rows = append(rows, labelCell+" "+bar+" "+pctCell)
	}

	// Compact footer: average weight + corpus span on one dim line. The
	// Token Cost panel now owns the top half of the right column, so Weight
	// Distribution is squeezed to its essentials.
	var span string
	if !d.OldestAt.IsZero() && !d.NewestAt.IsZero() {
		span = fmt.Sprintf(
			"%s → %s",
			d.OldestAt.Format("01-02"),
			d.NewestAt.Format("01-02"),
		)
	}
	rows = append(rows, styleDimText.Render(fmt.Sprintf(
		"  avg %.2f · %s", d.AvgWeight, span)))

	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return panelBoxH("Weight Distribution", width, outerH, body)
}

// renderActivityPanel renders the 30-day facts-created sparkline. Honest
// label — graymatter doesn't persist retrieval history, so this reflects
// fact-creation velocity, not recalls-per-day.
//
// The peak value is anchored above the tallest sparkline cell so the reader
// can see the magnitude in context, not just as a footer fact.
func (m tuiModel) renderActivityPanel(d dashboardData, width int) string {
	if width < 40 {
		width = 40
	}
	inner := width - 4

	// The sparkline uses one cell per day (30 cells) but we center it inside
	// the panel with a short label on the left.
	sparkWidth := len(d.DailyCreated)
	leftLabel := styleSubText.Render(padRight("30d", 4))
	blankLeft := strings.Repeat(" ", lipgloss.Width("30d"))
	sparkline := spark(d.DailyCreated[:], colorAmber)

	peak, peakIdx := 0, -1
	for i, v := range d.DailyCreated {
		if v > peak {
			peak = v
			peakIdx = i
		}
	}

	// Peak anchor: number centred over the tallest cell. Left-pad with the
	// same spacing the sparkline uses so the label aligns column-accurate.
	var peakRow string
	if peak > 0 && peakIdx >= 0 {
		peakStr := fmt.Sprintf("%d", peak)
		// Centre the label over the peak cell by shifting left by half
		// the label width (but never past the sparkline origin).
		shift := peakIdx - lipgloss.Width(peakStr)/2
		if shift < 0 {
			shift = 0
		}
		anchor := strings.Repeat(" ", shift)
		peakLbl := lipgloss.NewStyle().Foreground(colorAmber).Bold(true).Render(peakStr)
		peakRow = blankLeft + " " + anchor + peakLbl
	}

	line1 := leftLabel + " " + sparkline
	// Date footer aligned under the sparkline.
	startLbl := d.DailyStart.Format("01-02")
	endLbl := d.DailyStart.AddDate(0, 0, 29).Format("01-02")
	footerSpace := sparkWidth - lipgloss.Width(startLbl) - lipgloss.Width(endLbl)
	if footerSpace < 1 {
		footerSpace = 1
	}
	dateRow := blankLeft + " " + styleDimText.Render(
		startLbl+strings.Repeat(" ", footerSpace)+endLbl,
	)

	meta := styleDimText.Render(fmt.Sprintf(
		"  total %d in window",
		sumInts(d.DailyCreated[:]),
	))

	_ = inner
	var body string
	if peakRow != "" {
		body = lipgloss.JoinVertical(lipgloss.Left, peakRow, line1, dateRow, "", meta)
	} else {
		body = lipgloss.JoinVertical(lipgloss.Left, line1, dateRow, "", meta)
	}
	return panelBox("Activity · Facts Created (last 30 days)", width, body)
}

func sumInts(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}

// renderTokenPanel renders the "Token Cost · 30d" card: total USD spent in
// window, a cache-hit-rate hero line (the virality hook — prompt caching is
// where graymatter's memory layer actually saves the user money), and a
// per-model breakdown. Empty-state when no harness runs have been recorded.
//
// All numbers come from the `token_usage` bbolt bucket, populated by
// harness.RecordTokenUsage on every successful Anthropic call — zero
// estimation, zero extrapolation.
func (m tuiModel) renderTokenPanel(d dashboardData, width int) string {
	if width < 24 {
		width = 24
	}
	ts := d.Tokens

	// Empty state: no harness runs yet. Render a single-line hint so the
	// panel isn't a floating blank — it should look intentional, not broken.
	if !ts.Loaded || ts.Requests == 0 {
		lines := []string{
			styleDimText.Render("  No agent runs yet."),
			styleDimText.Render("  Tracked automatically on"),
			styleDimText.Render("  " + lipgloss.NewStyle().Foreground(colorCyan).Render("graymatter run") + "."),
		}
		body := lipgloss.JoinVertical(lipgloss.Left, lines...)
		return panelBox("Token Cost · 30d", width, body)
	}

	// Hero line: big USD total in purple + tiny "last 30d" annotation.
	totalStyle := lipgloss.NewStyle().Bold(true).Foreground(colorPurple)
	hero := totalStyle.Render(formatUSD(ts.TotalUSD)) +
		" " + styleDimText.Render(fmt.Sprintf("· %s reqs", formatCompact(int(ts.Requests))))
	if ts.Partial {
		hero += " " + styleDimText.Render("(partial)")
	}

	// Cache-hit headline: the most compelling number for social. Colour
	// scales with hit rate so a healthy setup literally glows green.
	hitPct := ts.CacheHitRate * 100
	hitColor := colorRose
	switch {
	case hitPct >= 60:
		hitColor = colorMint
	case hitPct >= 30:
		hitColor = colorAmber
	}
	hitLine := styleDimText.Render("cache hit ") +
		lipgloss.NewStyle().Foreground(hitColor).Bold(true).Render(
			fmt.Sprintf("%.0f%%", hitPct)) +
		"  " + styleDimText.Render(fmt.Sprintf(
			"%s reads · %s fresh",
			formatCompact(int(ts.CacheRead)),
			formatCompact(int(ts.Input)),
		))

	rows := []string{hero, hitLine, ""}

	// Per-model breakdown — one line per model, share% rendered as a slim
	// cyan bar. Up to 3 models (more would blow the compact right column).
	if len(ts.ByModel) > 0 {
		rows = append(rows, styleDimText.Render("  by model"))
		inner := width - 4
		modelW := 10
		costW := 7
		barW := inner - modelW - costW - 2
		if barW < 6 {
			barW = 6
		}
		limit := len(ts.ByModel)
		if limit > 3 {
			limit = 3
		}
		for i := 0; i < limit; i++ {
			mb := ts.ByModel[i]
			label := mb.Model
			if lipgloss.Width(label) > modelW {
				label = label[:modelW-1] + "…"
			}
			bar := hbarSlim(mb.Sharepct, 100, barW, colorCyan)
			costCell := lipgloss.NewStyle().Foreground(colorWhite).Render(
				padLeft(formatUSD(mb.CostUSD), costW))
			rows = append(rows,
				styleSubText.Render(padRight(label, modelW))+" "+bar+" "+costCell)
		}
	}

	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return panelBox("Token Cost · 30d", width, body)
}

// formatUSD renders a USD amount with sensible precision for a dashboard:
// "$0.00" below a dollar, "$12.84" in the common range, "$1.4K" and "$2.3M"
// when the spend gets loud. Always prefixed with "$" so it's unambiguous.
func formatUSD(v float64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs < 1:
		return fmt.Sprintf("$%.2f", v)
	case abs < 1000:
		return fmt.Sprintf("$%.2f", v)
	case abs < 1_000_000:
		return fmt.Sprintf("$%.1fK", v/1000)
	default:
		return fmt.Sprintf("$%.1fM", v/1_000_000)
	}
}
