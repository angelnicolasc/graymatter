// hook_latency gates the Claude Code hook budgets against the real compiled
// binary doing the real work:
//
//	user-prompt  < 150 ms   (the per-turn injection path; the hot path)
//	session-end  < 500 ms   (checkpoint + detached consolidate spawn)
//	pre-compact  < 200 ms   (deterministic checkpoint)
//
// Two clocks are recorded per run and both are printed:
//
//   - internal: what the hook itself reports to hooks.log — store connect +
//     recall/checkpoint/spawn. This is the part GrayMatter controls, and the
//     part the budgets gate.
//   - wall: process start to exit, including the OS spawn cost of a fresh
//     process per event (the shape Claude Code fires). Reported for
//     transparency; dominated by the platform's process-creation cost, which
//     is not GrayMatter's to optimise.
//
// The store is seeded with 10,000 facts in-process (library level, one
// process), then the hook runner executes as a fresh OS process per sample —
// the same shape Claude Code fires it, through the daemon the hooks actually
// use. Warm-up runs absorb the daemon spawn and binary page-in; none count
// toward the measured numbers.
//
// The queries deliberately overlap the seeded corpus so every measured run
// produces a distinct injected block — an unmatched query returns the same
// recency top-3 every time, and the runner's identical-block throttle would
// (correctly) suppress it, measuring nothing.
//
// Usage:
//
//	go test ./benchmarks/hook_latency/   # the CI gate
//	go run  ./benchmarks/hook_latency    # human-readable report
package main

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

const (
	seedFacts     = 10000
	warmupSamples = 4
	measuredRuns  = 20

	// The budgets from the hardening playbook, mirrored in
	// cmd/graymatter/hooks_run.go's hookLatency* constants. They gate the
	// hook-internal time (see package comment).
	userPromptBudget = 150 * time.Millisecond
	sessionEndBudget = 500 * time.Millisecond
	preCompactBudget = 200 * time.Millisecond

	// benchAgent is the basename of the working directory the hook runs
	// from, so deriveAgentID resolves to exactly the seeded agent.
	benchAgent = "hookbench"
)

type sample struct {
	event    string
	internal time.Duration // the hook's own clock (hooks.log "ms")
	wall     time.Duration // process start to exit
}

// binaryPath is the built benchmark binary, set once run() has it; the
// daemon-stop cleanup reads it.
var binaryPath string

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "hook_latency: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole benchmark; main and the CI test both drive it.
func run(stdout io.Writer) error {
	root, err := os.MkdirTemp("", "graymatter-hook-latency-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}

	storeDir := filepath.Join(root, "store")
	workDir := filepath.Join(root, benchAgent) // basename → the seeded agent id
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("work dir: %w", err)
	}

	// The samples run against the store daemon, which spawns from the built
	// binary and outlives them. Stop it so the temp root is releasable
	// (Windows locks executing files). Best-effort.
	defer func() {
		stop := exec.Command(binaryPath, "--dir", storeDir, "daemon", "stop")
		stop.Stdout = io.Discard
		stop.Stderr = io.Discard
		_ = stop.Run()
		time.Sleep(500 * time.Millisecond)
		_ = os.RemoveAll(root)
	}()

	if err := seedStore(storeDir); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	binary, cleanup, err := buildBinary(root)
	if err != nil {
		return err
	}
	defer cleanup()
	// The daemon-stop cleanup (deferred above) needs the built binary's path;
	// it is only known now, after the build.
	binaryPath = binary

	runHook := func(event, payload string) (sample, string) {
		start := time.Now()
		out, err := execHook(binary, workDir, storeDir, event, payload)
		wall := time.Since(start)
		if err != nil {
			return sample{event: event}, fmt.Sprintf("%s: %v: %s", event, err, out)
		}
		internal, perr := lastHookInternalMs(storeDir)
		if perr != nil {
			return sample{event: event}, fmt.Sprintf("%s: hooks.log: %v", event, perr)
		}
		if internal < 0 {
			return sample{event: event}, fmt.Sprintf("%s: hooks.log carried no ms field", event)
		}
		return sample{event: event, internal: internal, wall: wall}, ""
	}

	// Warm-up: the first user-prompt run pays the daemon spawn; the rest
	// absorb binary page-in and FS cache.
	for i := 0; i < warmupSamples; i++ {
		if _, errOut := runHook("user-prompt", hookPayload(workDir, fmt.Sprintf("warm-up runbook subsystem %d", i))); errOut != "" {
			return fmt.Errorf("warm-up: %s", errOut)
		}
	}

	results := map[string][]sample{}
	for i := 0; i < measuredRuns; i++ {
		// A distinct corpus-matching prompt per run: the runner suppresses
		// identical consecutive injections, which would measure the throttle,
		// not recall.
		events := []struct {
			event   string
			payload string
		}{
			{"user-prompt", hookPayload(workDir, fmt.Sprintf("runbook %d subsystem review cycle", i))},
			{"pre-compact", hookPayload(workDir, "")},
			{"session-end", hookPayload(workDir, "")},
		}
		for _, e := range events {
			s, errOut := runHook(e.event, e.payload)
			if errOut != "" {
				return fmt.Errorf("%s", errOut)
			}
			results[e.event] = append(results[e.event], s)
		}
	}

	fmt.Fprintf(stdout, "store: %d facts · %d warm-up + %d measured process runs per event\n\n",
		seedFacts, warmupSamples, measuredRuns)

	// The user-prompt run must actually inject — a benchmark that silently
	// measures the throttle is worse than no benchmark.
	if got, err := injectedBlockCount(storeDir); err != nil {
		return fmt.Errorf("verify injections: %w", err)
	} else if got == 0 {
		return fmt.Errorf("no user-prompt run injected a memory block; the benchmark corpus and queries disagree")
	}

	fails := 0
	for _, b := range []struct {
		name   string
		budget time.Duration
		ss     []sample
	}{
		{"user-prompt", userPromptBudget, results["user-prompt"]},
		{"pre-compact", preCompactBudget, results["pre-compact"]},
		{"session-end", sessionEndBudget, results["session-end"]},
	} {
		p99 := percentile(internalDurations(b.ss), 0.99)
		max := maxOf(internalDurations(b.ss))
		wallMax := maxOf(wallDurations(b.ss))
		status := "ok"
		if p99 > b.budget || max > b.budget {
			status = "FAIL"
			fails++
		}
		fmt.Fprintf(stdout, "  %-12s internal p99 %7.1fms · max %7.1fms · wall max %7.1fms · budget %v · %s\n",
			b.name, ms(p99), ms(max), ms(wallMax), b.budget, status)
	}

	if fails > 0 {
		return fmt.Errorf("%d hook budget(s) breached", fails)
	}
	fmt.Fprintf(stdout, "\nall hook budgets hold\n")
	return nil
}

// execHook runs the hook runner as one fresh process with the benchmark store
// as its data dir, the way Claude Code invokes it (stdin JSON, output drained).
func execHook(binary, workDir, storeDir, event, payload string) (string, error) {
	cmd := exec.Command(binary, "--dir", storeDir, "hooks", "run", event)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// hookPayload is the stdin JSON Claude Code sends for each event.
func hookPayload(workDir, prompt string) string {
	if prompt != "" {
		return fmt.Sprintf(`{"session_id":"bench","cwd":%q,"hook_event_name":"UserPromptSubmit","prompt":%q}`,
			workDir, prompt)
	}
	return fmt.Sprintf(`{"session_id":"bench","cwd":%q,"hook_event_name":"SessionEnd"}`, workDir)
}

// lastHookInternalMs reads the hook's own timing for the last logged event.
// Every runner appends one JSON line with an "ms" field; internal < 0 means
// the line carried none.
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
		Ms float64 `json:"ms"`
	}
	if err := json.Unmarshal([]byte(last), &entry); err != nil {
		return -1, err
	}
	return time.Duration(entry.Ms * float64(time.Millisecond)), nil
}

// injectedBlockCount counts user-prompt runs that actually produced an
// injected block, from the hook log.
func injectedBlockCount(storeDir string) (int, error) {
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

// internalDurations extracts the hook-internal timing series.
func internalDurations(ss []sample) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = s.internal
	}
	return out
}

// wallDurations extracts the end-to-end timing series.
func wallDurations(ss []sample) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = s.wall
	}
	return out
}

func percentile(durs []time.Duration, p float64) time.Duration {
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

func maxOf(durs []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range durs {
		if d > m {
			m = d
		}
	}
	return m
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// buildBinary compiles the current tree; the cleanup removes the artifact.
func buildBinary(dir string) (string, func(), error) {
	bin := filepath.Join(dir, "graymatter-hooklatency.bin")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/angelnicolasc/graymatter/cmd/graymatter").CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("build binary: %v: %s", err, out)
	}
	return bin, func() { _ = os.Remove(bin) }, nil
}

// seedStore plants seedFacts through the library (one process, no daemon),
// with per-fact distinct texts so recall exercises real keyword scoring over
// a realistic corpus.
func seedStore(dir string) error {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	cfg.VectorReconcileInterval = 0 // no background churn during measurement
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = mem.Close() }()

	ctx := context.Background()
	topics := []string{"deploy", "database", "cache", "auth", "billing", "search", "queue", "logging", "metrics", "oncall"}
	for i := 0; i < seedFacts; i++ {
		topic := topics[i%len(topics)]
		text := fmt.Sprintf("Fact %d: the %s subsystem follows runbook %d and was last reviewed on cycle %d",
			i, topic, i%97, i%13)
		if err := mem.Remember(ctx, benchAgent, text); err != nil {
			return err
		}
	}
	return nil
}
