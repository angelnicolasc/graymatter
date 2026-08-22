// Package embedding provides pluggable vector embedding backends for GrayMatter.
//
// Auto-detection order:
//  1. Ollama (default — fires a HEAD to /api/tags)
//  2. Anthropic API (if ANTHROPIC_API_KEY is set)
//  3. Keyword-only fallback (zero network deps)
package embedding

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxErrorBodyBytes caps how much of a non-200 response body goes into an
// error message.
//
// The three HTTP providers used to io.ReadAll an error body whole. The Ollama
// URL is user-configurable and the others sit behind whatever proxy the
// network imposes, so a hostile or broken endpoint could answer 500 with an
// endless chunked body and exhaust the memory of whatever is embedding. Eight
// kibibytes is more than any real error message needs.
const maxErrorBodyBytes = 8 << 10

// errorBody reads a bounded prefix of an error response for use in a message.
func errorBody(r io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
	if err != nil && len(data) == 0 {
		return fmt.Sprintf("<unreadable: %v>", err)
	}
	return string(data)
}

// Mode mirrors graymatter.EmbeddingMode to avoid circular imports.
type Mode int

const (
	ModeAuto     Mode = iota
	ModeOllama
	ModeAnthropic
	ModeKeyword
	ModeOpenAI
)

// Provider generates float32 vector embeddings from text.
// A nil return from Embed signals keyword-only mode to the caller.
type Provider interface {
	// Embed returns a vector for text. Returns nil if unavailable.
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dimensions returns the embedding dimension (0 for keyword provider).
	Dimensions() int
	// Name is a human-readable identifier used in logs and stats.
	Name() string
}

// Config carries provider configuration from graymatter.Config.
type Config struct {
	Mode            Mode
	OllamaURL       string
	OllamaModel     string
	AnthropicAPIKey string
	OpenAIAPIKey    string
	OpenAIModel     string // defaults to text-embedding-3-small
}

// AutoDetect selects the best available Provider given cfg.
// It probes network endpoints with a short timeout so startup is fast.
func AutoDetect(cfg Config) Provider {
	switch cfg.Mode {
	case ModeOllama:
		return NewOllama(cfg)
	case ModeAnthropic:
		if cfg.AnthropicAPIKey != "" {
			return NewAnthropic(cfg)
		}
		return NewKeyword()
	case ModeOpenAI:
		if cfg.OpenAIAPIKey != "" {
			return NewOpenAI(cfg)
		}
		return NewKeyword()
	case ModeKeyword:
		return NewKeyword()
	default: // ModeAuto
		if ollamaReachable(cfg.OllamaURL) {
			return NewOllama(cfg)
		}
		if cfg.OpenAIAPIKey != "" {
			return NewOpenAI(cfg)
		}
		if cfg.AnthropicAPIKey != "" {
			return NewAnthropic(cfg)
		}
		return NewKeyword()
	}
}

// ollamaReachable does a fast HEAD probe to check if Ollama is up.
func ollamaReachable(baseURL string) bool {
	if baseURL == "" {
		return false
	}
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(baseURL + "/api/tags")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}
