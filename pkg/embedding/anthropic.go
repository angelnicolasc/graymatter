package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	anthropicEmbedURL = "https://api.anthropic.com/v1/embeddings"
	anthropicModel    = "voyage-3"
	anthropicDims     = 1024
	cacheSize         = 128
)

// AnthropicProvider calls the Anthropic embeddings API (Voyage-3 model).
// It maintains a small in-process LRU cache to avoid re-embedding identical texts.
type AnthropicProvider struct {
	apiKey     string
	httpClient *http.Client
	mu         sync.Mutex
	cache      map[string][]float32 // LRU approximation via insertion-order list
	cacheOrder []string
}

// NewAnthropic creates an Anthropic-backed embedding provider.
func NewAnthropic(cfg Config) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey: cfg.AnthropicAPIKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		cache:      make(map[string][]float32, cacheSize),
		cacheOrder: make([]string, 0, cacheSize),
	}
}

type anthropicEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type anthropicEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (a *AnthropicProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	// Check cache.
	a.mu.Lock()
	if emb, ok := a.cache[text]; ok {
		a.mu.Unlock()
		return emb, nil
	}
	a.mu.Unlock()

	body, err := json.Marshal(anthropicEmbedRequest{
		Model: anthropicModel,
		Input: []string{text},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicEmbedURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := errorBody(resp.Body)
		return nil, fmt.Errorf("anthropic embed: status %d: %s", resp.StatusCode, body)
	}

	var result anthropicEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("anthropic embed decode: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("anthropic embed: empty response")
	}

	emb := result.Data[0].Embedding
	a.store(text, emb)
	return emb, nil
}

func (a *AnthropicProvider) store(text string, emb []float32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cacheOrder) >= cacheSize {
		oldest := a.cacheOrder[0]
		a.cacheOrder = a.cacheOrder[1:]
		delete(a.cache, oldest)
	}
	a.cache[text] = emb
	a.cacheOrder = append(a.cacheOrder, text)
}

func (a *AnthropicProvider) Dimensions() int { return anthropicDims }
func (a *AnthropicProvider) Name() string    { return "anthropic:" + anthropicModel }
