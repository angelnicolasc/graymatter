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
// Machine-relative gates (methodology identical to the benchmarks/
// hook_latency CI gate): absolute wall budgets are a hardware lottery on
// shared runners — the same tree measured 121 ms p99 user-prompt on the
// reference dev machine and 284-369 ms on Windows CI runners, with the
// spawn+connect baseline alone reaching 192-248 ms. What is gated is the
// code's marginal cost, normalized per machine and per run:
//
//   recall delta     user-prompt p99 − pre-compact p99 (the recall's
//                    marginal cost on this machine this run)
//   session-end delta
//   scaling          in-process Recall(10k) / Recall(500), normalized by
//                    the size ratio so 1.0x is exactly linear
//
// HookUserPromptBudget is the published reference figure for the hot path
// (121 ms p99 on the reference dev machine, no LLM, localhost only). It is
// NOT a gate — absolute budgets are a hardware lottery on shared runners;
// the doctor uses it as a local advisory threshold and the deltas above are
// what the gate enforces.
const (
	HookRecallDeltaBudget     = 200 * time.Millisecond
	HookSessionEndDeltaBudget = 200 * time.Millisecond
	HookScalingMaxNormalized  = 2.5
	HookUserPromptBudget      = 150 * time.Millisecond

	HookSeedFacts    = 10000
	HookWarmupRuns   = 3
	HookMeasuredRuns = 12
)

// HookLatencyRow is one event's measured row (absolute numbers, informational).
type HookLatencyRow struct {
	Event     string  `json:"event"`
	P99Ms     float64 `json:"p99_ms"`
	MaxMs     float64 `json:"max_ms"`
	WallMaxMs float64 `json:"wall_max_ms"`
	DeltaMs   float64 `json:"delta_ms"`
}

// HookLatencyReport is the full --hooks result. Pass reflects the
// machine-relative gates: recall delta, session-end delta, and scaling.
type HookLatencyReport struct {
	SeedFacts            int              `json:"seed_facts"`
	Runs                 int              `json:"measured_runs"`
	Rows                 []HookLatencyRow `json:"rows"`
	RecallDeltaMs        float64          `json:"recall_delta_ms"`
	SessionEndDeltaMs    float64          `json:"session_end_delta_ms"`
	DeltaBudgetMs        float64          `json:"delta_budget_ms"`
	ScalingNormalized    float64          `json:"scaling_normalized"`
	ScalingMaxNormalized float64          `json:"scaling_max_normalized"`
	Pass                 bool             `json:"pass"`
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

	// Scaling gate, measured in-process while this run still owns the store
	// (the samples have exited; the daemon may linger but only holds the
	// write lock, so the scaling store lives in its own directory).
	scalingNormalized, _, err := hookScalingRatio(p.SeedFacts)
	if err != nil {
		return report, fmt.Errorf("scaling: %w", err)
	}
	report.ScalingNormalized = scalingNormalized
	report.ScalingMaxNormalized = HookScalingMaxNormalized

	// Machine-relative deltas against this run's pre-compact baseline: the
	// spawn+connect+checkpoint cost of THIS machine, measured in the same
	// run the recall samples came from. Deltas gate on the MEDIAN — with a
	// dozen samples the p99 is the single worst run, and gating on it
	// measures the runner's noise floor. p99 and max stay reported.
	preCompactMedian := percentileDuration(samples["pre-compact"], 0.5)
	breaches := 0
	for _, e := range []string{"user-prompt", "pre-compact", "session-end"} {
		ss := samples[e]
		row := HookLatencyRow{
			Event:     e,
			P99Ms:     msDuration(percentileDuration(ss, 0.99)),
			MaxMs:     msDuration(maxDuration(ss)),
			WallMaxMs: msDuration(maxDuration(walls[e])),
			DeltaMs:   msDuration(percentileDuration(ss, 0.5) - preCompactMedian),
		}
		report.Rows = append(report.Rows, row)
	}
	if len(report.Rows) != 3 {
		return report, fmt.Errorf("expected 3 event rows, got %d", len(report.Rows))
	}
	report.RecallDeltaMs = report.Rows[0].DeltaMs
	report.SessionEndDeltaMs = report.Rows[2].DeltaMs
	report.DeltaBudgetMs = msDuration(HookRecallDeltaBudget)
	if report.RecallDeltaMs > report.DeltaBudgetMs {
		breaches++
	}
	if report.SessionEndDeltaMs > msDuration(HookSessionEndDeltaBudget) {
		breaches++
	}
	if scalingNormalized > HookScalingMaxNormalized {
		breaches++
	}
	report.Pass = breaches == 0

	fmt.Fprintf(stdout, "hook latency: %d facts · %d warm-up + %d measured process runs per event\n\n", p.SeedFacts, p.Warmup, p.Runs)
	fmt.Fprintf(stdout, "  %-12s internal p99 %7.1fms (machine baseline: spawn + connect + checkpoint)\n", "baseline", report.Rows[1].P99Ms)
	for _, row := range report.Rows {
		if row.Event == "pre-compact" {
			fmt.Fprintf(stdout, "  %-12s internal p99 %7.1fms · max %7.1fms · wall max %7.1fms\n",
				row.Event, row.P99Ms, row.MaxMs, row.WallMaxMs)
			continue
		}
		fmt.Fprintf(stdout, "  %-12s internal p99 %7.1fms · max %7.1fms · wall max %7.1fms · delta %+7.1fms (budget ≤ %v)\n",
			row.Event, row.P99Ms, row.MaxMs, row.WallMaxMs, row.DeltaMs, HookRecallDeltaBudget)
	}
	fmt.Fprintf(stdout, "  %-12s Recall(10k)/Recall(500) = %.2fx of linear (max %.1fx)\n",
		"scaling", scalingNormalized, HookScalingMaxNormalized)
	if !report.Pass {
		fmt.Fprintf(stdout, "\n%d hook gate(s) breached (gates are machine-relative deltas + scaling)\n", breaches)
	} else {
		fmt.Fprintf(stdout, "\nall hook gates hold\n")
	}
	return report, nil
}

// hookScalingRatio times in-process Recall over two store sizes and returns
// (normalized, raw) ratios. The small store derives from seedFacts (1/20th,
// floor 5) so scaled-down runs keep a meaningful big-over-small relationship
// instead of querying a "big" store smaller than the small one. Each store
// lives in its own temp directory and is opened/closed inside this function.
func hookScalingRatio(seedFacts int) (float64, float64, error) {
	open := func(dataDir string, n int) (*graymatter.Memory, error) {
		cfg := graymatter.DefaultConfig()
		cfg.DataDir = dataDir
		cfg.VectorReconcileInterval = 0
		cfg.AsyncConsolidate = false
		mem, err := graymatter.NewWithConfig(cfg)
		if err != nil {
			return nil, err
		}
		ctx := context.Background()
		for i := 0; i < n; i++ {
			if err := mem.Remember(ctx, benchHookAgent, fmt.Sprintf("scaling fact %d: the %s subsystem follows runbook %d", i, scalingTopic(i), i%97)); err != nil {
				_ = mem.Close()
				return nil, err
			}
		}
		return mem, nil
	}
	bestOf := func(mem *graymatter.Memory, runs int) (time.Duration, error) {
		ctx := context.Background()
		var best time.Duration
		for i := 0; i < runs; i++ {
			start := time.Now()
			if _, err := mem.Recall(ctx, benchHookAgent, fmt.Sprintf("runbook %d subsystem review cycle %d", i, i)); err != nil {
				return 0, err
			}
			if d := time.Since(start); best == 0 || d < best {
				best = d
			}
		}
		return best, nil
	}

	smallDir, err := os.MkdirTemp("", "graymatter-scaling-small-")
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = os.RemoveAll(smallDir) }()

	smallFacts := seedFacts / 20
	if smallFacts < 5 {
		smallFacts = 5
	}
	small, err := open(smallDir, smallFacts)
	if err != nil {
		return 0, 0, err
	}
	smallBest, err := bestOf(small, 3)
	_ = small.Close()
	if err != nil {
		return 0, 0, err
	}

	bigDir, err := os.MkdirTemp("", "graymatter-scaling-big-")
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = os.RemoveAll(bigDir) }()
	big, err := open(bigDir, seedFacts)
	if err != nil {
		return 0, 0, err
	}
	bigBest, err := bestOf(big, 3)
	_ = big.Close()
	if err != nil {
		return 0, 0, err
	}
	if smallBest <= 0 {
		return 0, 0, fmt.Errorf("small recall measured %v", smallBest)
	}
	raw := float64(bigBest) / float64(smallBest)
	return raw / (float64(seedFacts) / float64(smallFacts)), raw, nil
}

func scalingTopic(i int) string {
	topics := []string{"deploy", "database", "cache", "auth", "billing", "search", "queue", "logging", "metrics", "oncall"}
	return topics[i%len(topics)]
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
