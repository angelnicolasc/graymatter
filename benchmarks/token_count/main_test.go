package main

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every token figure published in the repository has to come from this
// benchmark. Before v0.10.0 docs/benchmarks.md carried a table nothing
// produced (~40,000 → ~1,200 tokens at 100 sessions, against a measured
// ~6,959 → ~666) plus a "Relevance@8 ~91%" that no code in the tree
// measures. The README was correct and the wrong numbers still shipped for
// several releases, because "is this number real?" was a thing a human had
// to remember to ask.
//
// These tests ask it on every CI run: the published tables are parsed out of
// the markdown and compared against a live measurement.

// tolerance is the relative error allowed between a published figure and the
// measured one. The docs round to two or three significant figures on purpose
// — a reader wants ~6,960, not 6,959 — so exact equality would fail on
// rounding alone. 2% permits the rounding and nothing else: the fabricated
// 100-session row was off by 475%.
//
// absFloor covers the small end, where rounding costs more than 2%: the
// 1-session row measures 78 tokens and publishes ~80, which is 3% off and
// entirely honest. Below this many tokens nobody is being misled.
const (
	tolerance = 0.02
	absFloor  = 5
)

// docTable is one parsed markdown table row.
type docRow struct {
	sessions     int
	fullTokens   int
	recallTokens int
	reduction    int // percent; -1 when the table does not publish one
}

func TestPublishedTokenTables_MatchMeasuredBenchmark(t *testing.T) {
	measured, err := runBenchmark()
	if err != nil {
		t.Fatalf("runBenchmark: %v", err)
	}
	bySessions := make(map[int]result, len(measured))
	for _, r := range measured {
		bySessions[r.Sessions] = r
	}

	for _, doc := range []struct {
		name string
		path string
	}{
		{"README.md", "../../README.md"},
		{"docs/benchmarks.md", "../../docs/benchmarks.md"},
	} {
		t.Run(doc.name, func(t *testing.T) {
			raw, err := os.ReadFile(doc.path)
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			rows := parseTokenRows(string(raw))
			if len(rows) == 0 {
				t.Fatalf("%s publishes no token table; every token figure in the "+
					"repository must be traceable to ./benchmarks/token_count", doc.name)
			}

			for _, row := range rows {
				got, ok := bySessions[row.sessions]
				if !ok {
					t.Errorf("%s publishes a row for %d sessions, which the benchmark "+
						"does not measure (measured: %v)", doc.name, row.sessions, sessionCounts)
					continue
				}
				assertClose(t, doc.name, row.sessions, "full injection", row.fullTokens, got.FullTokens)
				assertClose(t, doc.name, row.sessions, "recall", row.recallTokens, got.RecallTokens)

				if row.reduction >= 0 {
					// The reduction column is the headline claim. It is an
					// integer in both the table and the program output, so it
					// gets no tolerance at all.
					if want := int(math.Round(got.Reduction)); row.reduction != want {
						t.Errorf("%s, %d sessions: published %d%% reduction, benchmark measures %d%%",
							doc.name, row.sessions, row.reduction, want)
					}
				}
			}
		})
	}
}

// TestDocsPublishNoUnmeasuredMetric guards the other half of the same problem.
// docs/benchmarks.md used to publish "Relevance@8 vs full context — ~91%".
// Nothing in the tree measures relevance, so the figure could not be wrong or
// right; it could only be believed. A metric may be published once something
// computes it.
func TestDocsPublishNoUnmeasuredMetric(t *testing.T) {
	raw, err := os.ReadFile("../../docs/benchmarks.md")
	if err != nil {
		t.Fatalf("read docs/benchmarks.md: %v", err)
	}
	text := string(raw)

	// Claims of a relevance/precision/recall *score* require a measurement.
	// The benchmark reports token counts only. Checked per line so a metric
	// name and its percentage are caught across markdown table cells, which is
	// exactly how the ~91% shipped.
	name := regexp.MustCompile(`(?i)\b(relevance|precision|hitrate|hit rate)\b`)
	pct := regexp.MustCompile(`\d+(\.\d+)?\s*%`)
	for i, line := range strings.Split(text, "\n") {
		if name.MatchString(line) && pct.MatchString(line) {
			t.Errorf("docs/benchmarks.md:%d publishes a quality score that no code measures:\n  %s\n"+
				"Either add the measurement to ./benchmarks/token_count and gate it in this test, "+
				"or remove the claim.", i+1, strings.TrimSpace(line))
		}
	}
}

func assertClose(t *testing.T, doc string, sessions int, label string, published, measured int) {
	t.Helper()
	if measured == 0 {
		if published != 0 {
			t.Errorf("%s, %d sessions: published %d %s tokens, benchmark measures 0",
				doc, sessions, published, label)
		}
		return
	}
	absErr := math.Abs(float64(published - measured))
	relErr := absErr / float64(measured)
	if relErr > tolerance && absErr > absFloor {
		t.Errorf("%s, %d sessions: published ~%d %s tokens, benchmark measures %d (%.0f%% off, max %.0f%%). "+
			"Update the table from `go run ./benchmarks/token_count`.",
			doc, sessions, published, label, measured, relErr*100, tolerance*100)
	}
}

// tokenRowRe matches a markdown table row whose first cell is a session count
// and whose next two cells are token figures, in either published layout:
//
//	| 100      | ~6,960 tokens | ~670 tokens | **90%** |
//	| Tokens per run (session 100)  | ~40,000 | ~1,200 |
var (
	tokenRowRe   = regexp.MustCompile(`^\|\s*(\d+)\s*\|\s*~?([\d,]+)[^|]*\|\s*~?([\d,]+)[^|]*\|(?:\s*\*{0,2}(\d+)%)?`)
	sessionRowRe = regexp.MustCompile(`^\|[^|]*session\s+(\d+)\s*\)?\s*\|\s*~?([\d,]+)[^|]*\|\s*~?([\d,]+)[^|]*\|`)
)

// parseTokenRows pulls every published (sessions, full, recall) triple out of
// a markdown document, in either of the two table layouts the repo has used.
func parseTokenRows(doc string) []docRow {
	var rows []docRow
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if m := tokenRowRe.FindStringSubmatch(line); m != nil {
			rows = append(rows, docRow{
				sessions:     atoiCommas(m[1]),
				fullTokens:   atoiCommas(m[2]),
				recallTokens: atoiCommas(m[3]),
				reduction:    atoiOrNeg(m[4]),
			})
			continue
		}
		if m := sessionRowRe.FindStringSubmatch(strings.ToLower(line)); m != nil {
			rows = append(rows, docRow{
				sessions:     atoiCommas(m[1]),
				fullTokens:   atoiCommas(m[2]),
				recallTokens: atoiCommas(m[3]),
				reduction:    -1,
			})
		}
	}
	return rows
}

func atoiCommas(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(s), ",", ""))
	return n
}

func atoiOrNeg(s string) int {
	if s == "" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// chartRows are the four rows drawn into .github/assets/token-reduction.png,
// which the README shows.
//
// A PNG cannot be parsed and compared the way the markdown tables above are,
// so this is a staleness alarm rather than a gate: it fails when
// docs/benchmarks.md stops saying what the image says, which is exactly when
// the image needs redrawing. Without it the chart could sit on the README
// showing last release's numbers with nothing objecting — the same failure
// that let a five-fold-wrong table survive several releases.
//
// The chart is redrawn from the design canvas, exported at 1600x800, and
// dropped in at .github/assets/token-reduction.png.
var chartRows = []docRow{
	{sessions: 1, fullTokens: 80, recallTokens: 80, reduction: 0},
	{sessions: 10, fullTokens: 630, recallTokens: 550, reduction: 12},
	{sessions: 30, fullTokens: 1880, recallTokens: 550, reduction: 71},
	{sessions: 100, fullTokens: 6960, recallTokens: 670, reduction: 90},
}

func TestChartImageMatchesPublishedTable(t *testing.T) {
	raw, err := os.ReadFile("../../docs/benchmarks.md")
	if err != nil {
		t.Fatalf("read docs/benchmarks.md: %v", err)
	}
	published := parseTokenRows(string(raw))

	bySessions := make(map[int]docRow, len(published))
	for _, r := range published {
		bySessions[r.sessions] = r
	}

	for _, want := range chartRows {
		got, ok := bySessions[want.sessions]
		if !ok {
			t.Errorf("the chart draws a %d-session group that docs/benchmarks.md no longer publishes; "+
				"redraw .github/assets/token-reduction.png", want.sessions)
			continue
		}
		if got.fullTokens != want.fullTokens || got.recallTokens != want.recallTokens || got.reduction != want.reduction {
			t.Errorf("the chart is stale at %d sessions.\n"+
				"  chart shows:   %d full, %d recall, %d%%\n"+
				"  the table says: %d full, %d recall, %d%%\n"+
				"Redraw .github/assets/token-reduction.png and update chartRows to match.",
				want.sessions,
				want.fullTokens, want.recallTokens, want.reduction,
				got.fullTokens, got.recallTokens, got.reduction)
		}
	}

	// A row added to the table and not to the chart is the same staleness in
	// the other direction.
	for _, r := range published {
		found := false
		for _, want := range chartRows {
			if want.sessions == r.sessions {
				found = true
			}
		}
		if !found {
			t.Errorf("docs/benchmarks.md publishes a %d-session row the chart does not draw; "+
				"redraw .github/assets/token-reduction.png", r.sessions)
		}
	}
}

// TestChartImageExists guards the README reference itself: the file has to be
// there, be a PNG, and be the size the README is written around.
func TestChartImageExists(t *testing.T) {
	const path = "../../.github/assets/token-reduction.png"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("README references %s but it cannot be read: %v", path, err)
	}
	// PNG signature, then IHDR width/height as big-endian uint32.
	if len(data) < 24 || string(data[1:4]) != "PNG" {
		t.Fatalf("%s is not a PNG", path)
	}
	width := int(data[16])<<24 | int(data[17])<<16 | int(data[18])<<8 | int(data[19])
	height := int(data[20])<<24 | int(data[21])<<16 | int(data[22])<<8 | int(data[23])
	if width != 1600 || height != 800 {
		t.Errorf("%s is %dx%d; the README shows it at 800 wide and expects a 1600x800 source",
			path, width, height)
	}
}
