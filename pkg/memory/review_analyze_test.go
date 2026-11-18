package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const analysisTestNamespace = "review-provider-fixture"

func analysisTestFact(t *testing.T, s *Store, namespace, text string) Fact {
	t.Helper()
	fact, err := s.PutReturningFact(context.Background(), namespace, text)
	if err != nil {
		t.Fatal(err)
	}
	return fact
}

func analysisEnvelope(provider, payload, stop string) []byte {
	if provider == "anthropic" {
		if stop == "" {
			stop = "end_turn"
		}
		body, _ := json.Marshal(map[string]any{
			"stop_reason": stop,
			"content":     []map[string]string{{"type": "text", "text": payload}},
		})
		return body
	}
	if stop == "" {
		stop = "stop"
	}
	body, _ := json.Marshal(map[string]any{"done": true, "done_reason": stop, "response": payload})
	return body
}

func analysisTestProvider(t *testing.T, provider string, handler func(reviewInput) (int, []byte)) (ReviewAnalyzeOptions, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected provider request method or content type: %s %q", r.Method, r.Header.Get("Content-Type"))
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read provider request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(raw) > ReviewMaxRequestBytes {
			t.Errorf("provider request is %d bytes", len(raw))
		}
		var envelope struct {
			Model     string          `json:"model"`
			Prompt    string          `json:"prompt"`
			System    string          `json:"system"`
			Format    string          `json:"format"`
			Stream    bool            `json:"stream"`
			Options   map[string]int  `json:"options"`
			Messages  []reviewMessage `json:"messages"`
			MaxTokens int             `json:"max_tokens"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Errorf("decode provider request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if envelope.Model != "test-review-model" || envelope.System != reviewSystemPrompt {
			t.Error("provider did not receive the configured model and fixed review instructions")
		}
		prompt := envelope.Prompt
		if provider == "anthropic" {
			if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-only" ||
				r.Header.Get("anthropic-version") == "" || envelope.MaxTokens != reviewMaxOutputTokens ||
				len(envelope.Messages) != 1 || envelope.Messages[0].Role != "user" {
				t.Error("Anthropic route, credentials, or output bound is incorrect")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			prompt = envelope.Messages[0].Content
		} else if r.URL.Path != "/api/generate" || r.Header.Get("x-api-key") != "" ||
			envelope.Stream || envelope.Format != "json" || envelope.Options["num_predict"] != reviewMaxOutputTokens {
			t.Error("Ollama route, JSON mode, or output bound is incorrect")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var input reviewInput
		if err := json.Unmarshal([]byte(prompt), &input); err != nil {
			t.Errorf("decode review evidence: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status, reply := handler(input)
		w.WriteHeader(status)
		_, _ = w.Write(reply)
	}))
	t.Cleanup(server.Close)
	return ReviewAnalyzeOptions{
		Provider: provider, Model: "test-review-model", Endpoint: server.URL, APIKey: "test-only",
	}, &requests
}

func TestReviewAnalyzeC101C102SharedProviderContract(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		for _, kind := range []string{ReviewKindSummary, ReviewKindConflict, ReviewKindUntyped} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				ctx := context.Background()
				s, closeStore := openTestStore(t)
				defer closeStore()
				first := analysisTestFact(t, s, analysisTestNamespace, "Production timeout is 30 seconds.\nPath: <\\> 界")
				second := analysisTestFact(t, s, analysisTestNamespace, "Production timeout is 90 seconds.")
				analysisTestFact(t, s, "different-namespace", "Unrelated private evidence.")
				before, err := s.List(analysisTestNamespace)
				if err != nil {
					t.Fatal(err)
				}
				expected := []EvidenceSnapshot{NewEvidenceSnapshot(first), NewEvidenceSnapshot(second)}
				slices.SortFunc(expected, func(a, b EvidenceSnapshot) int { return strings.Compare(a.FactID, b.FactID) })
				const hypothesis = "Proposed review hypothesis is unverified."
				const secondNote = "A separate legacy conflict observation."
				opts, requests := analysisTestProvider(t, provider, func(input reviewInput) (int, []byte) {
					if input.Namespace != analysisTestNamespace || input.PromptVersion != reviewPromptVersion ||
						!reflect.DeepEqual(input.Sources, expected) {
						t.Errorf("provider batch is not the exact ordered canonical snapshot: %+v", input)
					}
					var payload string
					switch kind {
					case ReviewKindSummary:
						payload = fmt.Sprintf(`{"summary":%q,"consumes":[%q]}`, hypothesis, first.ID)
					case ReviewKindConflict:
						payload = fmt.Sprintf(`{"summary":"","consumes":[],"conflicts":[{"ids":[%q,%q],"note":%q}]}`, first.ID, second.ID, hypothesis)
					case ReviewKindUntyped:
						payload = fmt.Sprintf(`{"summary":"","consumes":[],"contradictions":[%q,%q]}`, hypothesis, secondNote)
					}
					return http.StatusOK, analysisEnvelope(provider, payload, "")
				})
				records, err := s.ReviewAnalyze(ctx, analysisTestNamespace, opts)
				wantRecords := 1
				if kind == ReviewKindUntyped {
					wantRecords = 2
				}
				if err != nil || len(records) != wantRecords || records[0].Kind != kind || requests.Load() != 1 {
					t.Fatalf("ReviewAnalyze got records %+v, requests %d, error %v", records, requests.Load(), err)
				}
				if kind == ReviewKindUntyped && (records[0].Note != hypothesis || records[1].Note != secondNote || records[0].ID == records[1].ID) {
					t.Fatalf("legacy observations lost distinct identities: %+v", records)
				}
				persisted, err := s.ReviewInspect(ctx, analysisTestNamespace, records[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				stored := make(map[string]EvidenceSnapshot)
				for _, source := range persisted.Sources {
					stored[source.Snapshot.FactID] = source.Snapshot
				}
				for _, source := range persisted.AdditionalEvidence {
					stored[source.FactID] = source
				}
				if len(stored) != len(expected) {
					t.Fatalf("persisted snapshots = %+v", stored)
				}
				for _, source := range expected {
					if got := stored[source.FactID]; !reflect.DeepEqual(got, source) {
						t.Errorf("snapshot changed after journal commit: %+v", got)
					}
				}
				after, err := s.List(analysisTestNamespace)
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("analysis changed active facts: before %+v, after %+v, error %v", before, after, err)
				}
				receipts, err := s.RecallPreview(ctx, analysisTestNamespace, hypothesis, 12)
				if err != nil {
					t.Fatal(err)
				}
				for _, receipt := range receipts {
					if receipt.Text == hypothesis {
						t.Fatal("review hypothesis reached active recall")
					}
				}
			})
		}
	}
}

func TestReviewAnalyzeC103RejectsWholeInvalidResponse(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			s, closeStore := openTestStore(t)
			defer closeStore()
			first := analysisTestFact(t, s, analysisTestNamespace, "Service timeout is 30 seconds.")
			second := analysisTestFact(t, s, analysisTestNamespace, "Service timeout is 90 seconds.")
			foreign := analysisTestFact(t, s, "foreign-namespace", "Service timeout is 120 seconds.")
			valid := fmt.Sprintf(`{"summary":"A candidate","consumes":[%q]}`, first.ID)
			for _, fixture := range []struct{ name, payload, stop string }{
				{"foreign", fmt.Sprintf(`{"summary":"A candidate","consumes":[%q],"conflicts":[{"ids":[%q,%q],"note":"disputed"}]}`, first.ID, first.ID, foreign.ID), ""},
				{"missing", fmt.Sprintf(`{"summary":"A candidate","consumes":[%q,"invented"]}`, first.ID), ""},
				{"duplicate", fmt.Sprintf(`{"conflicts":[{"ids":[%q,%q],"note":"disputed"}]}`, first.ID, first.ID), ""},
				{"one-source", fmt.Sprintf(`{"conflicts":[{"ids":[%q],"note":"disputed"}]}`, first.ID), ""},
				{"empty-group", `{"conflicts":[{}]}`, ""},
				{"duplicate-field", fmt.Sprintf(`{"conflicts":[{"ids":[%q,%q],"note":"one","note":"two"}]}`, first.ID, second.ID), ""},
				{"unknown-field", `{"conflicts":[],"retire":true}`, ""},
				{"null-field", `{"conflicts":null}`, ""},
				{"malformed-json", `{"conflicts":[`, ""},
				{"trailing-json", valid + `{}`, ""},
				{"truncated-provider", valid, map[string]string{"anthropic": "max_tokens", "ollama": "length"}[provider]},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					opts, requests := analysisTestProvider(t, provider, func(reviewInput) (int, []byte) {
						return http.StatusOK, analysisEnvelope(provider, fixture.payload, fixture.stop)
					})
					got, err := s.ReviewAnalyze(ctx, analysisTestNamespace, opts)
					if !errors.Is(err, ErrInvalidReviewResponse) || len(got) != 0 || requests.Load() != 1 {
						t.Fatalf("invalid response was not rejected: got %+v, requests %d, error %v", got, requests.Load(), err)
					}
					records, err := s.ReviewList(ctx, analysisTestNamespace)
					if err != nil || len(records) != 0 {
						t.Fatalf("invalid response left journal records: %+v, error %v", records, err)
					}
					facts, err := s.List(analysisTestNamespace)
					if err != nil || len(facts) != 2 || facts[0].IsSuperseded() || facts[1].IsSuperseded() {
						t.Fatalf("invalid response mutated active facts: %+v, error %v", facts, err)
					}
				})
			}
		})
	}
}

func TestReviewAnalyzeC106RevalidatesAfterProviderRequest(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			s, closeStore := openTestStore(t)
			defer closeStore()
			first := analysisTestFact(t, s, analysisTestNamespace, "Production cache expiry is 30 minutes.")
			second := analysisTestFact(t, s, analysisTestNamespace, "Production cache expiry is 60 minutes.")
			opts, requests := analysisTestProvider(t, provider, func(input reviewInput) (int, []byte) {
				if len(input.Sources) != 2 {
					t.Error("both sources must have been sent before the edit")
				}
				changed := first
				changed.Text = "Production cache expiry is 90 minutes."
				if err := s.UpdateFact(analysisTestNamespace, changed); err != nil {
					t.Errorf("edit during provider request: %v", err)
					return http.StatusInternalServerError, nil
				}
				payload := fmt.Sprintf(`{"summary":"A candidate","consumes":[%q],"conflicts":[{"ids":[%q,%q],"note":"Cache expiry differs"}]}`, second.ID, first.ID, second.ID)
				return http.StatusOK, analysisEnvelope(provider, payload, "")
			})
			result, err := s.ReviewAnalyze(ctx, analysisTestNamespace, opts)
			if !errors.Is(err, ErrFactChanged) || len(result) != 0 || requests.Load() != 1 {
				t.Fatalf("stale provider response = %+v, requests %d, error %v", result, requests.Load(), err)
			}
			journal, err := s.ReviewList(ctx, analysisTestNamespace)
			if err != nil || len(journal) != 0 {
				t.Fatalf("stale response left records: %+v, error %v", journal, err)
			}
			current, exists := s.loadFact(analysisTestNamespace, first.ID)
			if !exists || current.IsSuperseded() || current.Text != "Production cache expiry is 90 minutes." {
				t.Fatalf("review changed the concurrent canonical edit: %+v", current)
			}
		})
	}
}

func TestReviewAnalyzeC108ByteBudgets(t *testing.T) {
	ctx := context.Background()
	fact := func(id, text string, weight float64) Fact {
		return Fact{ID: id, AgentID: analysisTestNamespace, Text: text, Weight: weight}
	}
	ids := func(snapshots []EvidenceSnapshot) []string {
		var result []string
		for _, snapshot := range snapshots {
			result = append(result, snapshot.FactID)
		}
		return result
	}
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			opts := ReviewAnalyzeOptions{Provider: provider, Model: "test-model", APIKey: "test-only"}
			t.Run("skip-oversized-and-use-later-sources", func(t *testing.T) {
				facts := []Fact{
					fact("900kb-cjk", strings.Repeat("界", 300000), 0.01),
					fact("unbroken-url", "https://example.test/"+strings.Repeat("x", ReviewMaxSourceBytes*3), 0.02),
					fact("escaped-overflow", strings.Repeat("\\", ReviewMaxSourceBytes), 0.03),
					fact("small-z", "Literal small source <\\> 界.", 0.5),
					fact("small-a", "Another small source.", 0.5),
				}
				snapshots, body, err := buildReviewRequest(ctx, analysisTestNamespace, opts, facts)
				if err != nil || !slices.Equal(ids(snapshots), []string{"small-a", "small-z"}) || len(body) > ReviewMaxRequestBytes {
					t.Fatalf("oversized inputs blocked later sources: selected %v, bytes %d, error %v", ids(snapshots), len(body), err)
				}
				if snapshots[1].Text != "Literal small source <\\> 界." {
					t.Fatal("selected source text was rewritten")
				}
			})
			t.Run("source-and-count-bounds", func(t *testing.T) {
				facts := []Fact{
					fact("under", strings.Repeat("a", ReviewMaxSourceBytes-1), 0.1),
					fact("exact", strings.Repeat("a", ReviewMaxSourceBytes), 0.2),
					fact("over", strings.Repeat("a", ReviewMaxSourceBytes+1), 0.3),
				}
				snapshots, _, err := buildReviewRequest(ctx, analysisTestNamespace, opts, facts)
				if err != nil || !slices.Equal(ids(snapshots), []string{"under", "exact"}) {
					t.Fatalf("source byte boundary selected %v, error %v", ids(snapshots), err)
				}
				facts = facts[:0]
				for i := 0; i < ReviewMaxSources+1; i++ {
					facts = append(facts, fact(fmt.Sprintf("%02d", i), "small source", 1))
				}
				snapshots, _, err = buildReviewRequest(ctx, analysisTestNamespace, opts, facts)
				if err != nil || len(snapshots) != ReviewMaxSources || snapshots[ReviewMaxSources-1].FactID != "11" {
					t.Fatalf("source count boundary selected %v, error %v", ids(snapshots), err)
				}
			})
			t.Run("actual-serialized-request-boundary", func(t *testing.T) {
				facts := []Fact{
					fact("1", strings.Repeat("a", ReviewMaxSourceBytes), 0.1),
					fact("2", strings.Repeat("b", ReviewMaxSourceBytes), 0.2),
					fact("3", strings.Repeat("c", ReviewMaxSourceBytes), 0.3),
					fact("4", "", 0.4),
					fact("5", "later small source", 0.5),
				}
				var base []EvidenceSnapshot
				for _, input := range facts[:4] {
					base = append(base, NewEvidenceSnapshot(input))
				}
				body, err := marshalReviewRequest(analysisTestNamespace, opts, base)
				if err != nil {
					t.Fatal(err)
				}
				fit := ReviewMaxRequestBytes - len(body)
				if fit < 2 || fit+1 > ReviewMaxSourceBytes {
					t.Fatalf("boundary fixture cannot fit an eligible fourth source: remaining %d", fit)
				}
				for _, delta := range []int{-1, 0, 1} {
					facts[3].Text = strings.Repeat("d", fit+delta)
					allFour := append(slices.Clone(base[:3]), NewEvidenceSnapshot(facts[3]))
					exact, err := marshalReviewRequest(analysisTestNamespace, opts, allFour)
					if err != nil || len(exact) != ReviewMaxRequestBytes+delta {
						t.Fatalf("fixture serialized to %d bytes, error %v", len(exact), err)
					}
					snapshots, got, err := buildReviewRequest(ctx, analysisTestNamespace, opts, slices.Clone(facts))
					expected := []string{"1", "2", "3", "4"}
					if delta > 0 {
						expected[3] = "5"
					}
					if err != nil || !slices.Equal(ids(snapshots), expected) || len(got) > ReviewMaxRequestBytes {
						t.Fatalf("request limit %+d: selected %v, bytes %d, error %v", delta, ids(snapshots), len(got), err)
					}
					if delta <= 0 && len(got) != ReviewMaxRequestBytes+delta {
						t.Fatalf("accepted serialized request was %d bytes", len(got))
					}
				}
			})
			t.Run("actual-provider-response-boundary", func(t *testing.T) {
				const payload = `{"conflicts":[]}`
				envelope := analysisEnvelope(provider, payload, "")
				for _, delta := range []int{-1, 0, 1} {
					length := ReviewMaxResponseBytes + delta
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Length", strconv.Itoa(length))
						_, _ = w.Write(envelope)
						_, _ = io.WriteString(w, strings.Repeat(" ", length-len(envelope)))
					}))
					got, err := requestReview(ctx, opts, server.URL, []byte("{}"))
					server.Close()
					if delta <= 0 && (err != nil || got != payload) {
						t.Fatalf("response limit %+d: got %q, error %v", delta, got, err)
					}
					if delta > 0 && !errors.Is(err, ErrInvalidReviewResponse) {
						t.Fatalf("over-budget response was accepted: %v", err)
					}
				}
			})
		})
	}
	t.Run("note-boundary-is-utf8-bytes", func(t *testing.T) {
		batch := []EvidenceSnapshot{NewEvidenceSnapshot(fact("one", "one", 1)), NewEvidenceSnapshot(fact("two", "two", 1))}
		for _, delta := range []int{-1, 0, 1} {
			note := strings.Repeat("界", 682) + strings.Repeat("a", 2+delta)
			if len(note) != ReviewMaxNoteBytes+delta {
				t.Fatal("incorrect UTF-8 note fixture")
			}
			raw := fmt.Sprintf(`{"conflicts":[{"ids":["one","two"],"note":%q}]}`, note)
			findings, err := parseReviewFindings(raw, batch)
			if delta <= 0 && (err != nil || len(findings) != 1 || findings[0].Note != note) {
				t.Fatalf("note limit %+d: findings %d, error %v", delta, len(findings), err)
			}
			if delta > 0 && !errors.Is(err, ErrInvalidReviewResponse) {
				t.Fatalf("over-budget note was accepted: %v", err)
			}
		}
	})
}

func TestReviewAnalyzeC112PreservesRecallAndMaintenance(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			s, _ := openConsolidationSafetyStore(t, StoreConfig{DataDir: t.TempDir()})
			source := analysisTestFact(t, s, analysisTestNamespace, "Backups are encrypted.")
			analysisTestFact(t, s, analysisTestNamespace, "Storage uses checksum verification.")
			before, err := s.RecallPreview(ctx, analysisTestNamespace, "backup storage", 10)
			if err != nil {
				t.Fatal(err)
			}
			var response atomic.Int32
			opts, requests := analysisTestProvider(t, provider, func(reviewInput) (int, []byte) {
				switch response.Load() {
				case 1:
					return http.StatusOK, analysisEnvelope(provider, fmt.Sprintf(`{"summary":"A candidate","consumes":[%q]}`, source.ID), "")
				case 2:
					return http.StatusServiceUnavailable, []byte("mock unavailable")
				default:
					return http.StatusOK, analysisEnvelope(provider, `{"summary":"","consumes":[],"conflicts":[]}`, "")
				}
			})
			if records, err := s.ReviewAnalyze(ctx, analysisTestNamespace, opts); err != nil || len(records) != 0 {
				t.Fatalf("empty response = %+v, error %v", records, err)
			}
			response.Store(1)
			records, err := s.ReviewAnalyze(ctx, analysisTestNamespace, opts)
			if err != nil || len(records) != 1 {
				t.Fatalf("summary response = %+v, error %v", records, err)
			}
			if err := s.ReviewApply(ctx, analysisTestNamespace, records[0].ID, ReviewAction{Action: "reject"}); err != nil {
				t.Fatal(err)
			}
			response.Store(2)
			if records, err := s.ReviewAnalyze(ctx, analysisTestNamespace, opts); err == nil || len(records) != 0 {
				t.Fatalf("provider failure = %+v, error %v", records, err)
			}
			after, err := s.RecallPreview(ctx, analysisTestNamespace, "backup storage", 10)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("review changed recall: before %+v, after %+v, error %v", before, after, err)
			}
			expired := analysisTestFact(t, s, analysisTestNamespace, "An expired cache receipt.")
			expired.AccessedAt = s.now().Add(-201 * 24 * time.Hour)
			if err := s.UpdateFact(analysisTestNamespace, expired); err != nil {
				t.Fatal(err)
			}
			cfg := &testConsolidateCfg{threshold: 0, llm: provider, apiKey: "test-only", model: opts.Model, ollamaURL: opts.Endpoint, ollamaMdl: opts.Model}
			t.Setenv("ANTHROPIC_BASE_URL", opts.Endpoint)
			if err := s.MaybeConsolidate(ctx, analysisTestNamespace, cfg); err != nil {
				t.Fatal(err)
			}
			s.LaunchAsyncConsolidate(analysisTestNamespace, cfg)
			s.wg.Wait()
			if _, exists := s.loadFact(analysisTestNamespace, expired.ID); exists || requests.Load() != 3 {
				t.Fatalf("maintenance failed to prune or made a provider call: expired exists %t, calls %d", exists, requests.Load())
			}
		})
	}
}
