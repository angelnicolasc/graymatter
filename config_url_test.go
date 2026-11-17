package graymatter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type providerURLCase struct {
	provider, env, official string
	set                     func(*Config, string)
	get                     func(Config) string
	activate                func(*Config)
}

var providerURLCases = []providerURLCase{
	{"anthropic", "ANTHROPIC_BASE_URL", "https://api.anthropic.com",
		func(c *Config, v string) { c.AnthropicBaseURL = v },
		func(c Config) string { return c.AnthropicBaseURL },
		func(c *Config) { c.AnthropicAPIKey = "fake-config-key" }},
	{"openai", "OPENAI_BASE_URL", "https://api.openai.com/v1",
		func(c *Config, v string) { c.OpenAIBaseURL = v },
		func(c Config) string { return c.OpenAIBaseURL },
		func(c *Config) { c.EmbeddingMode, c.OpenAIAPIKey = EmbeddingOpenAI, "fake-config-key" }},
	{"voyage", "VOYAGE_BASE_URL", "https://api.voyageai.com/v1",
		func(c *Config, v string) { c.VoyageBaseURL = v },
		func(c Config) string { return c.VoyageBaseURL },
		func(c *Config) { c.EmbeddingMode, c.VoyageAPIKey = EmbeddingVoyage, "fake-config-key" }},
}

func newURLTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(t.TempDir(), "store")
	cfg.EmbeddingMode = EmbeddingKeyword
	cfg.AnthropicAPIKey, cfg.OpenAIAPIKey, cfg.VoyageAPIKey = "", "", ""
	cfg.ConsolidateLLM = ""
	cfg.AsyncConsolidate = false
	cfg.VectorReconcileInterval = 0
	return cfg
}

func TestProviderBaseURLPrecedence(t *testing.T) {
	valid, explicit := "http://127.0.0.1:1/environment", "http://127.0.0.1:1/explicit"
	invalid := "http://test-secret@127.0.0.1:1"
	variants := []struct {
		name, field string
		environment *string
		want        string
	}{
		{"default", "", nil, ""},
		{"empty-environment", "", new(string), ""},
		{"environment", "", &valid, valid},
		{"environment-malformed", "", &invalid, invalid},
		{"explicit-default-environment", explicit, nil, explicit},
		{"explicit-empty-environment", explicit, new(string), explicit},
		{"explicit-wins", explicit, &valid, explicit},
		{"explicit-wins-malformed", explicit, &invalid, explicit},
		{"invalid-field-no-environment", invalid, nil, invalid},
		{"invalid-field-empty-environment", invalid, new(string), invalid},
		{"invalid-field-no-fallback", invalid, &valid, invalid},
		{"invalid-field-and-environment", invalid, &invalid, invalid},
	}
	for _, provider := range providerURLCases {
		t.Run(provider.provider, func(t *testing.T) {
			for _, scenario := range variants {
				t.Run(scenario.name, func(t *testing.T) {
					t.Setenv(provider.env, "")
					if scenario.environment == nil {
						if err := os.Unsetenv(provider.env); err != nil {
							t.Fatal(err)
						}
					} else {
						t.Setenv(provider.env, *scenario.environment)
					}
					cfg := newURLTestConfig(t)
					provider.set(&cfg, scenario.field)
					provider.activate(&cfg)
					m, err := NewWithConfig(cfg)
					if scenario.want == invalid {
						var configErr *providerurl.ConfigError
						if !errors.As(err, &configErr) || configErr.Provider != provider.provider {
							t.Fatalf("got %v; expected typed %s configuration error", err, provider.provider)
						}
						if strings.Contains(err.Error(), "test-secret") {
							t.Fatal("configuration error leaked URL credentials")
						}
						if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("invalid configuration created store: stat err=%v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer m.Close()
					want := scenario.want
					if want == "" {
						want = provider.official
					}
					if got := provider.get(m.Config()); got != want {
						t.Fatalf("effective base = %q, want %q", got, want)
					}
				})
			}
		})
	}
}

func TestProviderBaseURLOnlyChecksActiveProviders(t *testing.T) {
	var ollamaCalls atomic.Int32
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaCalls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" {
			t.Errorf("unexpected Ollama probe %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ollama.Close()

	for _, p := range providerURLCases {
		t.Setenv(p.env, "not a URL")
	}
	for _, mode := range []EmbeddingMode{EmbeddingKeyword, EmbeddingOllama, EmbeddingOpenAI, EmbeddingVoyage, EmbeddingAnthropic} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			cfg := newURLTestConfig(t)
			cfg.EmbeddingMode = mode
			m, err := NewWithConfig(cfg)
			if err != nil {
				t.Fatalf("inactive URL blocked memory: %v", err)
			}
			defer m.Close()
			if mode != EmbeddingOllama && m.embedder.Dimensions() != 0 {
				t.Fatalf("URL alone selected %s", m.embedder.Name())
			}
		})
	}
	cfg := newURLTestConfig(t)
	cfg.EmbeddingMode, cfg.OllamaURL = EmbeddingAuto, ollama.URL
	cfg.OpenAIAPIKey, cfg.VoyageAPIKey = "fake-openai", "fake-voyage"
	m, err := NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if !strings.HasPrefix(m.embedder.Name(), "ollama:") || ollamaCalls.Load() != 1 {
		t.Fatalf("priority changed: provider %s, probes %d", m.embedder.Name(), ollamaCalls.Load())
	}
}

func TestProviderBaseURLEmbeddingsPersist(t *testing.T) {
	for _, p := range []struct {
		name, env, model string
		dimensions       int
		mode             EmbeddingMode
		configure        func(*Config)
	}{
		{"openai", "OPENAI_BASE_URL", "fixture-openai-model", 1536, EmbeddingOpenAI,
			func(c *Config) { c.OpenAIAPIKey, c.OpenAIModel = "fake-embedding-key", "fixture-openai-model" }},
		{"voyage", "VOYAGE_BASE_URL", "voyage-3", 1024, EmbeddingVoyage,
			func(c *Config) { c.VoyageAPIKey = "fake-embedding-key" }},
	} {
		t.Run(p.name, func(t *testing.T) {
			ctx := context.Background()
			a, b := make([]float32, p.dimensions), make([]float32, p.dimensions)
			a[0], b[1] = 1, 1
			newGateway := func(label string, vector []float32) (*httptest.Server, *atomic.Int32) {
				calls := new(atomic.Int32)
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.URL.EscapedPath() != "/edge%2Fsegment/"+label+"/v1/embeddings" {
						t.Errorf("unexpected embeddings request: %s %s", r.Method, r.URL.EscapedPath())
					}
					if r.Header.Get("Authorization") != "Bearer fake-embedding-key" || r.Header.Get("Content-Type") != "application/json" {
						t.Error("embeddings headers did not match")
					}
					var req struct {
						Model string   `json:"model"`
						Input []string `json:"input"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
						req.Model != p.model || !reflect.DeepEqual(req.Input, []string{"synthetic fact " + label}) {
						t.Errorf("embeddings payload did not match: %+v, %v", req, err)
					}
					if err := json.NewEncoder(w).Encode(map[string]any{
						"data": []map[string]any{{"embedding": vector}},
					}); err != nil {
						t.Errorf("fixture response: %v", err)
					}
				}))
				return s, calls
			}
			first, callsA := newGateway("a", a)
			defer first.Close()
			second, callsB := newGateway("b", b)
			defer second.Close()
			t.Setenv(p.env, first.URL+"/edge%2Fsegment/a/v1///")
			cfgA := newURLTestConfig(t)
			t.Setenv(p.env, second.URL+"/edge%2Fsegment/b/v1")
			cfgB := newURLTestConfig(t)
			var indexErrors atomic.Int32
			for _, cfg := range []*Config{&cfgA, &cfgB} {
				cfg.EmbeddingMode = p.mode
				cfg.OnVectorIndexError = func(_, _ string, _ error) { indexErrors.Add(1) }
				p.configure(cfg)
			}
			stores := []struct {
				cfg    Config
				label  string
				vector []float32
				calls  *atomic.Int32
				handle *Memory
			}{
				{cfg: cfgA, label: "a", vector: a, calls: callsA},
				{cfg: cfgB, label: "b", vector: b, calls: callsB},
			}
			for i := range stores {
				c := &stores[i]
				m, err := NewWithConfig(c.cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				c.handle = m
			}
			t.Setenv(p.env, "http://%zz/")
			writes := make(chan error, len(stores))
			for _, c := range stores {
				go func() { writes <- c.handle.Remember(ctx, "audit", "synthetic fact "+c.label) }()
			}
			var writeErr error
			for range stores {
				writeErr = errors.Join(writeErr, <-writes)
			}
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			for _, c := range stores {
				facts, err := c.handle.Advanced().List("audit")
				if err != nil || len(facts) != 1 || facts[0].Text != "synthetic fact "+c.label || !reflect.DeepEqual(facts[0].Embedding, c.vector) {
					t.Fatalf("expected exact committed vector for %s, got %d facts (error %v)", c.label, len(facts), err)
				}
				if p := c.handle.Advanced().PendingVectorCount(); p != 0 || c.calls.Load() != 1 || indexErrors.Load() != 0 {
					t.Fatalf("pending=%d, calls=%d, index errors=%d", p, c.calls.Load(), indexErrors.Load())
				}
			}
			if callsA.Load() != 1 || callsB.Load() != 1 {
				t.Fatalf("crossed snapshots: A=%d, B=%d", callsA.Load(), callsB.Load())
			}
		})
	}
}

func TestMemoryAnthropicBaseURLUsedForBothOperations(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "fixture-environment-token")
	t.Setenv("ANTHROPIC_API_KEY", "fixture-environment-key")
	t.Setenv("ANTHROPIC_BASE_URL", "http://%zz/")
	const extracted = "A synthetic fact produced by extraction."
	const summary = "A distinct synthetic summary from consolidation."
	var extractions, consolidations atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/edge%2F/v1/messages" {
			t.Errorf("wrong Messages request: %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.Header.Get("X-Api-Key") != "fixture-explicit-key" ||
			r.Header.Get("Authorization") != "Bearer fixture-environment-token" ||
			r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Error("wrong Messages credential or version headers")
		}
		var request struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "fixture-claude" {
			t.Errorf("invalid Messages payload: %+v %v", request, err)
		}
		text := summary
		switch request.MaxTokens {
		case 512:
			consolidations.Add(1)
		case 1024:
			extractions.Add(1)
			raw, err := json.Marshal([]string{extracted})
			if err != nil {
				t.Errorf("fixture extraction: %v", err)
			}
			text = string(raw)
		default:
			t.Errorf("unexpected max_tokens: %d", request.MaxTokens)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"text","text":%q}],"model":"fixture-claude","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, text)
	}))
	defer gateway.Close()
	cfg := newURLTestConfig(t)
	cfg.AnthropicAPIKey, cfg.AnthropicBaseURL = "fixture-explicit-key", gateway.URL+"/edge%2F"
	cfg.ConsolidateLLM, cfg.ConsolidateModel, cfg.ConsolidateThreshold = "anthropic", "fixture-claude", 1
	m, err := NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// URL is snapshotted, even though the SDK can read auth environment anew.
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1/changed")
	ctx := context.Background()
	if err := m.RememberExtracted(ctx, "audit", "synthetic original extraction input"); err != nil {
		t.Fatal(err)
	}
	facts, err := m.Advanced().List("audit")
	if err != nil || len(facts) != 1 || facts[0].Text != extracted || memory.EffectiveConfidence(facts[0].Confidence) != "unverified" {
		t.Fatalf("did not persist LLM-derived fact: %+v, %v", facts, err)
	}
	if err := m.Remember(ctx, "audit", "a second synthetic original"); err != nil {
		t.Fatal(err)
	}
	if err := m.Consolidate(ctx, "audit"); err != nil {
		t.Fatal(err)
	}
	facts, err = m.Advanced().List("audit")
	if err != nil {
		t.Fatal(err)
	}
	summaryID := ""
	for _, fact := range facts {
		if fact.Text == summary {
			summaryID = fact.ID
		}
	}
	if summaryID == "" {
		t.Fatal("summary response was not persisted")
	}
	retired := 0
	for _, fact := range facts {
		if fact.SupersededBy == summaryID {
			retired++
		}
	}
	if retired != 1 || extractions.Load() != 1 || consolidations.Load() != 1 {
		t.Fatalf("retired=%d extraction calls=%d consolidation calls=%d", retired, extractions.Load(), consolidations.Load())
	}
}
