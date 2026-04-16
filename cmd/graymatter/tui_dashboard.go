package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

	// Row 2 — Agents panel | Weight distribution panel (side-by-side).
	leftW := w * 3 / 5
	rightW := w - leftW - 1
	if rightW < 22 {
		rightW = 22
		leftW = w - rightW - 1
	}
	agents := m.renderAgentsPanel(d, leftW)
	weights := m.renderWeightPanel(d, rightW)
	row2 := lipgloss.JoinHorizontal(lipgloss.Top, agents, " ", weights)

	// Row 3 — Activity sparkline panel (full width).
	activity := m.renderActivityPanel(d, w)

	body := lipgloss.JoinVertical(lipgloss.Left, kpiRow, "", row2, "", activity)
	return body
}

// renderKPIRow renders the 4-tile CodeBurn-style KPI strip.
func (m tuiModel) renderKPIRow(d dashboardData, width int) string {
	tileW := width / 4
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
// two stacked bars — fact count (cyan) and recall count (amber).
func (m tuiModel) renderAgentsPanel(d dashboardData, width int) string {
	if width < 20 {
		width = 20
	}

	// Figure out column widths inside the panel (width-2 for borders, -2 for padding).
	inner := width - 4

	// Max across agents for normalization.
	maxFacts, maxRecalls := 0, 0
	maxName := 0
	for _, a := range d.Agents {
		if a.Facts > maxFacts {
			maxFacts = a.Facts
		}
		if a.Recalls > maxRecalls {
			maxRecalls = a.Recalls
		}
		if n := lipgloss.Width(a.ID); n > maxName {
			maxName = n
		}
	}
	if maxName > 18 {
		maxName = 18
	}

	// Reserve: name | value | bar
	valueW := 6
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

		factsVal := lipgloss.NewStyle().Foreground(colorCyan).Render(
			padRight(formatCompact(a.Facts), valueW))
		factsBar := hbar(float64(a.Facts), float64(maxFacts), barW, colorCyan)
		rows = append(rows, nameCell+" "+factsVal+" "+factsBar)

		blank := strings.Repeat(" ", maxName)
		recallsVal := lipgloss.NewStyle().Foreground(colorAmber).Render(
			padRight(formatCompact(a.Recalls), valueW))
		recallsBar := hbar(float64(a.Recalls), float64(maxRecalls), barW, colorAmber)
		rows = append(rows, blank+" "+recallsVal+" "+recallsBar)

		if i < limit-1 {
			rows = append(rows, "")
		}
	}

	legend := styleDimText.Render(
		"  " + lipgloss.NewStyle().Foreground(colorCyan).Render("■") +
			" facts    " +
			lipgloss.NewStyle().Foreground(colorAmber).Render("■") +
			" recalls",
	)
	rows = append(rows, "", legend)

	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return panelBox("Agents · Inventory vs Activity", width, body)
}

// renderWeightPanel renders the weight-distribution histogram + corpus
// oldest/newest timestamps.
func (m tuiModel) renderWeightPanel(d dashboardData, width int) string {
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

	rows = append(rows, "", styleDimText.Render("  Corpus age"))
	if !d.OldestAt.IsZero() {
		rows = append(rows, styleSubText.Render(
			"  oldest  "+d.OldestAt.Format("2006-01-02")))
	}
	if !d.NewestAt.IsZero() {
		rows = append(rows, styleSubText.Render(
			"  newest  "+d.NewestAt.Format("2006-01-02")))
	}
	rows = append(rows, styleSubText.Render(
		fmt.Sprintf("  avg     %.3f", d.AvgWeight)))

	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return panelBox("Weight Distribution", width, body)
}

// renderActivityPanel renders the 30-day facts-created sparkline. Honest
// label — graymatter doesn't persist retrieval history, so this reflects
// fact-creation velocity, not recalls-per-day.
func (m tuiModel) renderActivityPanel(d dashboardData, width int) string {
	if width < 40 {
		width = 40
	}
	inner := width - 4

	// The sparkline uses one cell per day (30 cells) but we center it inside
	// the panel with a short label on the left.
	sparkWidth := len(d.DailyCreated)
	leftLabel := styleSubText.Render(padRight("30d", 4))
	sparkline := spark(d.DailyCreated[:], colorAmber)

	peak := 0
	for _, v := range d.DailyCreated {
		if v > peak {
			peak = v
		}
	}

	line1 := leftLabel + " " + sparkline
	// Date footer aligned under the sparkline.
	startLbl := d.DailyStart.Format("01-02")
	endLbl := d.DailyStart.AddDate(0, 0, 29).Format("01-02")
	footerSpace := sparkWidth - lipgloss.Width(startLbl) - lipgloss.Width(endLbl)
	if footerSpace < 1 {
		footerSpace = 1
	}
	dateRow := "    " + styleDimText.Render(
		startLbl+strings.Repeat(" ", footerSpace)+endLbl,
	)

	meta := styleDimText.Render(fmt.Sprintf(
		"  peak %d facts/day · total %d in window",
		peak, sumInts(d.DailyCreated[:]),
	))

	_ = inner
	body := lipgloss.JoinVertical(lipgloss.Left, line1, dateRow, "", meta)
	return panelBox("Activity · Facts Created (last 30 days)", width, body)
}

func sumInts(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}
