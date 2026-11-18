package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestConsolidationReportIdentifiesCommittedSummaryAndSources(t *testing.T) {
	const summary = "Synthetic summary returned by the reporting fixture."
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/edge/v1/messages" || r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("unexpected Messages request: %s %s", r.Method, r.URL.EscapedPath())
		}
		var input struct {
			Model     string          `json:"model"`
			MaxTokens int             `json:"max_tokens"`
			Messages  json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil ||
			input.Model != "claude-test" || input.MaxTokens != 512 ||
			!strings.Contains(string(input.Messages), "batch fact number") ||
			strings.Contains(string(input.Messages), summary) {
			t.Errorf("unexpected Messages body: model=%q, max_tokens=%d, err=%v", input.Model, input.MaxTokens, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, anthropicTextMessage(summary))
	}))
	defer gateway.Close()
	t.Setenv("ANTHROPIC_BASE_URL", gateway.URL+"/edge")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	ctx := context.Background()
	store := openHookedStore(t, func(_ string, err error) {
		t.Errorf("unexpected legacy callback for successful consolidation: %v", err)
	})
	seedBatch(t, store, "audit")
	before, err := store.List("audit")
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.ConsolidateWithReport(ctx, "audit", anthropicCfg())
	if err != nil || report.Status != ConsolidationApplied || report.Reason != "summary_applied" ||
		report.Provider != "anthropic" || report.SummaryFactID == "" || report.SourcesRetired != 2 || report.Diagnostic != nil {
		t.Fatalf("unexpected consolidation report: %+v, err=%v", report, err)
	}
	if _, _, err := ValidateConsolidationResult(report, nil); err != nil {
		t.Fatalf("core produced an invalid report: %v", err)
	}
	after, err := store.List("audit")
	if err != nil {
		t.Fatal(err)
	}
	beforeByID := make(map[string]string, len(before))
	for _, fact := range before {
		beforeByID[fact.ID] = fact.Text
	}
	committed, retired := 0, 0
	for _, fact := range after {
		if fact.ID == report.SummaryFactID {
			if fact.Text != summary || fact.IsSuperseded() {
				t.Errorf("summary fact does not match returned ID: %+v", fact)
			}
			committed++
			continue
		}
		text, ok := beforeByID[fact.ID]
		if !ok || text != fact.Text {
			t.Errorf("unexpected change to source ID %q", fact.ID)
		}
		if fact.SupersededBy == report.SummaryFactID {
			retired++
		} else if fact.IsSuperseded() {
			t.Errorf("retirement for %q references a different summary", fact.ID)
		}
	}
	if len(after) != len(before)+1 || committed != 1 || retired != report.SourcesRetired || requests.Load() != 1 {
		t.Fatalf("got %d facts, %d committed summary, %d retired sources, %d requests", len(after), committed, retired, requests.Load())
	}
	if cycles, sources := store.ConsolidationCounters(); cycles != 1 || sources != retired {
		t.Fatalf("legacy counters = %d/%d, want 1/%d", cycles, sources, retired)
	}
}

func TestConsolidationReportSkipReasons(t *testing.T) {
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer gateway.Close()
	t.Setenv("ANTHROPIC_BASE_URL", gateway.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	for _, test := range []struct {
		reason    string
		seed      bool
		threshold int
		disabled  bool
		pinAll    bool
	}{
		{reason: "no_facts", threshold: 0},
		{reason: "disabled", seed: true, threshold: 5, disabled: true},
		{reason: "threshold_not_exceeded", seed: true, threshold: 5},
		{reason: "threshold_not_exceeded", seed: true, threshold: 6},
		{reason: "no_eligible_batch", seed: true, threshold: 0, pinAll: true},
	} {
		t.Run(fmt.Sprintf("%s_%d", test.reason, test.threshold), func(t *testing.T) {
			const agent = "audit"
			store := openHookedStore(t, func(_ string, err error) {
				t.Errorf("skip emitted a consolidation callback: %v", err)
			})
			if test.seed {
				seedBatch(t, store, agent)
			}
			if test.pinAll {
				facts, err := store.List(agent)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetPinned(agent, true, facts...); err != nil {
					t.Fatal(err)
				}
			}
			cfg := anthropicCfg()
			cfg.threshold = test.threshold
			if test.disabled {
				cfg.llm = ""
			}
			report, err := store.ConsolidateWithReport(context.Background(), agent, cfg)
			if err != nil || report.Status != ConsolidationSkipped || report.Reason != test.reason ||
				report.Provider != "" || report.SummaryFactID != "" || report.SourcesRetired != 0 || report.Diagnostic != nil {
				t.Fatalf("unexpected skip report: %+v, err=%v", report, err)
			}
			if _, _, err := ValidateConsolidationResult(report, nil); err != nil {
				t.Fatalf("core produced an invalid report: %v", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Errorf("skip cases made %d Messages requests", requests.Load())
	}
}

func TestConsolidationReportProviderFailurePreservesLegacyCallback(t *testing.T) {
	const canary = "synthetic-private-response"
	for _, test := range []struct {
		name             string
		status, wantHTTP int
		body, diagnostic string
		mediaType        string
	}{
		{"unauthorized", http.StatusUnauthorized, http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"synthetic-private-response"}}`, "provider_http_error", "application/json"},
		{"null-message", http.StatusOK, 0, "null", "provider_response_invalid", "application/json"},
		{"empty-response", http.StatusOK, 0, "", "provider_response_invalid", "application/json"},
		{"truncated-response", http.StatusOK, 0, `{"type":"message","content":`, "provider_response_invalid", "application/json"},
		{"non-json-response", http.StatusOK, 0, "<p>synthetic-private-response</p>", "provider_response_invalid", "text/html"},
		{"non-json-error", http.StatusNotFound, http.StatusNotFound, "<p>synthetic-private-response</p>", "provider_http_error", "text/html"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
					t.Errorf("unexpected Messages request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", test.mediaType)
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer gateway.Close()
			t.Setenv("ANTHROPIC_BASE_URL", gateway.URL)
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

			ctx := context.Background()
			var callbacks []error
			store := openHookedStore(t, func(_ string, err error) { callbacks = append(callbacks, err) })
			seedBatch(t, store, "audit")
			before, err := store.List("audit")
			if err != nil {
				t.Fatal(err)
			}
			report, err := store.ConsolidateWithReport(ctx, "audit", anthropicCfg())
			if err != nil || report.Status != ConsolidationFailed || report.Reason != "provider_error" ||
				report.Provider != "anthropic" || report.SummaryFactID != "" || report.SourcesRetired != 0 {
				t.Fatalf("unexpected failure report: %+v, err=%v", report, err)
			}
			d := report.Diagnostic
			if d == nil || d.Code != test.diagnostic || d.Provider != "anthropic" || d.HTTPStatus != test.wantHTTP || strings.Contains(d.Message, canary) {
				t.Fatalf("unexpected failure diagnostic: %+v", d)
			}
			if _, _, err := ValidateConsolidationResult(report, nil); err != nil {
				t.Fatalf("core produced an invalid report: %v", err)
			}
			if len(callbacks) != 1 || callbacks[0] == nil {
				t.Fatalf("reporting call invoked legacy callback %d times, want 1", len(callbacks))
			}

			if err := store.Consolidate(ctx, "audit", anthropicCfg()); err != nil {
				t.Fatalf("legacy provider failure became an operation error: %v", err)
			}
			if len(callbacks) != 2 || callbacks[1] == nil || requests.Load() != 2 {
				t.Fatalf("after both calls: callbacks=%d requests=%d", len(callbacks), requests.Load())
			}
			if test.name == "unauthorized" {
				for i, cause := range callbacks {
					sdkError, ok := cause.(*anthropic.Error)
					if !ok || sdkError.StatusCode != test.wantHTTP {
						t.Fatalf("callback %d lost SDK HTTP cause: %v", i, cause)
					}
				}
			}

			after, err := store.List("audit")
			if err != nil || len(after) != len(before) {
				t.Fatalf("post-failure store contained %d facts, want %d, err=%v", len(after), len(before), err)
			}
			for i, fact := range after {
				if fact.ID != before[i].ID || fact.Text != before[i].Text || fact.IsSuperseded() {
					t.Errorf("provider failure changed source fact %q", fact.ID)
				}
			}
		})
	}
}
