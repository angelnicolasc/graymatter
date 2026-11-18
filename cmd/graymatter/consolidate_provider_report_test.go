package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

func TestConsolidationReportingBelowThresholdKGCLI(t *testing.T) {
	binary := buildE2EBinary(t)
	root := t.TempDir()
	const agent = "audit"
	const source = "Synthetic source mentioning an example project."
	const entity = "fixture-only graph entity"
	var messages atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messages.Add(1)
		var req struct {
			MaxTokens int `json:"max_tokens"`
			Messages  []struct {
				Content []struct{ Text string }
			}
		}
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/graph/v1/messages" ||
			r.Header.Get("x-api-key") != "synthetic-kg-key" || req.MaxTokens != 1024 || len(req.Messages) != 1 ||
			len(req.Messages[0].Content) != 1 ||
			!strings.HasPrefix(req.Messages[0].Content[0].Text, "Extract named entities") ||
			!strings.HasSuffix(req.Messages[0].Content[0].Text, source) {
			t.Errorf("unexpected provider traffic: %s %s, max_tokens=%d, decode=%v", r.Method, r.URL.Path, req.MaxTokens, err)
			http.Error(w, "only synthetic entity extraction is enabled", http.StatusBadRequest)
			return
		}
		nodes := `{"nodes":[{"label":"` + entity + `","entity_type":"concept"}],"edges":[]}`
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]string{{"type": "text", "text": nodes}}})
	}))
	defer fixture.Close()

	for _, mode := range []struct {
		name     string
		kg       string
		wantHTTP int32
	}{{"off", "0", 0}, {"on", "1", 1}} {
		t.Run(mode.name, func(t *testing.T) {
			store := filepath.Join(root, mode.name)
			baseEnv := providerURLCLIEnv(root)
			run := func(env []string, args ...string) []byte {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, append([]string{"--dir", store, "--no-daemon"}, args...)...)
				cmd.Dir, cmd.Env = root, env
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Run(); err != nil || stderr.Len() != 0 {
					t.Fatalf("CLI %v: %v, stdout=%q stderr=%q", args, err, &stdout, &stderr)
				}
				return stdout.Bytes()
			}
			run(baseEnv, "remember", agent, source)
			env := append(providerURLCLIEnv(root), "GRAYMATTER_KG="+mode.kg, "GRAYMATTER_KG_LLM_EXTRACT=1",
				"ANTHROPIC_API_KEY=synthetic-kg-key", "ANTHROPIC_BASE_URL="+fixture.URL+"/graph")
			before := messages.Load()
			for cycle := range 2 {
				out := run(env, "consolidate", agent, "--json")
				var envelope consolidationCLIResult
				if err := json.Unmarshal(out, &envelope); err != nil || envelope.Error != nil ||
					envelope.Summary == nil || envelope.Summary.Status != memory.ConsolidationSkipped ||
					envelope.Summary.Reason != "threshold_not_exceeded" || envelope.Summary.Provider != "" {
					t.Fatalf("cycle %d summary report = %s, %v", cycle, out, err)
				}
				if got := messages.Load() - before; got != mode.wantHTTP {
					t.Fatalf("cycle %d provider requests = %d; want %d total", cycle, got, mode.wantHTTP)
				}
			}

			reopened, err := memory.Open(memory.StoreConfig{DataDir: store, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			facts, err := reopened.List(agent)
			if err != nil || len(facts) != 1 || facts[0].Text != source || facts[0].IsSuperseded() {
				t.Fatalf("skip changed the live source fact: %+v, %v", facts, err)
			}
			var watermark string
			err = reopened.DB().View(func(tx *bolt.Tx) error {
				if bucket := tx.Bucket([]byte("kg_extracted")); bucket != nil {
					watermark = string(bucket.Get([]byte(agent + "\x00" + facts[0].ID)))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			graph, err := kg.OpenRead(reopened.DB())
			if mode.kg == "0" {
				if !errors.Is(err, kg.ErrNoGraph) || watermark != "" {
					t.Fatalf("KG-off store graph=%v watermark=%q", err, watermark)
				}
				return
			}
			if err != nil || watermark != fmt.Sprintf("%x", sha256.Sum256([]byte(source))) {
				t.Fatalf("KG-on store graph=%v watermark=%q", err, watermark)
			}
			nodes, err := graph.AllNodes()
			if err != nil || len(nodes) != 1 || nodes[0].Label != entity {
				t.Fatalf("persisted graph nodes=%+v, %v", nodes, err)
			}
		})
	}
}

func TestConsolidationReportingOllamaDirectCommand(t *testing.T) {
	const agent, model = "audit", "audit-ollama-model"
	const summaryText = "Synthetic Ollama response only available from the fixture."
	ctx := context.Background()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir, cfg.EmbeddingMode, cfg.AsyncConsolidate = t.TempDir(), graymatter.EmbeddingKeyword, false
	cfg.AnthropicAPIKey, cfg.ConsolidateLLM, cfg.ConsolidateThreshold = "", "", 2

	seed, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if seed != nil {
			_ = seed.Close()
		}
	})
	for i := range 6 {
		if err := seed.Remember(ctx, agent, fmt.Sprintf("Synthetic Ollama source fact %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	original, err := seed.Advanced().List(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	seed = nil

	selected := make(chan []string, 1)
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			Model, Prompt, Format string
			Stream                bool
		}
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/fixture/api/generate" ||
			req.Model != model || req.Format != "json" || req.Stream || strings.Contains(req.Prompt, summaryText) {
			t.Errorf("unexpected Ollama request: %s %s, model=%q format=%q stream=%v error=%v",
				r.Method, r.URL.Path, req.Model, req.Format, req.Stream, err)
			http.Error(w, "unexpected synthetic request", http.StatusBadRequest)
			return
		}
		var batch []string
		for _, fact := range original {
			if strings.Contains(req.Prompt, fact.ID) && strings.Contains(req.Prompt, fact.Text) {
				batch = append(batch, fact.ID)
			}
		}
		if len(batch) != 3 {
			t.Errorf("Ollama prompt included %d seeded facts, want weakest half of 6", len(batch))
			http.Error(w, "unexpected synthetic batch", http.StatusBadRequest)
			return
		}
		consumed := batch[:2]
		select {
		case selected <- consumed:
		default:
			t.Error("unexpected second Ollama request")
		}
		proposal, _ := json.Marshal(map[string]any{"summary": summaryText, "consumes": consumed})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"response": string(proposal)})
	}))
	defer fixture.Close()

	// Ollama summarization is configured through the Go API. Use the actual
	// CLI command and direct store with that configuration at the open seam.
	cfg.ConsolidateLLM, cfg.OllamaURL, cfg.OllamaConsolidateModel = "ollama", fixture.URL+"/fixture/", model
	open := func(context.Context) (reportingStore, error) {
		mem, err := graymatter.NewWithConfig(cfg)
		if err != nil {
			return nil, err
		}
		return &directStore{mem: mem, store: mem.Advanced()}, nil
	}
	var stdout, stderr bytes.Buffer
	err = runReportingCommandForTest(t, true, true, open, &stdout, &stderr)
	var envelope consolidationCLIResult
	decodeErr := json.Unmarshal(stdout.Bytes(), &envelope)
	if err != nil || decodeErr != nil || envelope.SchemaVersion != 1 || envelope.AgentID != agent ||
		stderr.Len() != 0 || envelope.Error != nil || envelope.Summary == nil {
		t.Fatalf("Ollama command: %v, JSON=%v, stdout=%q, stderr=%q", err, decodeErr, &stdout, &stderr)
	}
	report := envelope.Summary
	if report.Status != memory.ConsolidationApplied || report.Reason != "summary_applied" ||
		report.Provider != "ollama" || report.SummaryFactID == "" || report.SourcesRetired != 2 || requests.Load() != 1 {
		t.Fatalf("Ollama report: %+v, requests=%d", report, requests.Load())
	}
	wantRetired := make(map[string]bool, 2)
	select {
	case chosen := <-selected:
		for _, id := range chosen {
			wantRetired[id] = true
		}
	default:
		t.Fatal("fixture did not record a proposal")
	}
	wantText := make(map[string]string, len(original))
	for _, fact := range original {
		wantText[fact.ID] = fact.Text
	}

	reopened, err := memory.Open(memory.StoreConfig{DataDir: cfg.DataDir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	facts, err := reopened.List(agent)
	if err != nil || len(facts) != len(original)+1 {
		t.Fatalf("reopened store: %d facts, %v", len(facts), err)
	}
	summaries, retired := 0, 0
	for _, fact := range facts {
		if fact.ID == report.SummaryFactID {
			if fact.Text != summaryText || fact.IsSuperseded() {
				t.Fatalf("reported ID does not identify the live fixture summary: %+v", fact)
			}
			summaries++
			continue
		}
		if expected, ok := wantText[fact.ID]; !ok || fact.Text != expected {
			t.Fatalf("unexpected reopened source fact: %+v", fact)
		}
		if wantRetired[fact.ID] {
			if fact.SupersededBy != report.SummaryFactID {
				t.Fatalf("wrong retirement receipt for %+v, summary %q", fact, report.SummaryFactID)
			}
			retired++
		} else if fact.IsSuperseded() {
			t.Fatalf("unselected source was retired: %+v", fact)
		}
	}
	if cycles, sources := reopened.ConsolidationCounters(); summaries != 1 || retired != report.SourcesRetired ||
		cycles != 1 || sources != retired {
		t.Fatalf("reopened summary/receipts/cycles/sources = %d/%d/%d/%d", summaries, retired, cycles, sources)
	}
}

func TestConsolidationReportingInvalidURLDirectCLI(t *testing.T) {
	binary := buildE2EBinary(t)
	root := t.TempDir()
	store := filepath.Join(root, "not-created")
	const canary = "invalid-cli-url-canary"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--dir", store, "--no-daemon", "consolidate", "audit", "--json")
	cmd.Dir = root
	cmd.Env = append(providerURLCLIEnv(root), "ANTHROPIC_API_KEY=synthetic-consolidation-key",
		"ANTHROPIC_BASE_URL=http://dummy:"+canary+"@127.0.0.1:0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil || stderr.Len() != 0 ||
		strings.Contains(stdout.String(), canary) || strings.Contains(stdout.String(), "synthetic-consolidation-key") {
		t.Fatalf("invalid URL CLI: %v, stdout=%q, stderr=%q", err, &stdout, &stderr)
	}
	var envelope consolidationCLIResult
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || strings.Count(stdout.String(), "\n") != 1 ||
		envelope.SchemaVersion != 1 || envelope.AgentID != "audit" || envelope.Summary != nil ||
		envelope.Error == nil || envelope.Error.Code != "config_invalid_url" {
		t.Fatalf("invalid URL diagnostic: JSON=%v, stdout=%s", err, &stdout)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatalf("invalid URL created a store: %v", err)
	}
}
