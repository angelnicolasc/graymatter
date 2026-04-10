package graymatter

import (
	"os"
	"time"
)

// EmbeddingMode controls how GrayMatter generates vector embeddings.
type EmbeddingMode int

const (
	// EmbeddingAuto detects the best available provider at runtime.
	// Detection order: Ollama → OpenAI → Anthropic → keyword-only.
	EmbeddingAuto EmbeddingMode = iota
	// EmbeddingOllama forces Ollama (requires a running Ollama instance).
	EmbeddingOllama
	// EmbeddingAnthropic forces Anthropic API (requires ANTHROPIC_API_KEY).
	EmbeddingAnthropic
	// EmbeddingKeyword disables vector search; uses keyword+recency scoring only.
	EmbeddingKeyword
	// EmbeddingOpenAI forces OpenAI Embeddings API (requires OPENAI_API_KEY).
	EmbeddingOpenAI
)

// Config holds all GrayMatter configuration. All fields have sane defaults
// via DefaultConfig(). Zero-value Config is not valid — always call DefaultConfig().
type Config struct {
	// DataDir is the directory where gray.db and vector files are stored.
	// Default: ".graymatter"
	DataDir string

	// TopK is the maximum number of facts returned by Recall.
	// Default: 8
	TopK int

	// EmbeddingMode controls which embedding backend is used.
	// Default: EmbeddingAuto (Ollama → OpenAI → Anthropic → keyword)
	EmbeddingMode EmbeddingMode

	// OllamaURL is the base URL of the Ollama API.
	// Default: value of GRAYMATTER_OLLAMA_URL env var, or "http://localhost:11434"
	OllamaURL string

	// OllamaModel is the embedding model used with Ollama.
	// Default: value of GRAYMATTER_OLLAMA_MODEL env var, or "nomic-embed-text"
	OllamaModel string

	// AnthropicAPIKey for the Anthropic embeddings and consolidation endpoints.
	// Default: value of ANTHROPIC_API_KEY env var.
	AnthropicAPIKey string

	// OpenAIAPIKey for the OpenAI Embeddings API (text-embedding-3-small).
	// Default: value of OPENAI_API_KEY env var.
	OpenAIAPIKey string

	// OpenAIModel overrides the OpenAI embedding model.
	// Default: value of GRAYMATTER_OPENAI_MODEL env var, or "text-embedding-3-small"
	OpenAIModel string

	// ConsolidateLLM specifies which LLM provider drives memory consolidation.
	// Values: "anthropic", "ollama", "" (disable consolidation).
	// Default: "anthropic" if ANTHROPIC_API_KEY is set, else "" (disabled).
	// To use Ollama as the consolidation LLM, set this field explicitly to "ollama".
	ConsolidateLLM string

	// ConsolidateModel is the model used for consolidation summarisation.
	// Default: "claude-haiku-4-5-20251001"
	ConsolidateModel string

	// ConsolidateThreshold is the minimum fact count that triggers consolidation.
	// Default: 100
	ConsolidateThreshold int

	// DecayHalfLife is the half-life for the exponential weight decay curve.
	// Facts not accessed within this window lose half their retrieval weight.
	// Default: 720h (30 days)
	DecayHalfLife time.Duration

	// AsyncConsolidate runs consolidation in a background goroutine after Remember.
	// Default: true
	AsyncConsolidate bool

	// MaxAsyncConsolidations bounds how many consolidation goroutines may run
	// concurrently. Additional triggers while at capacity are silently dropped.
	// Default: 2
	MaxAsyncConsolidations int

	// OnConsolidateError is called when an async consolidation goroutine returns
	// an error. If nil, errors are discarded. The callback must be safe for
	// concurrent use.
	OnConsolidateError func(agentID string, err error)
}

// DefaultConfig returns a Config with all defaults applied from environment
// variables and runtime probes. Safe to call multiple times.
func DefaultConfig() Config {
	return Config{
		DataDir:              ".graymatter",
		TopK:                 8,
		EmbeddingMode:        EmbeddingAuto,
		OllamaURL:            envOrDefault("GRAYMATTER_OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:          envOrDefault("GRAYMATTER_OLLAMA_MODEL", "nomic-embed-text"),
		AnthropicAPIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIAPIKey:         os.Getenv("OPENAI_API_KEY"),
		OpenAIModel:          envOrDefault("GRAYMATTER_OPENAI_MODEL", "text-embedding-3-small"),
		ConsolidateLLM:       resolveConsolidateLLM(),
		ConsolidateModel:     "claude-haiku-4-5-20251001",
		ConsolidateThreshold: 100,
		DecayHalfLife:        720 * time.Hour,
		AsyncConsolidate:       true,
		MaxAsyncConsolidations: 2,
	}
}

// resolveConsolidateLLM returns the best available consolidation LLM based on
// environment variables. It does NOT probe network endpoints at startup.
//
// Detection order:
//  1. "anthropic" — if ANTHROPIC_API_KEY is set
//  2. ""          — disabled (set ConsolidateLLM="ollama" explicitly for Ollama)
//
// Ollama is excluded from auto-detection because probing the Ollama endpoint on
// every process startup would add 500 ms+ of latency. Configure it explicitly:
//
//	cfg := graymatter.DefaultConfig()
//	cfg.ConsolidateLLM = "ollama"
func resolveConsolidateLLM() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "anthropic"
	}
	return ""
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Config implements memory.ConsolidateConfig so it can be passed directly
// to Store.Consolidate / Store.MaybeConsolidate without an adapter.

func (c Config) GetAnthropicAPIKey() string      { return c.AnthropicAPIKey }
func (c Config) GetConsolidateLLM() string       { return c.ConsolidateLLM }
func (c Config) GetConsolidateModel() string     { return c.ConsolidateModel }
func (c Config) GetConsolidateThreshold() int    { return c.ConsolidateThreshold }
func (c Config) GetDecayHalfLife() time.Duration { return c.DecayHalfLife }
