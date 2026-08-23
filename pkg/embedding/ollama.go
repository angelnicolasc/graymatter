package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OllamaProvider calls the local Ollama HTTP API to generate embeddings.
// Default model: nomic-embed-text (768 dimensions).
type OllamaProvider struct {
	baseURL    string
	model      string
	httpClient *http.Client
	dims       int
}

// NewOllama creates an Ollama-backed embedding provider.
func NewOllama(cfg Config) *OllamaProvider {
	url := cfg.OllamaURL
	if url == "" {
		url = "http://localhost:11434"
	}
	model := cfg.OllamaModel
	if model == "" {
		model = "nomic-embed-text"
	}
	return &OllamaProvider{
		baseURL: url,
		model:   model,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		dims: 768, // nomic-embed-text default; updated on first successful call
	}
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
}

func (o *OllamaProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(ollamaEmbedRequest{Model: o.model, Prompt: text})
	if err != nil {
		return nil, err
	}

	// Retry up to 3 attempts with exponential backoff.
	// Each attempt rebuilds the request because the body reader is consumed.
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
				backoff *= 2
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			o.baseURL+"/api/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := o.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body := errorBody(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("ollama embed: status %d: %s", resp.StatusCode, body)
			continue
		}

		var result ollamaEmbedResponse
		decErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if decErr != nil {
			lastErr = fmt.Errorf("ollama embed decode: %w", decErr)
			continue
		}

		if len(result.Embedding) > 0 {
			o.dims = len(result.Embedding)
		}
		return result.Embedding, nil
	}

	return nil, fmt.Errorf("ollama embed: %w", lastErr)
}

func (o *OllamaProvider) Dimensions() int { return o.dims }
func (o *OllamaProvider) Name() string    { return "ollama:" + o.model }
