package benchsyn

import (
	"math"
	"testing"
)

// The published token table (README.md and docs/benchmarks.md, gated on the
// root-module side by benchmarks/token_count/main_test.go) is the single
// source of truth for what this suite is allowed to report. This test pins
// the CLI's live run to the same figures at the same tolerance the gate uses,
// so `graymatter bench` and `go run ./benchmarks/token_count` cannot disagree
// without one of the two failing.
//
// If a corpus or engine change legitimately moves the numbers, update them in
// the benchmark, regenerate the README tables, and mirror the new constants
// here — in that order, with the reasoning in the commit message.
const (
	tolerance = 0.02
	absFloor  = 5
)

var publishedRows = []struct {
	sessions     int
	fullTokens   int
	recallTokens int
	reduction    int // integer percent; compared exactly, as the gate does
}{
	{1, 80, 80, 0},
	{10, 630, 550, 12},
	{30, 1880, 550, 71},
	{100, 6960, 670, 90},
}

func TestPublishedTokenTable_MatchesCLIRun(t *testing.T) {
	results, err := RunTokenCount()
	if err != nil {
		t.Fatalf("RunTokenCount: %v", err)
	}
	bySessions := make(map[int]TokenResult, len(results))
	for _, r := range results {
		bySessions[r.Sessions] = r
	}

	for _, want := range publishedRows {
		got, ok := bySessions[want.sessions]
		if !ok {
			t.Fatalf("suite no longer measures %d sessions", want.sessions)
		}
		assertClose(t, want.sessions, "full injection", want.fullTokens, got.FullTokens)
		assertClose(t, want.sessions, "recall", want.recallTokens, got.RecallTokens)
		if reduction := int(math.Round(got.Reduction)); reduction != want.reduction {
			t.Errorf("%d sessions: CLI reports %d%% reduction, published table says %d%%",
				want.sessions, reduction, want.reduction)
		}
	}
}

func assertClose(t *testing.T, sessions int, label string, published, measured int) {
	t.Helper()
	if measured == 0 {
		if published != 0 {
			t.Errorf("%d sessions: published %d %s tokens, measured 0", sessions, published, label)
		}
		return
	}
	absErr := math.Abs(float64(published - measured))
	relErr := absErr / float64(measured)
	if relErr > tolerance && absErr > absFloor {
		t.Errorf("%d sessions: published ~%d %s tokens, CLI measures %d (%.0f%% off, max %.0f%%)",
			sessions, published, label, measured, relErr*100, tolerance*100)
	}
}
