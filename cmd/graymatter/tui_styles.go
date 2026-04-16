package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ── Palette ──────────────────────────────────────────────────────────────────
//
// Neo-dark palette tuned for dark terminals. Colors are chosen to read well
// against both true-black and iTerm/Windows Terminal defaults. Deliberately
// uses 24-bit hex — graceful fallback happens inside lipgloss when the
// terminal only supports 256 or 16 colors.

var (
	colorAccent = lipgloss.Color("#7C7CFF") // primary indigo
	colorOrange = lipgloss.Color("#FF875F") // focus / active border
	colorDim    = lipgloss.Color("#6B7280") // muted secondary
	colorGreen  = lipgloss.Color("#5FAF5F") // success
	colorRed    = lipgloss.Color("#FF5F5F") // error
	colorYellow = lipgloss.Color("#FFAF00") // pending / warning
	colorWhite  = lipgloss.Color("#FFFFFF") // high-contrast fg

	colorPurple = lipgloss.Color("#A78BFA") // logo & KPI accent
	colorCyan   = lipgloss.Color("#22D3EE") // inventory bars / key hints
	colorAmber  = lipgloss.Color("#F59E0B") // activity bars
	colorSlate  = lipgloss.Color("#4B5563") // inactive borders / separators
	colorMint   = lipgloss.Color("#34D399") // health good
	colorRose   = lipgloss.Color("#FB7185") // health bad
	colorInk    = lipgloss.Color("#111827") // tile background
	colorSub    = lipgloss.Color("#9CA3AF") // secondary text on dark
)

// ── Chrome ───────────────────────────────────────────────────────────────────

var (
	styleLogo = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite).
			Background(colorPurple).
			Padding(0, 1)

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite).
			Background(colorAccent).
			Padding(0, 1)

	styleTabActive = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite).
			Background(colorAccent).
			Padding(0, 2)

	styleTabInactive = lipgloss.NewStyle().
				Foreground(colorSub).
				Padding(0, 2)

	styleVersion = lipgloss.NewStyle().
			Foreground(colorDim).
			Padding(0, 1)

	styleBorderActive = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorOrange)

	styleBorderInactive = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorSlate)

	styleHelp = lipgloss.NewStyle().
			Foreground(colorDim).
			Padding(0, 1)

	styleHelpKey = lipgloss.NewStyle().
			Foreground(colorCyan).
			Bold(true)

	styleHelpSep = lipgloss.NewStyle().
			Foreground(colorSlate)

	styleDimText = lipgloss.NewStyle().
			Foreground(colorDim)

	styleSubText = lipgloss.NewStyle().
			Foreground(colorSub)

	styleStatusOK      = lipgloss.NewStyle().Foreground(colorGreen)
	styleStatusFail    = lipgloss.NewStyle().Foreground(colorRed)
	styleStatusPending = lipgloss.NewStyle().Foreground(colorYellow)
)

// ── Dashboard primitives ─────────────────────────────────────────────────────

var (
	stylePanelTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPurple).
			Padding(0, 1)

	stylePanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorSlate).
			Padding(0, 1)

	styleKPILabel = lipgloss.NewStyle().
			Foreground(colorSub).
			Bold(true)

	styleKPIValue = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite)

	styleKPIUnit = lipgloss.NewStyle().
			Foreground(colorDim)
)

// panelBox wraps body in a rounded-border panel with a coloured title.
// Width is the outer width — the border subtracts 2 internally.
func panelBox(title string, width int, body string) string {
	if width < 10 {
		width = 10
	}
	inner := width - 2
	titleLine := stylePanelTitle.Render("▸ " + title)
	content := lipgloss.JoinVertical(lipgloss.Left, titleLine, body)
	return stylePanel.Width(inner).Render(content)
}

// kpiBlock renders a single KPI tile: label, value, optional unit.
// The accent color is applied to the value text.
func kpiBlock(label, value, unit string, accent lipgloss.Color, width int) string {
	if width < 10 {
		width = 10
	}
	labelLine := styleKPILabel.Render(label)
	valueStyle := styleKPIValue.Foreground(accent)
	var valLine string
	if unit != "" {
		valLine = valueStyle.Render(value) + " " + styleKPIUnit.Render(unit)
	} else {
		valLine = valueStyle.Render(value)
	}

	body := lipgloss.JoinVertical(lipgloss.Left, labelLine, valLine)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorSlate).
		Padding(0, 1).
		Width(width - 2).
		Render(body)
}

// hbar renders a horizontal bar using Unicode block-partial characters so it
// can represent sub-cell precision. value/max ∈ [0, 1] range. width is the
// total cell count available for the bar glyphs (not counting label).
func hbar(value, max float64, width int, c lipgloss.Color) string {
	if width <= 0 {
		return ""
	}
	if max <= 0 {
		max = 1
	}
	ratio := value / max
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	total := ratio * float64(width)
	full := int(total)
	frac := total - float64(full)

	// Eighth-block partials, brightest to dimmest.
	parts := []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}
	var b strings.Builder
	for i := 0; i < full && i < width; i++ {
		b.WriteRune('█')
	}
	if full < width {
		idx := int(frac * 8)
		if idx < 0 {
			idx = 0
		} else if idx > 7 {
			idx = 7
		}
		b.WriteRune(parts[idx])
		for i := full + 1; i < width; i++ {
			b.WriteRune(' ')
		}
	}
	return lipgloss.NewStyle().Foreground(c).Render(b.String())
}

// spark renders a sparkline of values, scaled to the [0, peak] range over a
// fixed width (one cell per value). If len(values) > width, the tail is used.
func spark(values []int, c lipgloss.Color) string {
	if len(values) == 0 {
		return ""
	}
	blocks := []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	peak := 0
	for _, v := range values {
		if v > peak {
			peak = v
		}
	}
	var b strings.Builder
	for _, v := range values {
		var idx int
		if peak == 0 {
			idx = 0
		} else {
			idx = int(float64(v) / float64(peak) * 8)
			if idx < 0 {
				idx = 0
			} else if idx > 8 {
				idx = 8
			}
		}
		b.WriteRune(blocks[idx])
	}
	return lipgloss.NewStyle().Foreground(c).Render(b.String())
}

// formatBytes renders a byte count compactly: 1023 → "1023 B", 1024 → "1.0 KB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffix)
}

// formatCompact renders an integer in human-friendly compact form.
// 999 → "999", 1500 → "1.5K", 2_300_000 → "2.3M".
func formatCompact(n int) string {
	abs := n
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs < 1000:
		return fmt.Sprintf("%d", n)
	case abs < 1_000_000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	case abs < 1_000_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
}

// padRight pads s with spaces so that its display width reaches width.
// Uses lipgloss.Width for correct ANSI-aware measurement.
func padRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// border picks the active/inactive border style.
func border(active bool) lipgloss.Style {
	if active {
		return styleBorderActive
	}
	return styleBorderInactive
}
