package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The --embeddings report must be byte-identical across runs over the same
// store (the --health contract), and its verdicts must separate the three
// honest states: healthy vector channel, supported keyword-only, and a
// failing backend that used to hide behind Put's silence.

func openEmbeddingsStore(t *testing.T, p embedding.Provider) *memory.Store {
	t.Helper()
	s, err := memory.Open(memory.StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      p,
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func finding2(t *testing.T, rep embeddingsReport, rule string) embeddingsFinding {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Rule == rule {
			return f
		}
	}
	t.Fatalf("finding %q missing from report %+v", rule, rep)
	return embeddingsFinding{}
}

func TestEmbeddingsReportFailingBackendWarns(t *testing.T) {
	s := openEmbeddingsStore(t, failingCLIEmbedder{err: errors.New("dial tcp: connection refused")})
	for i := 0; i < 2; i++ {
		if err := s.Put(context.Background(), "a", "fact"); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	rep := buildEmbeddingsReport(s)
	if rep.Verdict != "warn" {
		t.Errorf("verdict = %q, want warn", rep.Verdict)
	}
	d := finding2(t, rep, "embed-degradation")
	if d.Status != "warn" {
		t.Errorf("degradation status = %q, want warn", d.Status)
	}
	if rep.DegradedFacts != 2 || rep.LiveFacts != 2 || rep.FactsWithVec != 0 {
		t.Errorf("counters = %+v, want 2 degraded / 2 live / 0 indexed", rep)
	}
}

func TestEmbeddingsReportKeywordOnlyIsInfo(t *testing.T) {
	s := openEmbeddingsStore(t, embedding.NewKeyword())
	if err := s.Put(context.Background(), "a", "keyword fact"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rep := buildEmbeddingsReport(s)
	if rep.Verdict == "fail" {
		t.Errorf("verdict = %q; keyword-only is supported configuration, never failure", rep.Verdict)
	}
	cov := finding2(t, rep, "vector-coverage")
	if cov.Status != "info" {
		t.Errorf("coverage status = %q, want info for keyword-only", cov.Status)
	}
}

func TestEmbeddingsReportHealthyIsOk(t *testing.T) {
	s := openEmbeddingsStore(t, staticCLIEmbedder{})
	if err := s.Put(context.Background(), "a", "vectorised fact"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rep := buildEmbeddingsReport(s)
	if rep.Verdict != "ok" {
		t.Errorf("verdict = %q, want ok", rep.Verdict)
	}
	cov := finding2(t, rep, "vector-coverage")
	if cov.Status != "ok" {
		t.Errorf("coverage status = %q, want ok", cov.Status)
	}
}

func TestEmbeddingsReportJSONByteStableAcrossRuns(t *testing.T) {
	s := openEmbeddingsStore(t, embedding.NewKeyword())
	if err := s.Put(context.Background(), "a", "determinism fact"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	first, err := json.Marshal(buildEmbeddingsReport(s))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(buildEmbeddingsReport(s))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("report is not deterministic:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// Local stubs: the pkg/memory test helpers are not exported on purpose.
type failingCLIEmbedder struct{ err error }

func (f failingCLIEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, f.err
}
func (failingCLIEmbedder) Dimensions() int { return 1024 }
func (failingCLIEmbedder) Name() string    { return "failing-cli-test" }

type staticCLIEmbedder struct{}

func (staticCLIEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.5}, nil
}
func (staticCLIEmbedder) Dimensions() int { return 1 }
func (staticCLIEmbedder) Name() string    { return "static-cli-test" }
