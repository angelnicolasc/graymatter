package benchsyn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
)

// Hook latency: the published budgets the Claude Code hooks must meet,
// audited with the same methodology as benchmarks/hook_latency (the CI gate
// that owns the canonical copy — this package cannot import it for the same
// GOWORK=off reason documented in token_count.go, so the budgets live here as
// the CLI's single source and cmd/graymatter/hooks_run.go aliases them).
//
// What makes `graymatter bench --hooks` cheaper than the CI gate: the running
// CLI IS the hook binary, so nothing is compiled — the runner executes
// os.Executable() as a fresh process per sample, exactly the shape Claude
// Code fires.
//
// Methodology (mirrors the gate): seed a 10k-fact store through the library,
// warm up (daemon spawn, binary page-in), then run each hook event as a fresh
// process and gate the hook-internal time the runner reports to hooks.log.
// Wall time (spawn + init, platform-owned) is reported, not gated. Queries
// overlap the seeded corpus so every measured run injects a distinct block —
// an unmatched query returns the same recency top-3 and the runner's
// identical-block throttle would (correctly) suppress it, measuring nothing.
const (
	HookUserPromptBudget = 150 * time.Millisecond
	HookPreCompactBudget = 200 * time.Millisecond
	HookSessionEndBudget = 500 * time.Millisecond

	HookSeedFacts    = 10000
	HookWarmupRuns   = 4
	HookMeasuredRuns = 20
)

// HookLatencyRow is one event's measured row.
type HookLatencyRow struct {
	Event     string  `json:"event"`
	P99Ms     float64 `json:"p99_ms"`
	MaxMs     float64 `json:"max_ms"`
	WallMaxMs float64 `json:"wall_max_ms"`
	BudgetMs  float64 `json:"budget_ms"`
	Pass      bool    `json:"pass"`
}

// HookLatencyReport is the full --hooks result.
type HookLatencyReport struct {
	SeedFacts int              `json:"seed_facts"`
	Runs      int              `json:"measured_runs"`
	Rows      []HookLatencyRow `json:"rows"`
	Pass      bool             `json:"pass"`
}

// HookLatencyParams scales the run. Zero fields take the published defaults;
// tests scale them down to keep the full pipeline verifiable in seconds.
type HookLatencyParams struct {
	// Binary is the graymatter executable the hook samples run against.
	// Empty means os.Executable() — the CLI itself.
	Binary    string
	SeedFacts int
	Warmup    int
	Runs      int
}

func (p HookLatencyParams) withDefaults() HookLatencyParams {
	if p.Binary == "" {
		if exe, err := os.Executable(); err == nil {
			p.Binary = exe
		}
	}
	if p.SeedFacts <= 0 {
		p.SeedFacts = HookSeedFacts
	}
	if p.Warmup <= 0 {
		p.Warmup = HookWarmupRuns
	}
	if p.Runs <= 0 {
		p.Runs = HookMeasuredRuns
	}
	return p
}

// RunHookLatency executes the published hook-latency audit and returns the
// report. An error means the pipeline itself failed (binary missing, hook
// crashed, no injections recorded); budget breaches are reported in the rows
// with Pass=false and reflected in Report.Pass, not as errors — callers print
// and decide.
func RunHookLatency(p HookLatencyParams, stdout io.Writer) (HookLatencyReport, error) {
	p = p.withDefaults()
	report := HookLatencyReport{SeedFacts: p.SeedFacts, Runs: p.Runs}

	if _, err := os.Stat(p.Binary); err != nil {
		return report, fmt.Errorf("hook binary %s: %w", p.Binary, err)
	}

	root, err := os.MkdirTemp("", "graymatter-bench-hooks-")
	if err != nil {
		return report, fmt.Errorf("temp dir: %w", err)
	}

	storeDir := filepath.Join(root, "store")
	workDir := filepath.Join(root, benchHookAgent) // basename → the seeded agent id
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return report, fmt.Errorf("work dir: %w", err)
	}

	defer func() {
		// The samples run against the store daemon, which spawns from the
		// binary under test and outlives them. Stop it so the temp root —
		// and the binary on Windows, which locks executing files — is
		// releasable. Best-effort: no daemon is also fine.
		stop := exec.Command(p.Binary, "--dir", storeDir, "daemon", "stop")
		stop.Stdout = io.Discard
		stop.Stderr = io.Discard
		_ = stop.Run()
		time.Sleep(500 * time.Millisecond) // the daemon's shutdown reply lands first, then it exits
		_ = os.RemoveAll(root)
	}()

	if err := seedHookStore(storeDir, p.SeedFacts); err != nil {
		return report, fmt.Errorf("seed: %w", err)
	}

	runHook := func(event, payload string) (internal, wall time.Duration, errOut string) {
		start := time.Now()
		out, err := execHookSample(p.Binary, workDir, storeDir, event, payload)
		wall = time.Since(start)
		if err != nil {
			return 0, 0, fmt.Sprintf("%s: %v: %s", event, err, out)
		}
		ms, perr := lastHookInternalMs(storeDir)
		if perr != nil {
			return 0, 0, fmt.Sprintf("%s: hooks.log: %v", event, perr)
		}
		if ms < 0 {
			return 0, 0, fmt.Sprintf("%s: hooks.log carried no ms field", event)
		}
		return ms, wall, ""
	}

	for i := 0; i < p.Warmup; i++ {
		if _, _, errOut := runHook("user-prompt", hookSamplePayload(workDir, fmt.Sprintf("warm-up runbook subsystem %d", i))); errOut != "" {
			return report, fmt.Errorf("warm-up: %s", errOut)
		}
	}

	samples := map[string][]time.Duration{}
	walls := map[string][]time.Duration{}
	for i := 0; i < p.Runs; i++ {
		for _, e := range []struct {
			event   string
			payload string
		}{
			{"user-prompt", hookSamplePayload(workDir, fmt.Sprintf("runbook %d subsystem review cycle", i))},
			{"pre-compact", hookSamplePayload(workDir, "")},
			{"session-end", hookSamplePayload(workDir, "")},
		} {
			internal, wall, errOut := runHook(e.event, e.payload)
			if errOut != "" {
				return report, fmt.Errorf("%s", errOut)
			}
			samples[e.event] = append(samples[e.event], internal)
			walls[e.event] = append(walls[e.event], wall)
		}
	}

	injected, err := countHookInjections(storeDir)
	if err != nil {
		return report, fmt.Errorf("verify injections: %w", err)
	}
	if injected == 0 {
		return report, fmt.Errorf("no user-prompt run injected a memory block; the corpus and queries disagree")
	}

	for _, b := range []struct {
		event  string
		budget time.Duration
	}{
		{"user-prompt", HookUserPromptBudget},
		{"pre-compact", HookPreCompactBudget},
		{"session-end", HookSessionEndBudget},
	} {
		ss := samples[b.event]
		row := HookLatencyRow{
			Event:     b.event,
			P99Ms:     msDuration(percentileDuration(ss, 0.99)),
			MaxMs:     msDuration(maxDuration(ss)),
			WallMaxMs: msDuration(maxDuration(walls[b.event])),
			BudgetMs:  msDuration(b.budget),
		}
		row.Pass = row.P99Ms <= row.BudgetMs && row.MaxMs <= row.BudgetMs
		report.Rows = append(report.Rows, row)
	}
	report.Pass = breaches(report.Rows) == 0 && len(report.Rows) > 0

	fmt.Fprintf(stdout, "hook latency: %d facts · %d warm-up + %d measured process runs per event\n\n", p.SeedFacts, p.Warmup, p.Runs)
	for _, row := range report.Rows {
		status := "ok"
		if !row.Pass {
			status = "FAIL"
		}
		fmt.Fprintf(stdout, "  %-12s internal p99 %7.1fms · max %7.1fms · wall max %7.1fms · budget %v · %s\n",
			row.Event, row.P99Ms, row.MaxMs, row.WallMaxMs, time.Duration(row.BudgetMs*float64(time.Millisecond)), status)
	}
	if !report.Pass {
		fmt.Fprintf(stdout, "\n%d budget(s) breached\n", breaches(report.Rows))
	} else {
		fmt.Fprintf(stdout, "\nall hook budgets hold\n")
	}
	return report, nil
}

func breaches(rows []HookLatencyRow) int {
	n := 0
	for _, r := range rows {
		if !r.Pass {
			n++
		}
	}
	return n
}

const benchHookAgent = "hookbench"

// execHookSample runs one hook event as a fresh process, stdin JSON from
// Claude Code's contract, output drained.
func execHookSample(binary, workDir, storeDir, event, payload string) (string, error) {
	cmd := exec.Command(binary, "--dir", storeDir, "hooks", "run", event)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// hookSamplePayload is the stdin JSON Claude Code sends per event.
func hookSamplePayload(workDir, prompt string) string {
	if prompt != "" {
		return fmt.Sprintf(`{"session_id":"bench","cwd":%q,"hook_event_name":"UserPromptSubmit","prompt":%q}`, workDir, prompt)
	}
	return fmt.Sprintf(`{"session_id":"bench","cwd":%q,"hook_event_name":"SessionEnd"}`, workDir)
}

// seedHookStore plants n facts through the library, keyword embedder, no
// background churn — the same corpus shape the CI gate uses.
func seedHookStore(dir string, n int) error {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	cfg.VectorReconcileInterval = 0
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = mem.Close() }()

	ctx := context.Background()
	topics := []string{"deploy", "database", "cache", "auth", "billing", "search", "queue", "logging", "metrics", "oncall"}
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("Fact %d: the %s subsystem follows runbook %d and was last reviewed on cycle %d",
			i, topics[i%len(topics)], i%97, i%13)
		if err := mem.Remember(ctx, benchHookAgent, text); err != nil {
			return err
		}
	}
	return nil
}

// lastHookInternalMs reads the hook's own timing from the last hooks.log
// line. A missing or unparseable "ms" field is an error — never a
// zero-duration sample, which would flatter the measurement.
func lastHookInternalMs(storeDir string) (time.Duration, error) {
	f, err := os.Open(filepath.Join(storeDir, "hooks.log"))
	if err != nil {
		return -1, err
	}
	defer func() { _ = f.Close() }()

	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			last = line
		}
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	if last == "" {
		return -1, fmt.Errorf("log is empty")
	}
	var entry struct {
		Ms *float64 `json:"ms"`
	}
	if err := json.Unmarshal([]byte(last), &entry); err != nil {
		return -1, err
	}
	if entry.Ms == nil {
		return -1, fmt.Errorf("log line carries no ms field: %s", last)
	}
	return time.Duration(*entry.Ms * float64(time.Millisecond)), nil
}

// countHookInjections counts user-prompt runs that actually injected, from
// hooks.log — the guard against silently measuring the throttle.
func countHookInjections(storeDir string) (int, error) {
	f, err := os.Open(filepath.Join(storeDir, "hooks.log"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		var entry struct {
			Event  string `json:"event"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Event == "user-prompt" && entry.Detail == "injected" {
			count++
		}
	}
	return count, sc.Err()
}

func percentileDuration(durs []time.Duration, p float64) time.Duration {
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1)*p + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func maxDuration(durs []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range durs {
		if d > m {
			m = d
		}
	}
	return m
}

func msDuration(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
