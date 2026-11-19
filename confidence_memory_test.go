package graymatter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestMemoryConfidenceOptionsAndNamespaces(t *testing.T) {
	m, err := NewWithConfig(Config{DataDir: t.TempDir(), EmbeddingMode: EmbeddingKeyword, TopK: 1, CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	verified, unverified := "verified", "unverified"
	a, err := m.RememberWithOptions(ctx, "a", "deployment approved", memory.WriteOptions{Confidence: &verified})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RememberWithOptions(ctx, "a", "deployment rumored", memory.WriteOptions{Confidence: &unverified}); err != nil {
		t.Fatal(err)
	}
	s, err := m.RememberSharedWithOptions(ctx, "deployment global policy", memory.WriteOptions{Confidence: &verified})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == s.ID || a.Confidence != verified || s.AgentID != memory.SharedAgentID {
		t.Fatalf("invalid receipts: %+v / %+v", a, s)
	}
	zero := 0.0
	opts := memory.RecallOptions{MinConfidence: &verified, ConfidenceWeight: &zero}
	r, err := m.RecallWithOptions(ctx, "a", "deployment", opts)
	if err != nil || len(r.Facts) != 1 || r.Facts[0] != a.Text || r.Retrieval == nil {
		t.Fatalf("agent options: %+v %v", r, err)
	}
	e, err := m.RecallExplainWithOptions(ctx, "a", "deployment", opts)
	if err != nil || len(e.Receipts) != 1 || e.Receipts[0].Provenance.FactID != a.ID || e.Retrieval == nil {
		t.Fatalf("explain options: %+v %v", e, err)
	}
	r, err = m.RecallSharedWithOptions(ctx, "deployment", opts)
	if err != nil || len(r.Facts) != 1 || r.Facts[0] != s.Text {
		t.Fatalf("shared options: %+v %v", r, err)
	}
	r, err = m.RecallAllWithOptions(ctx, "a", "deployment", opts)
	if err != nil || len(r.Facts) != 1 || r.Retrieval == nil {
		t.Fatalf("all options: %+v %v", r, err)
	}
	bad := " verified"
	if _, err := m.RememberWithOptions(ctx, "a", "invalid", memory.WriteOptions{Confidence: &bad}); err == nil {
		t.Fatal("invalid write accepted")
	}
	facts, _ := m.Advanced().List("a")
	if len(facts) != 2 {
		t.Fatal("invalid write had effects")
	}
}

func confidenceAnthropicMessage(text string) string {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	return fmt.Sprintf(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-test","content":%s,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, content)
}

func TestMemoryRememberExtractedConfidenceFollowsProvenance(t *testing.T) {
	for _, tc := range []struct{ name, key, response, want string }{
		{"offline", "", "", ""},
		{"generated identical text", "test-key", `["source"]`, "unverified"},
		{"malformed model fallback", "test-key", `not JSON`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.key != "" {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, confidenceAnthropicMessage(tc.response))
				}))
				defer srv.Close()
				t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
			}
			m, err := NewWithConfig(Config{DataDir: t.TempDir(), EmbeddingMode: EmbeddingKeyword, AnthropicAPIKey: tc.key, ConsolidateModel: "claude-test"})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.RememberExtracted(context.Background(), "a", "source"); err != nil {
				t.Fatal(err)
			}
			facts, err := m.Advanced().List("a")
			if err != nil || len(facts) != 1 || facts[0].Text != "source" || facts[0].Confidence != tc.want {
				t.Fatalf("stored extraction: %+v %v", facts, err)
			}
		})
	}
}

func TestMemoryRememberWithOptionsSchedulesSafeMaintenanceAfterCommit(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, confidenceAnthropicMessage("This text must never be published."))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	cfg := Config{
		DataDir: t.TempDir(), EmbeddingMode: EmbeddingKeyword, AsyncConsolidate: true,
		ConsolidateThreshold: 2, ConsolidateLLM: "anthropic", AnthropicAPIKey: "test-key", ConsolidateModel: "claude-test",
	}
	m, err := NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	for _, text := range []string{"source one", "source two"} {
		if err := m.Advanced().Put(ctx, "a", text); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := m.Advanced().List("a")
	if err != nil || len(sources) != 2 {
		t.Fatalf("list sources: %+v %v", sources, err)
	}
	stale := sources[0]
	stale.AccessedAt = time.Now().Add(-31 * 24 * time.Hour)
	if err := m.Advanced().UpdateFact("a", stale); err != nil {
		t.Fatal(err)
	}

	label := "unverified"
	f, err := m.RememberWithOptions(ctx, "a", "final atomic label", memory.WriteOptions{Confidence: &label})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	facts, err := reopened.Advanced().List("a")
	if err != nil || len(facts) != 3 {
		t.Fatalf("maintenance must preserve all three facts: %+v %v", facts, err)
	}
	var maintained, committed bool
	for _, stored := range facts {
		if stored.IsSuperseded() {
			t.Fatalf("maintenance retired %s", stored.ID)
		}
		if stored.ID == stale.ID {
			maintained = stored.Weight > 0 && stored.Weight < stale.Weight
		}
		if stored.ID == f.ID {
			committed = stored.Confidence == label
		}
	}
	if !maintained || !committed || requests.Load() != 0 {
		t.Fatalf("maintenance=%t committed confidence=%t provider requests=%d", maintained, committed, requests.Load())
	}
}

func TestMemoryConfidenceOptionsRejectNoOpReceipt(t *testing.T) {
	m := New("invalid\x00path")
	if _, err := m.RememberWithOptions(context.Background(), "a", "source", memory.WriteOptions{}); err == nil {
		t.Fatal("unavailable store fabricated durable write")
	}
	if _, err := m.RecallWithOptions(context.Background(), "a", "source", memory.RecallOptions{}); err == nil {
		t.Fatal("unavailable store fabricated policy result")
	}
}
