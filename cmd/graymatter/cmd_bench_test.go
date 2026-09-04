package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/benchsyn"
)

// TestBench_SyntheticJSON executes the real cobra command and asserts the
// machine-readable payload: four published session counts, sane fields, and a
// reduction column consistent with the rows. The numbers themselves are gated
// against the published tables by benchmarks/token_count/main_test.go — that
// test owns "are these figures true", this one owns "is the output usable".
func TestBench_SyntheticJSON(t *testing.T) {
	cmd := benchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	jsonOut = true
	t.Cleanup(func() { jsonOut = false })

	if err := cmd.Execute(); err != nil {
		t.Fatalf("bench --json: %v", err)
	}

	var payload struct {
		Suite         string  `json:"suite"`
		Query         string  `json:"query"`
		TopK          int     `json:"top_k"`
		TokensPerWord float64 `json:"tokens_per_word"`
		DurationMS    int64   `json:"duration_ms"`
		Results       []struct {
			Sessions     int     `json:"sessions"`
			FullTokens   int     `json:"full_tokens"`
			RecallTokens int     `json:"recall_tokens"`
			ReductionPct float64 `json:"reduction_pct"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}

	if payload.Suite != "token-count" {
		t.Errorf("suite = %q, want token-count", payload.Suite)
	}
	if payload.TopK != 8 || payload.TokensPerWord != 1.33 {
		t.Errorf("top_k=%d tokens_per_word=%v, want 8 / 1.33", payload.TopK, payload.TokensPerWord)
	}
	if len(payload.Results) != len(benchsyn.SessionCounts) {
		t.Fatalf("got %d rows, want %d", len(payload.Results), len(benchsyn.SessionCounts))
	}

	for i, r := range payload.Results {
		wantSessions := benchsyn.SessionCounts[i]
		if r.Sessions != wantSessions {
			t.Errorf("row %d sessions = %d, want %d", i, r.Sessions, wantSessions)
		}
		if r.Sessions == 1 {
			continue // no history to compress yet; reduction is legitimately ~0
		}
		if r.RecallTokens >= r.FullTokens {
			t.Errorf("%d sessions: recall %d not smaller than full injection %d",
				r.Sessions, r.RecallTokens, r.FullTokens)
		}
		if r.ReductionPct <= 0 || r.ReductionPct > 100 {
			t.Errorf("%d sessions: reduction %v outside (0,100]", r.Sessions, r.ReductionPct)
		}
	}
}

// TestBench_SyntheticHuman checks the human path renders the report header and
// stays quiet about JSON.
func TestBench_SyntheticHuman(t *testing.T) {
	cmd := benchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("bench: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "GrayMatter Token Efficiency Benchmark") {
		t.Error("human output missing report title")
	}
	if !strings.Contains(got, "Reduction") {
		t.Error("human output missing table header")
	}
	if !strings.HasPrefix(got, "\n") {
		t.Errorf("renderer must start with the same leading newline as the historical output")
	}
}

func TestBenchHooks_HelpDescribesRealGates(t *testing.T) {
	help := strings.Join(strings.Fields(benchCmd().Long), " ")
	for _, want := range []string{
		"median deltas",
		"budgets ≤ 200ms and ≤ 200ms",
		"normalized recall scaling (≤ 2.5x)",
		"Pre-compact is the baseline and has no absolute gate",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q", want)
		}
	}
	for _, stale := range []string{"user-prompt <", "pre-compact <", "session-end <"} {
		if strings.Contains(help, stale) {
			t.Errorf("help still advertises stale absolute gate %q", stale)
		}
	}
}

func TestBenchHooks_OutputAndExitContract(t *testing.T) {
	report := benchsyn.HookLatencyReport{SeedFacts: 42, Pass: false}
	run := func(_ benchsyn.HookLatencyParams, out io.Writer) (benchsyn.HookLatencyReport, error) {
		_, _ = io.WriteString(out, "human report\n")
		return report, nil
	}

	t.Run("json is the only stdout document", func(t *testing.T) {
		jsonOut = true
		defer func() { jsonOut = false }()
		var out bytes.Buffer
		cmd := benchCmd()
		cmd.SetOut(&out)
		exitCode := 0
		if err := runBenchHooksWith(cmd, run, func(code int) { exitCode = code }); err != nil {
			t.Fatalf("run bench hooks: %v", err)
		}
		var got benchsyn.HookLatencyReport
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
		}
		if got.SeedFacts != report.SeedFacts || got.Pass {
			t.Fatalf("decoded report = %+v, want fabricated failed report", got)
		}
		if exitCode != 1 {
			t.Fatalf("exit code = %d, want 1", exitCode)
		}
	})

	t.Run("human report is unchanged", func(t *testing.T) {
		var out bytes.Buffer
		cmd := benchCmd()
		cmd.SetOut(&out)
		exitCode := 0
		if err := runBenchHooksWith(cmd, run, func(code int) { exitCode = code }); err != nil {
			t.Fatalf("run bench hooks: %v", err)
		}
		if got := out.String(); got != "human report\n" {
			t.Fatalf("human stdout = %q", got)
		}
		if exitCode != 1 {
			t.Fatalf("exit code = %d, want 1", exitCode)
		}
	})
}
