// genchart renders the README's token-reduction chart as SVG from the table
// published in docs/benchmarks.md — the same table a CI test replays against
// a live benchmark run on every build.
//
// One number, one source. A static image exported by hand cannot inherit the
// machine checks that gate every other published figure and can drift in
// either direction without anything noticing. This tool closes that gap the
// same way every other number is closed: read from the gated table,
// regenerate with one command,
//
//	go run ./benchmarks/genchart
//
// and commit the result.
package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type row struct {
	sessions int
	full     int
	gm       int
	pct      int
}

var rowRe = regexp.MustCompile(`^\|\s*(\d[\d,]*)\s*\|\s*~?\s*([\d,]+)\s*tokens\s*\|\s*~?\s*([\d,]+)\s*tokens\s*\|\s*\*{0,2}(\d+)\s*%\s*\*{0,2}\s*\|`)

func parseTable(md string) ([]row, error) {
	var rows []row
	for _, line := range strings.Split(md, "\n") {
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		num := func(s string) int {
			s = strings.ReplaceAll(strings.ReplaceAll(s, ",", ""), "~", "")
			n, _ := strconv.Atoi(s)
			return n
		}
		rows = append(rows, row{
			sessions: num(m[1]),
			full:     num(m[2]),
			gm:       num(m[3]),
			pct:      num(m[4]),
		})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no benchmark rows found in input")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].sessions < rows[j].sessions })
	return rows, nil
}

const (
	W, H       = 820, 400
	padL, padR = 70, 24
	padTop     = 56
	padBottom  = 86
	groupGap   = 44
	barW       = 52
	barGap     = 12
)

func render(rows []row) string {
	maxVal := 0
	for _, r := range rows {
		if r.full > maxVal {
			maxVal = r.full
		}
	}
	plotW := float64(W - padL - padR)
	plotH := float64(H - padTop - padBottom)
	groupW := plotW / float64(len(rows))
	scale := plotH / float64(maxVal)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="ui-sans-serif,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">`, W, H, W, H)

	el := func(format string, args ...any) {
		b.WriteByte('\n')
		fmt.Fprintf(&b, format, args...)
	}

	el(`<rect width="100%%" height="100%%" fill="#ffffff"/>`)
	el(`<text x="10" y="30" font-size="19" font-weight="600" fill="#0f172a">Context tokens per query vs full-history injection</text>`)
	el(`<text x="10" y="50" font-size="12.5" fill="#64748b">Approx tokens = words × 1.33 · lower is better · generated from the gated table in docs/benchmarks.md</text>`)

	for i := 1; i <= 4; i++ {
		y := padTop + plotH*(1-float64(i)/4)
		val := maxVal * i / 4
		el(`<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#e2e8f0" stroke-width="1"/>`, padL, y, W-padR, y)
		el(`<text x="%d" y="%.1f" font-size="11" fill="#94a3b8" text-anchor="end">%s</text>`, padL-8, y+4, thousands(val))
	}

	for gi, r := range rows {
		gx := float64(padL) + groupGap/2 + float64(gi)*groupW
		cx := gx + groupW/2 - groupGap/2

		hFull := float64(r.full) * scale
		xFull := cx - float64(barW)/2 - float64(barGap)
		el(`<rect x="%.1f" y="%.1f" width="%d" height="%.1f" rx="3" fill="#cbd5e1"/>`, xFull, padTop+plotH-hFull, barW, hFull)
		el(`<text x="%.1f" y="%.1f" font-size="11.5" fill="#64748b" text-anchor="middle">%s</text>`,
			xFull+float64(barW)/2, padTop+plotH-hFull-7, thousands(r.full))

		hGm := float64(r.gm) * scale
		xGm := cx + float64(barGap)/2
		el(`<rect x="%.1f" y="%.1f" width="%d" height="%.1f" rx="3" fill="#2563eb"/>`, xGm, padTop+plotH-hGm, barW, hGm)
		gmLabel := thousands(r.gm)
		if r.pct > 0 {
			gmLabel += fmt.Sprintf(" (−%d%%)", r.pct)
		}
		el(`<text x="%.1f" y="%.1f" font-size="11.5" font-weight="600" fill="#2563eb" text-anchor="middle">%s</text>`,
			xGm+float64(barW)/2, padTop+plotH-hGm-7, gmLabel)

		sessionLabel := fmt.Sprintf("%d sessions", r.sessions)
		if r.sessions == 1 {
			sessionLabel = "1 session"
		}
		el(`<text x="%.1f" y="%.1f" font-size="12" fill="#0f172a" text-anchor="middle">%s</text>`, cx, padTop+plotH+22, sessionLabel)
	}

	baseY := padTop + plotH
	el(`<rect x="%d" y="%.1f" width="12" height="12" rx="2" fill="#cbd5e1"/>`, padL, baseY+40)
	el(`<text x="%d" y="%.1f" font-size="12" fill="#475569">full history</text>`, padL+18, baseY+50)
	el(`<rect x="%d" y="%.1f" width="12" height="12" rx="2" fill="#2563eb"/>`, padL+110, baseY+40)
	el(`<text x="%d" y="%.1f" font-size="12" fill="#475569">GrayMatter top-8</text>`, padL+128, baseY+50)

	b.WriteString("\n</svg>\n")
	return b.String()
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	return thousandsFromString(s[:len(s)-3]) + "," + s[len(s)-3:]
}

func thousandsFromString(s string) string {
	if len(s) <= 3 {
		return s
	}
	return thousandsFromString(s[:len(s)-3]) + "," + s[len(s)-3:]
}

func main() {
	in, out := "docs/benchmarks.md", ".github/assets/token-reduction.svg"
	if len(os.Args) > 1 {
		in = os.Args[1]
	}
	if len(os.Args) > 2 {
		out = os.Args[2]
	}
	md, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", in, err)
		os.Exit(1)
	}
	rows, err := parseTable(string(md))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, []byte(render(rows)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d rows)\n", out, len(rows))
}
