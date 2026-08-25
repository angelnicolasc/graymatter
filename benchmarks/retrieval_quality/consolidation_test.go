package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The consolidation quality gate (hardening playbook W3, test plan item 5):
// consolidation must not degrade retrieval of canonical facts on a
// 500-fact store. The summariser under test is a *lossless* local merge —
// every consumed fact's text enters the summary verbatim — so this measures
// the mechanism itself (tombstone receipts + summary replacement), not the
// eloquence of any model. A regression here means the pipeline loses what it
// was given, which no amount of model quality can excuse.
//
// Deterministic: frozen store clock, keyword embedder, scripted HTTP
// summariser. No network, no key, no Ollama install required.

const (
	gateAgent      = "gate-agent"
	canonicalCount = 50
	fillerCount    = 450
	consolidations = 6
)

var promptRowRe = regexp.MustCompile(`^- ([0-9A-Za-z]+): (.*)$`)

type gateRequest struct {
	Prompt string `json:"prompt"`
}

func TestConsolidationGate_CanonicalRecallDoesNotRegress(t *testing.T) {
	// Scripted lossless summariser: consume everything shown, echo every
	// shown text into one paragraph.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req gateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		var texts []string
		var ids []string
		for _, line := range strings.Split(req.Prompt, "\n") {
			if m := promptRowRe.FindStringSubmatch(line); m != nil {
				ids = append(ids, m[1])
				texts = append(texts, m[2])
			}
		}
		if len(ids) == 0 {
			t.Errorf("summariser received no parsable rows:\n%s", req.Prompt)
		}
		summary := "consolidated digest: " + strings.Join(texts, " | ")
		prop, _ := json.Marshal(map[string]any{"summary": summary, "consumes": ids})
		env, _ := json.Marshal(map[string]any{"response": string(prop)})
		_, _ = w.Write(env)
	}))
	defer srv.Close()

	dir := t.TempDir()
	s, err := memory.Open(memory.StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	type canon struct{ text, marker string }
	canonicals := make([]canon, 0, canonicalCount)
	for i := 0; i < fillerCount; i++ {
		txt := fmt.Sprintf("session note %d: reviewed dashboard metrics and filed routine follow-ups", i)
		if err := s.Put(ctx, gateAgent, txt); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < canonicalCount; i++ {
		marker := fmt.Sprintf("QEDX-%04d", i)
		txt := fmt.Sprintf("binding decision %s: deploy %s only through the signed release channel", marker, marker)
		canonicals = append(canonicals, canon{text: txt, marker: marker})
		if err := s.Put(ctx, gateAgent, txt); err != nil {
			t.Fatal(err)
		}
	}

	recallHits := func() float64 {
		hits := 0
		for _, c := range canonicals {
			res, err := s.Recall(ctx, gateAgent, c.marker, 8)
			if err != nil {
				t.Fatalf("recall %s: %v", c.marker, err)
			}
			for _, got := range res {
				if strings.Contains(got, c.marker) {
					hits++
					break
				}
			}
		}
		return float64(hits) / float64(len(canonicals))
	}

	before := recallHits()

	cfg := &gateConsolidateCfg{
		llm:       "ollama",
		ollamaURL: srv.URL,
		threshold: 40,
	}
	for cycle := 0; cycle < consolidations; cycle++ {
		if err := s.Consolidate(ctx, gateAgent, cfg); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}

	after := recallHits()

	cycles, _ := s.ConsolidationCounters()
	if cycles == 0 {
		t.Fatal("gate ran no consolidations; nothing was measured")
	}
	t.Logf("canonical recall@8 before=%.3f after=%.3f across %d applied cycles (%d facts seeded)",
		before, after, cycles, canonicalCount+fillerCount)

	if after < before {
		t.Errorf("consolidation regressed canonical recall@8: %.3f -> %.3f", before, after)
	}
}

// gateConsolidateCfg satisfies memory.ConsolidateConfig for the gate.
type gateConsolidateCfg struct {
	llm       string
	ollamaURL string
	threshold int
}

func (c *gateConsolidateCfg) GetAnthropicAPIKey() string        { return "" }
func (c *gateConsolidateCfg) GetConsolidateLLM() string         { return c.llm }
func (c *gateConsolidateCfg) GetConsolidateModel() string       { return "" }
func (c *gateConsolidateCfg) GetConsolidateThreshold() int      { return c.threshold }
func (c *gateConsolidateCfg) GetDecayHalfLife() time.Duration   { return 720 * time.Hour }
func (c *gateConsolidateCfg) GetOllamaURL() string              { return c.ollamaURL }
func (c *gateConsolidateCfg) GetOllamaConsolidateModel() string { return "gate-mock" }
