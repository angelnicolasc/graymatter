package benchsyn

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hook-latency audit is the one benchsyn runner that spawns processes, so
// it gets a full-pipeline test with scaled-down parameters (same code path as
// the published run: seed → warm-up → measured process spawns → hooks.log
// parse → budget gating → report). The published-scale run is the
// benchmarks/hook_latency gate's job; this one only proves the pipeline and
// the report honest.
func TestRunHookLatency_ScaledPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real hook processes; skipped in -short")
	}

	// The runner executes a graymatter binary; build the current tree once.
	bin := filepath.Join(t.TempDir(), "graymatter-under-test.exe")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/angelnicolasc/graymatter/cmd/graymatter").CombinedOutput()
	if err != nil {
		t.Fatalf("build test binary: %v: %s", err, out)
	}

	var buf bytes.Buffer
	report, err := RunHookLatency(HookLatencyParams{
		Binary:    bin,
		SeedFacts: 60,
		Warmup:    1,
		Runs:      2,
	}, &buf)
	if err != nil {
		t.Fatalf("RunHookLatency: %v\n%s", err, buf.String())
	}

	if len(report.Rows) != 3 {
		t.Fatalf("report rows = %d, want 3 (user-prompt, pre-compact, session-end)", len(report.Rows))
	}
	if !report.Pass {
		t.Errorf("scaled run breached gates (scaled params, so any breach is a pipeline bug):\n%s", buf.String())
	}
	if report.SeedFacts != 60 || report.Runs != 2 {
		t.Errorf("report facts=%d runs=%d, want the scaled params", report.SeedFacts, report.Runs)
	}
	if report.DeltaBudgetMs != msDuration(HookRecallDeltaBudget) {
		t.Errorf("report delta budget = %v, want %v", report.DeltaBudgetMs, msDuration(HookRecallDeltaBudget))
	}
	if report.ScalingNormalized <= 0 || report.ScalingNormalized > HookScalingMaxNormalized {
		t.Errorf("scaling normalized = %v, want (0, %.1f]", report.ScalingNormalized, HookScalingMaxNormalized)
	}
	// The injected-block guard: with corpus-matching queries the user-prompt
	// runs must have injected (2 measured runs).
	if !strings.Contains(buf.String(), "all hook gates hold") {
		t.Errorf("report text missing the pass line:\n%s", buf.String())
	}

	for _, row := range report.Rows {
		if row.Event == "" {
			t.Errorf("row %+v malformed", row)
		}
	}
	if report.Rows[0].Event != "user-prompt" || report.Rows[2].Event != "session-end" {
		t.Errorf("row order = %s..%s, want user-prompt first, session-end last",
			report.Rows[0].Event, report.Rows[2].Event)
	}
}

// TestHookLatency_GateConstantsAreThePublishedOnes pins the machine-relative
// gate constants: they are published claims (README, docs) and a silent drift
// here would audit against the wrong bar. The absolute 150 ms figure is the
// reference-hardware number, deliberately not a gate.
func TestHookLatency_GateConstantsAreThePublishedOnes(t *testing.T) {
	if HookRecallDeltaBudget != 200*time.Millisecond {
		t.Errorf("HookRecallDeltaBudget = %v, want 200ms", HookRecallDeltaBudget)
	}
	if HookSessionEndDeltaBudget != 200*time.Millisecond {
		t.Errorf("HookSessionEndDeltaBudget = %v, want 200ms", HookSessionEndDeltaBudget)
	}
	if HookScalingMaxNormalized != 2.5 {
		t.Errorf("HookScalingMaxNormalized = %v, want 2.5x of linear", HookScalingMaxNormalized)
	}
	if HookUserPromptBudget != 150*time.Millisecond {
		t.Errorf("HookUserPromptBudget = %v, want the published 150ms reference", HookUserPromptBudget)
	}
	if HookSeedFacts != 10000 {
		t.Errorf("HookSeedFacts = %d, want the published 10k-fact store", HookSeedFacts)
	}
}

// TestLastHookInternalMs_MissingFieldIsError guards the log-parse path: a
// hooks.log line without "ms" must surface as an error upstream, never as a
// zero-duration sample that flatters the measurement.
func TestLastHookInternalMs_MissingFieldIsError(t *testing.T) {
	dir := t.TempDir()
	// No hooks.log at all.
	if _, err := lastHookInternalMs(dir); err == nil {
		t.Error("missing hooks.log must error")
	}
	// A line without ms.
	logPath := filepath.Join(dir, "hooks.log")
	if err := os.WriteFile(logPath, []byte(`{"event":"user-prompt","outcome":"ok"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lastHookInternalMs(dir); err == nil || !strings.Contains(err.Error(), "ms") {
		t.Errorf("line without ms field returned (%v, %v), want an error naming ms", -1, err)
	}
	// A well-formed line parses.
	if err := os.WriteFile(logPath, []byte(`{"event":"user-prompt","outcome":"ok","ms":42}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ms, err := lastHookInternalMs(dir)
	if err != nil || ms != 42*time.Millisecond {
		t.Errorf("well-formed line = (%v, %v), want (42ms, nil)", ms, err)
	}
}
