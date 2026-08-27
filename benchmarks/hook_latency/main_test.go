package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestHookLatencyBudgets is the CI gate: the hook budgets are machine-checked
// against the real binary, exactly like every other published number. A
// breach fails the benchmark package, which ci.yml runs as a blocking job
// (go test ./benchmarks/...).
//
// Budgets (playbook, mirrored in cmd/graymatter/hooks_run.go):
//
//	user-prompt  p99 < 150 ms on a 10k-fact store
//	session-end  max < 500 ms
//	pre-compact  max < 200 ms
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
	for _, event := range []string{"user-prompt", "pre-compact", "session-end"} {
		if !strings.Contains(out, event) {
			t.Errorf("report missing the %s row", event)
		}
	}
}
