package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

const (
	voyageModel = "voyage-3"
	voyageDims  = 1024
)

// VoyageProvider calls the Voyage AI embeddings API (voyage-3 model).
// It maintains a small in-process cache to avoid re-embedding identical texts.
//
// Voyage AI is Anthropic's recommended embeddings partner; Anthropic itself
// has never offered an embeddings endpoint. Before v0.15 this slot dialled
// api.anthropic.com/v1/embeddings, which does not exist, so every call failed
// and Put silently degraded to keyword-only memory. See CHANGELOG v0.15.
type VoyageProvider struct {
	apiKey     string
	endpoint   string
	httpClient httpDoer
	initErr    error
	mu         sync.Mutex
	cache      map[string][]float32 // insertion-ordered eviction, see store()
	cacheOrder []string
}

// NewVoyage creates a Voyage-backed embedding provider.
func NewVoyage(cfg Config) *VoyageProvider {
	base := providerurl.Select(cfg.VoyageBaseURL, providerurl.VoyageEnv, providerurl.VoyageDefault)
	endpoint, client, err := newEmbeddingHTTPClient("voyage", base)
	return &VoyageProvider{
		apiKey:     cfg.VoyageAPIKey,
		endpoint:   endpoint,
		httpClient: client,
		initErr:    err,
		cache:      make(map[string][]float32, cacheSize),
		cacheOrder: make([]string, 0, cacheSize),
	}
}

// InitializationError reports an invalid effective base URL, if any.
func (v *VoyageProvider) InitializationError() error { return v.initErr }

// NewAnthropic returns a Voyage provider.
//
// Deprecated: Anthropic does not offer an embeddings API. This constructor
// previously built a provider that POSTed to api.anthropic.com/v1/embeddings
// — an endpoint that has never existed — and every call failed, silently
// degrading stores to keyword-only retrieval. It is kept as an alias so
// existing code compiles; point configuration at VOYAGE_API_KEY instead.
func NewAnthropic(cfg Config) *VoyageProvider { return NewVoyage(cfg) }

// AnthropicProvider is an alias for VoyageProvider.
//
// Deprecated: see NewAnthropic.
type AnthropicProvider = VoyageProvider

type voyageEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type voyageEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (v *VoyageProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if v.initErr != nil {
		return nil, v.initErr
	}
	// Check cache.
	v.mu.Lock()
	if emb, ok := v.cache[text]; ok {
		v.mu.Unlock()
		return emb, nil
	}
	v.mu.Unlock()

	body, err := json.Marshal(voyageEmbedRequest{
		Model: voyageModel,
		Input: []string{text},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.apiKey)

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody := errorBody(resp.Body)
		return nil, fmt.Errorf("voyage embed: status %d: %s", resp.StatusCode, errBody)
	}

	var result voyageEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("voyage embed decode: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("voyage embed: empty response")
	}

	emb := result.Data[0].Embedding
	v.store(text, emb)
	return emb, nil
}

func (v *VoyageProvider) store(text string, emb []float32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cacheOrder) >= cacheSize {
		oldest := v.cacheOrder[0]
		v.cacheOrder = v.cacheOrder[1:]
		delete(v.cache, oldest)
	}
	v.cache[text] = emb
	v.cacheOrder = append(v.cacheOrder, text)
}

func (v *VoyageProvider) Dimensions() int { return voyageDims }
func (v *VoyageProvider) Name() string    { return "voyage:" + voyageModel }
