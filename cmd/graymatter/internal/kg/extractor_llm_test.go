package kg

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

// The LLM extractor is opt-in, so its failure modes must be invisible to the
// graph: every degradation lands on the regex extractor, and model noise
// (unknown types/relations) is dropped at the schema gate rather than stored.

func TestLLMFallback_DelegatesToRegexOnProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	f := NewLLMExtractor("test-key", "").(*llmFallback)
	nodes, _, err := f.Extract("Maria Rodriguez joined Acme Corp.")
	if err != nil {
		t.Fatalf("fallback must swallow the transport error: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("fallback produced no nodes; regex extractor should have")
	}
	found := false
	for _, n := range nodes {
		if strings.EqualFold(n.Label, "maria rodriguez") {
			found = true
		}
	}
	if !found {
		t.Errorf("regex fallback missed known entity; got %v", nodeLabels(nodes))
	}
}

func TestKGExtractorSnapshotsAnthropicBase(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	gateway := func(prefix, entity string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload struct{ Model string }
			err := json.NewDecoder(r.Body).Decode(&payload)
			if err != nil || payload.Model != "synthetic-kg-model" || r.Method != http.MethodPost ||
				r.URL.Path != prefix+"/v1/messages" || r.Header.Get("x-api-key") != "fake-kg-key" {
				t.Errorf("unexpected extraction request: %s %s %+v, %v", r.Method, r.URL.Path, payload, err)
			}
			w.Header().Set("Content-Type", "application/json")
			content := `{"nodes":[{"label":"` + entity + `","entity_type":"concept"}],"edges":[]}`
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]string{{"type": "text", "text": content}},
			})
		}))
		t.Cleanup(server.Close)
		return server
	}
	a := gateway("/first", "synthetic entity A")
	b := gateway("/second", "synthetic entity B")
	t.Setenv("ANTHROPIC_BASE_URL", a.URL+"/first")
	first := NewLLMExtractor("fake-kg-key", "synthetic-kg-model")
	t.Setenv("ANTHROPIC_BASE_URL", b.URL+"/second/")
	second := NewLLMExtractor("fake-kg-key", "synthetic-kg-model")
	for _, tc := range []struct {
		extractor EntityExtractor
		entity    string
	}{{first, "synthetic entity A"}, {second, "synthetic entity B"}, {first, "synthetic entity A"}} {
		nodes, _, err := tc.extractor.Extract("Source text without the fixture entity.")
		if err != nil || len(nodes) != 1 || nodes[0].Label != tc.entity {
			t.Fatalf("extract = %+v, %v; want %q", nodes, err, tc.entity)
		}
	}

	t.Setenv("ANTHROPIC_BASE_URL", "https://dummy:KG-CANARY@127.0.0.1")
	invalid := NewLLMExtractor("fake-kg-key", "").(*llmFallback)
	var urlError *providerurl.ConfigError
	if _, _, err := invalid.llm.Extract("source"); !errors.As(err, &urlError) || strings.Contains(err.Error(), "KG-CANARY") {
		t.Fatalf("invalid initializer error = %v", err)
	}
	text := "Maria Rodriguez joined Acme Corp."
	nodes, _, err := invalid.Extract(text)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("invalid URL fallback = %+v, %v", nodes, err)
	}
	nodes, _, err = NewExtractor(ExtractorConfig{}).Extract(text)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("explicit regex with inactive invalid URL = %+v, %v", nodes, err)
	}
}

func TestParseLLMExtractionJSON_SchemaGate(t *testing.T) {
	raw := `{"nodes":[
		{"id":"a","label":"Alice","entity_type":"person"},
		{"id":"b","label":"BobCorp","entity_type":"wizard"},
		{"id":"c","label":"Charlie","entity_type":""}
	],"edges":[
		{"from":"a","to":"b","relation":"related_to"},
		{"from":"a","to":"c","relation":"casts_spell"}
	]}`
	nodes, edges, err := parseLLMExtractionJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Label != "Alice" {
		t.Fatalf("nodes = %+v, want only Alice (unknown/empty types dropped)", nodes)
	}
	if len(edges) != 1 || edges[0].Relation != "related_to" {
		t.Fatalf("edges = %+v, want only related_to (unknown relations dropped)", edges)
	}
}

func TestExtractorFromEnv_DefaultIsRegex(t *testing.T) {
	t.Setenv("GRAYMATTER_KG_LLM_EXTRACT", "")
	e := ExtractorFromEnv()
	if _, ok := e.(*regexExtractor); !ok {
		t.Fatalf("default extractor = %T, want regex", e)
	}
	if _, ok := ExtractorFromEnv().(*llmFallback); ok {
		t.Fatal("env unset must not enable LLM extraction")
	}
}
