package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestHookLatencyBudgets runs the real binary against the machine-relative
// contract: the median user-prompt and session-end deltas from pre-compact
// must stay within recallDeltaBudget and sessionEndDeltaBudget (200 ms each),
// and normalized scaling must stay within recallScalingMaxNormalized (2.5x).
// Pre-compact is the per-run baseline and has no absolute gate.
//
// CI runs this timing measurement report-only with continue-on-error; it is
// deliberately excluded from the blocking benchmark-package tests because
// shared-runner timings are noisy.
func TestHookLatencyBudgets(t *testing.T) {
	if testing.Short() {
		t.Skip("hook latency gate needs process spawns; skipped in -short")
	}

	var buf bytes.Buffer
	start := time.Now()
	if err := run(&buf); err != nil {
		t.Fatalf("hook latency gate failed after %s:\n%s\n%v", time.Since(start).Round(time.Second), buf.String(), err)
	}
	t.Logf("gate passed in %s:\n%s", time.Since(start).Round(time.Second), buf.String())

	// The report must name every gated event — a gate that silently stops
	// measuring one of them is worse than a failing one.
	out := buf.String()
	if !strings.Contains(out, "Embedder: keyword (no LLM, no network, no API key)") {
		t.Error("report missing the explicit keyword embedder")
	}
	for _, event := range []string{"user-prompt", "pre-compact", "session-end"} {
		if !strings.Contains(out, event) {
			t.Errorf("report missing the %s row", event)
		}
	}
}
