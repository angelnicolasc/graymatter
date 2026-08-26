package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// voyageServer spins an httptest server that mimics the Voyage embeddings
// endpoint and records what actually crossed the wire. The provider's
// baseURL seam is pointed at it, so these tests exercise the full HTTP path
// without network access.
type voyageServer struct {
	*httptest.Server
	hits     atomic.Int32
	lastAuth atomic.Value // string
	lastPath atomic.Value // string
	lastBody atomic.Value // voyageEmbedRequest
}

func newVoyageServer(t *testing.T, status int, respBody string) *voyageServer {
	t.Helper()
	vs := &voyageServer{}
	vs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vs.hits.Add(1)
		vs.lastAuth.Store(r.Header.Get("Authorization"))
		vs.lastPath.Store(r.URL.Path)
		var req voyageEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		vs.lastBody.Store(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(vs.Close)
	return vs
}

func newTestVoyage(serverURL string) *VoyageProvider {
	p := NewVoyage(Config{VoyageAPIKey: "test-key"})
	// The seam carries the full request target, exactly as production uses it.
	p.endpoint = serverURL + "/v1/embeddings"
	return p
}

func TestVoyageEmbedSuccess(t *testing.T) {
	vs := newVoyageServer(t, http.StatusOK,
		`{"object":"list","data":[{"object":"embedding","embedding":[0.25,-0.5,1],"index":0}],"model":"voyage-3","usage":{"total_tokens":3}}`)
	p := newTestVoyage(vs.URL)

	emb, err := p.Embed(context.Background(), "hello memory")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(emb) != 3 || emb[0] != 0.25 || emb[2] != 1 {
		t.Fatalf("unexpected embedding: %v", emb)
	}

	if got := vs.lastAuth.Load().(string); got != "Bearer test-key" {
		t.Errorf("Authorization header = %q, want Bearer auth", got)
	}
	if got := vs.lastPath.Load().(string); got != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", got)
	}
	req := vs.lastBody.Load().(voyageEmbedRequest)
	if req.Model != "voyage-3" || len(req.Input) != 1 || req.Input[0] != "hello memory" {
		t.Errorf("request body = %+v, want model voyage-3 with single input", req)
	}

	// Second call for the same text must be served from cache: one wire hit total.
	if _, err := p.Embed(context.Background(), "hello memory"); err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	if n := vs.hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want 1 (cache must absorb repeats)", n)
	}
}

func TestVoyageEmbedNon200SurfacesStatus(t *testing.T) {
	vs := newVoyageServer(t, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	p := newTestVoyage(vs.URL)

	_, err := p.Embed(context.Background(), "text")
	if err == nil {
		t.Fatal("want error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "status 401") {
		t.Errorf("error = %v, want it to carry the HTTP status", err)
	}
}

func TestVoyageEmbedEmptyDataIsError(t *testing.T) {
	vs := newVoyageServer(t, http.StatusOK, `{"data":[]}`)
	p := newTestVoyage(vs.URL)

	if _, err := p.Embed(context.Background(), "text"); err == nil {
		t.Fatal("want error on empty data, got nil")
	}
}

// Regression for the v0.14 embeddings defect: NewAnthropic previously built a
// provider dialling api.anthropic.com/v1/embeddings — an endpoint that does
// not exist. It must now resolve to a functional Voyage provider.
func TestNewAnthropicAliasResolvesToVoyage(t *testing.T) {
	var p Provider = NewAnthropic(Config{VoyageAPIKey: "k"})
	if _, ok := p.(*VoyageProvider); !ok {
		t.Fatalf("NewAnthropic returned %T, want *VoyageProvider", p)
	}
	if got := p.Name(); got != "voyage:voyage-3" {
		t.Errorf("Name = %q, want voyage:voyage-3", got)
	}
	if got := p.Dimensions(); got != voyageDims {
		t.Errorf("Dimensions = %d, want %d", got, voyageDims)
	}
}

func TestAutoDetectVoyageMode(t *testing.T) {
	p := AutoDetect(Config{Mode: ModeVoyage})
	if got := p.Name(); got != "keyword-only" {
		t.Errorf("ModeVoyage without key: Name = %q, want keyword-only", got)
	}

	p = AutoDetect(Config{Mode: ModeVoyage, VoyageAPIKey: "k"})
	if _, ok := p.(*VoyageProvider); !ok {
		t.Errorf("ModeVoyage with key: got %T, want *VoyageProvider", p)
	}
}

// Legacy explicit mode must not construct a provider that dials with a
// credential that cannot succeed: without a Voyage key it is keyword-only,
// never doomed per-Put HTTP calls whose failures Put swallows.
func TestAutoDetectLegacyAnthropicMode(t *testing.T) {
	p := AutoDetect(Config{Mode: ModeAnthropic, AnthropicAPIKey: "sk-ant-test"})
	if got := p.Name(); got != "keyword-only" {
		t.Errorf("legacy mode without VOYAGE_API_KEY: Name = %q, want keyword-only", got)
	}

	p = AutoDetect(Config{Mode: ModeAnthropic, AnthropicAPIKey: "sk-ant-test", VoyageAPIKey: "vk"})
	if _, ok := p.(*VoyageProvider); !ok {
		t.Errorf("legacy mode with VOYAGE_API_KEY: got %T, want *VoyageProvider", p)
	}
}

func TestAutoDetectChainEndsInKeywordWithoutKeys(t *testing.T) {
	// Empty OllamaURL short-circuits the probe; no OpenAI or Voyage keys set.
	p := AutoDetect(Config{Mode: ModeAuto})
	if _, ok := p.(*KeywordProvider); !ok {
		t.Errorf("auto without keys: got %T, want *KeywordProvider", p)
	}
}
