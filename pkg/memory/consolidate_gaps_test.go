package memory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// Coverage-gap tests for the consolidation edges: the Anthropic wire paths
// (mocked through the SDK's base-URL override), the Ollama client's remaining
// failure modes, and the closed-store error branches. The empty-Anthropic-
// response regression here guards a real defect: an empty model reply used to
// become an empty summary fact that consumed its whole batch.

const anthropicMessageShape = `{
  "id": "msg_01",
  "type": "message",
  "role": "assistant",
  "model": "claude-test",
  "content": [%s],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 1, "output_tokens": 1}
}`

// anthropicServer returns a Messages-API mock and points the SDK at it via
// ANTHROPIC_BASE_URL, which anthropic.NewClient honours. handler receives the
// raw request body; it replies with whatever text it is given.
func anthropicServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	return srv
}

func anthropicTextMessage(text string) string {
	esc := strings.NewReplacer("\\", "\\\\", `"`, `\"`, "\n", `\n`).Replace(text)
	return fmt.Sprintf(anthropicMessageShape, fmt.Sprintf(`{"type":"text","text":"%s"}`, esc))
}

func anthropicCfg() *testConsolidateCfg {
	return &testConsolidateCfg{threshold: 3, halfLife: 720 * time.Hour,
		llm: "anthropic", apiKey: "test-key", model: "claude-test"}
}

func openHookedStore(t *testing.T, hook func(string, error)) *Store {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      720 * time.Hour,
		OnConsolidateError: hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedBatch(t *testing.T, s *Store, agent string) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if err := s.Put(context.Background(), agent, fmt.Sprintf("batch fact number %d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractFacts_TableDriven(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		key     string
		status  int
		body    string
		want    []string
		wantErr bool
	}{
		{
			name: "empty text degrades to nothing without calling the API",
			text: "", key: "k", want: nil,
		},
		{
			name: "no key degrades to the raw text as one fact",
			text: "raw thought", key: "", want: []string{"raw thought"},
		},
		{
			name: "fenced JSON with blanks parses and filters",
			text: "conversation body", key: "k", status: 200,
			body: anthropicTextMessage("```json\n[\"fact one.\", \"   \", \"fact two.\"]\n```"),
			want: []string{"fact one.", "fact two."},
		},
		{
			name: "non-JSON reply falls back to the whole text",
			text: "conversation body", key: "k", status: 200,
			body: anthropicTextMessage("I prefer prose over JSON."),
			want: []string{"conversation body"},
		},
		{
			name: "empty content is an error, not a silent nothing",
			text: "conversation body", key: "k", status: 200,
			body:    anthropicMessageShape + "",
			wantErr: true,
		},
		{
			name: "auth failure surfaces wrapped",
			text: "conversation body", key: "k", status: 401,
			body:    `{"error":{"type":"authentication_error","message":"bad key"}}`,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.key != "" {
				status := tc.status
				if status == 0 {
					status = 200
				}
				b := tc.body
				if b == "" && !tc.wantErr {
					b = anthropicTextMessage(`["unused"]`)
				}
				anthropicServer(t, status, b)
			}
			got, err := ExtractFacts(context.Background(), tc.text,
				&testConsolidateCfg{apiKey: tc.key, model: "claude-test"})
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got %+v", got)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestConsolidation_AnthropicEmptyContentNeverTouchesTheStore is the
// regression for the defect the coverage audit surfaced: an empty model
// response used to be applied as an empty summary fact that tombstoned the
// entire batch toward garbage. Now it is a discarded proposal.
func TestConsolidation_AnthropicEmptyContentNeverTouchesTheStore(t *testing.T) {
	var reported []error
	s := openHookedStore(t, func(_ string, err error) { reported = append(reported, err) })
	seedBatch(t, s, "empty-anthropic")

	anthropicServer(t, 200, fmt.Sprintf(anthropicMessageShape, "")) // content:[] — empty

	if err := s.Consolidate(context.Background(), "empty-anthropic", anthropicCfg()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	found := false
	for _, e := range reported {
		if errors.Is(e, ErrInvalidProposal) && strings.Contains(e.Error(), "empty content") {
			found = true
		}
	}
	if !found {
		t.Errorf("empty-content reply reported as %v, want ErrInvalidProposal naming it", reported)
	}
	facts, _ := s.List("empty-anthropic")
	if len(facts) != 5 {
		t.Fatalf("store mutated by an empty proposal: %d facts, want 5", len(facts))
	}
	for _, f := range facts {
		if f.IsSuperseded() || f.Text == "" {
			t.Errorf("batch fact %q was consumed by an empty proposal", f.Text)
		}
	}
	if cycles, _ := s.ConsolidationCounters(); cycles != 0 {
		t.Errorf("counters advanced on a discarded proposal")
	}
}

func TestConsolidation_AnthropicSuccessConsumesWholeBatchWithReceipts(t *testing.T) {
	s := openHookedStore(t, func(_ string, err error) { t.Errorf("hook fired: %v", err) })
	seedBatch(t, s, "anthropic-ok")

	anthropicServer(t, 200, anthropicTextMessage("One paragraph holding every consumed fact."))

	if err := s.Consolidate(context.Background(), "anthropic-ok", anthropicCfg()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	facts, _ := s.List("anthropic-ok")
	const wantBatch = 2 // weakest half of 5: len(live)/2
	var summary *Fact
	live := 0
	for i := range facts {
		if facts[i].Text == "One paragraph holding every consumed fact." {
			summary = &facts[i]
			continue
		}
		if !facts[i].IsSuperseded() {
			live++
		} else if facts[i].SupersededBy != summaryIDOf(facts, summary) {
			t.Errorf("tombstone %q points at %q, not the summary", facts[i].Text, facts[i].SupersededBy)
		}
	}
	if summary == nil {
		t.Fatal("summary never landed")
	}
	if live != 5-wantBatch {
		t.Errorf("%d batch facts stayed live beyond the unconsumed half", live)
	}
	cycles, consumed := s.ConsolidationCounters()
	if cycles != 1 || consumed != wantBatch {
		t.Errorf("counters = (%d,%d), want (1,%d)", cycles, consumed, wantBatch)
	}
}

// summaryIDOf resolves the summary fact's ID once it exists; before that it
// returns a sentinel that can never match, so premature comparisons fail.
func summaryIDOf(facts []Fact, summary *Fact) string {
	if summary == nil {
		return "\x00unlanded"
	}
	return summary.ID
}

func TestSummariseFacts_UnknownProviderIsASilentSkip(t *testing.T) {
	s := openHookedStore(t, func(_ string, err error) {
		t.Errorf("unknown provider reported as failure: %v", err)
	})
	seedBatch(t, s, "weird-provider")
	cfg := &testConsolidateCfg{threshold: 3, halfLife: 720 * time.Hour, llm: "crystal-ball"}
	if err := s.Consolidate(context.Background(), "weird-provider", cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	facts, _ := s.List("weird-provider")
	if len(facts) != 5 {
		t.Errorf("facts = %d, want 5 untouched", len(facts))
	}
}

func TestOllama_MalformedEnvelopeFailsWithoutRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"response":`) // truncated JSON envelope
	}))
	defer srv.Close()

	s := openHookedStore(t, nil)
	seedBatch(t, s, "envelope-agent")
	cfg := &testConsolidateCfg{threshold: 4, halfLife: 720 * time.Hour,
		llm: "ollama", ollamaURL: srv.URL}

	var reported []error
	s.cfg.OnConsolidateError = func(_ string, err error) { reported = append(reported, err) }

	if err := s.Consolidate(context.Background(), "envelope-agent", cfg); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("malformed envelope earned %d attempts, want 1 (not retryable)", attempts)
	}
	if len(reported) == 0 || !strings.Contains(reported[0].Error(), "decode envelope") {
		t.Errorf("reported %v, want envelope decode error", reported)
	}
}

func TestOllama_LongErrorBodyIsTruncatedInTheReport(t *testing.T) {
	long := strings.Repeat("x", 500)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, long, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var reported []error
	s := openHookedStore(t, func(_ string, err error) { reported = append(reported, err) })
	seedBatch(t, s, "long-body-agent")
	cfg := &testConsolidateCfg{threshold: 4, halfLife: 720 * time.Hour,
		llm: "ollama", ollamaURL: srv.URL}

	if err := s.Consolidate(context.Background(), "long-body-agent", cfg); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("503 earned %d attempts, want 2", attempts)
	}
	if len(reported) == 0 {
		t.Fatal("fallback was silent")
	}
	for _, e := range reported {
		if strings.Contains(e.Error(), strings.Repeat("x", 300)) {
			t.Error("error carries the untruncated body; logs would drown in it")
		}
	}
}

func TestClosedStore_ConsolidationPathsFailLoudly(t *testing.T) {
	s := openHookedStore(t, nil)
	seedBatch(t, s, "closed-agent")
	cfg := defaultTestCfg()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.List("closed-agent"); err == nil {
		t.Error("List on closed store returned no error")
	}
	if err := s.Consolidate(context.Background(), "closed-agent", cfg); err == nil {
		t.Error("Consolidate on closed store returned no error")
	}
	if got := s.factIDByText("closed-agent", "anything"); got != "" {
		t.Errorf("factIDByText on closed store = %q, want empty", got)
	}
	err := s.PutConfident(context.Background(), "closed-agent", "x", "verified")
	if err == nil {
		t.Error("PutConfident on closed store returned no error")
	}
}

// errTypedExtractor fails extraction for every fact: the step-4 loop must
// skip the fact without recording a watermark, so a later healthy pass can
// pick it up.
type errTypedExtractor struct{}

func (errTypedExtractor) ExtractIDs(string) ([]string, error) {
	return nil, errors.New("extractor offline")
}
func (errTypedExtractor) ExtractTyped(string) ([]EntityRef, []EntityLink, error) {
	return nil, nil, errors.New("extractor offline")
}

// errGraph fails every write, so nothing may be watermarked: a watermark on a
// failed upsert would hide the node from every future pass.
type errGraph struct{}

func (errGraph) UpsertNode(string, string, string) error     { return errors.New("graph read-only") }
func (errGraph) NeighborTexts(string, int) ([]string, error) { return nil, nil }

func TestExtraction_FailureBranchesNeverWatermark(t *testing.T) {
	t.Run("extractor error skips the fact cleanly", func(t *testing.T) {
		s := openHookedStore(t, nil)
		s.SetKG(&countingGraph{}, errTypedExtractor{})
		seedBatch(t, s, "ex-err")

		if err := s.Consolidate(context.Background(), "ex-err", defaultTestCfg()); err != nil {
			t.Fatal(err)
		}
		facts, _ := s.List("ex-err")
		for _, f := range facts {
			if _, done := s.extractedSignature("ex-err", f.ID); done {
				t.Error("failed extraction was watermarked anyway")
			}
		}
	})

	t.Run("graph write errors fire the hook and skip the watermark", func(t *testing.T) {
		var reported []error
		s := openHookedStore(t, func(_ string, err error) { reported = append(reported, err) })
		s.SetKG(errGraph{}, &countingTypedExtractor{})
		seedBatch(t, s, "graph-err")

		if err := s.Consolidate(context.Background(), "graph-err", defaultTestCfg()); err != nil {
			t.Fatal(err)
		}
		if len(reported) == 0 {
			t.Error("graph failures were swallowed")
		}
		facts, _ := s.List("graph-err")
		for _, f := range facts {
			if _, done := s.extractedSignature("graph-err", f.ID); done {
				t.Error("watermark recorded despite failed graph writes")
			}
		}
	})

	t.Run("legacy ID-only extractor still feeds nodes", func(t *testing.T) {
		s := openHookedStore(t, nil)
		g := &recordingKG{}
		ex := &recordingExtractor{}
		s.SetKG(g, ex) // plain accessor pair: neither implements Typed/EdgeWriter
		seedBatch(t, s, "legacy-kg")

		if err := s.Consolidate(context.Background(), "legacy-kg", defaultTestCfg()); err != nil {
			t.Fatal(err)
		}
		if len(g.upserts) == 0 {
			t.Error("legacy path produced no upserts")
		}
	})
}
