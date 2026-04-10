// Package graymatter provides persistent memory for Go AI agents.
//
// Single static binary. Zero infra. Three public functions.
//
//	mem := graymatter.New(".graymatter")
//	mem.Remember("agent", "user prefers bullet points")
//	ctx := mem.Recall("agent", "how should I format this?")
//	// ctx is a []string ready to inject into a system prompt
package graymatter

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Memory is the primary handle for GrayMatter operations.
// It is safe for concurrent use.
type Memory struct {
	store    *memory.Store
	embedder embedding.Provider
	cfg      Config
}

// New creates a Memory with default configuration rooted at dataDir.
// If initialisation fails, it logs the error to stderr and returns a
// no-op Memory that never panics (callers need not check for nil).
func New(dataDir string) *Memory {
	cfg := DefaultConfig()
	cfg.DataDir = dataDir
	m, err := NewWithConfig(cfg)
	if err != nil {
		log.Printf("graymatter: init error (running in no-op mode): %v", err)
		return &Memory{cfg: cfg}
	}
	return m
}

// NewWithConfig creates a Memory with explicit configuration.
// Returns an error if the data directory cannot be created or the
// database cannot be opened.
func NewWithConfig(cfg Config) (*Memory, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("graymatter: create data dir: %w", err)
	}

	embedder := embedding.AutoDetect(embedding.Config{
		Mode:            embedding.Mode(cfg.EmbeddingMode),
		OllamaURL:       cfg.OllamaURL,
		OllamaModel:     cfg.OllamaModel,
		AnthropicAPIKey: cfg.AnthropicAPIKey,
		OpenAIAPIKey:    cfg.OpenAIAPIKey,
		OpenAIModel:     cfg.OpenAIModel,
	})

	store, err := memory.Open(memory.StoreConfig{
		DataDir:               cfg.DataDir,
		Embedder:              embedder,
		DecayHalfLife:         cfg.DecayHalfLife,
		MaxAsyncConsolidations: cfg.MaxAsyncConsolidations,
		OnConsolidateError:    cfg.OnConsolidateError,
	})
	if err != nil {
		return nil, fmt.Errorf("graymatter: open store: %w", err)
	}

	return &Memory{
		store:    store,
		embedder: embedder,
		cfg:      cfg,
	}, nil
}

// Remember stores an observation associated with agentID.
// It is safe to call Remember concurrently from multiple goroutines.
//
//	mem.Remember("sales-closer", "Maria didn't reply Wednesday. Third touchpoint due Friday.")
func (m *Memory) Remember(agentID, text string) error {
	if m.store == nil {
		return nil // no-op mode
	}
	if err := m.store.Put(context.Background(), agentID, text); err != nil {
		return fmt.Errorf("graymatter: remember: %w", err)
	}
	if m.cfg.AsyncConsolidate {
		m.store.LaunchAsyncConsolidate(agentID, m.cfg)
	}
	return nil
}

// Recall returns the top-k most relevant facts for agentID given query.
// The returned []string is ready to be joined and injected into a system prompt.
//
//	ctx := mem.Recall("sales-closer", "follow up Maria")
//	systemPrompt += "\n\n## Memory\n" + strings.Join(ctx, "\n")
func (m *Memory) Recall(agentID, query string) ([]string, error) {
	if m.store == nil {
		return nil, nil // no-op mode
	}
	facts, err := m.store.Recall(context.Background(), agentID, query, m.cfg.TopK)
	if err != nil {
		return nil, fmt.Errorf("graymatter: recall: %w", err)
	}
	return facts, nil
}

// Consolidate summarises and compacts memories for agentID.
// It calls the configured LLM to produce summary facts, applies the
// exponential decay curve, and prunes dead facts.
//
// Consolidate is automatically triggered async after Remember when
// Config.AsyncConsolidate is true. Call it manually for synchronous control.
func (m *Memory) Consolidate(ctx context.Context, agentID string) error {
	if m.store == nil {
		return nil
	}
	return m.store.Consolidate(ctx, agentID, m.cfg)
}

// RememberShared stores an observation in the shared memory namespace,
// readable by all agents via RecallShared and RecallAll.
func (m *Memory) RememberShared(text string) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.PutShared(context.Background(), text); err != nil {
		return fmt.Errorf("graymatter: remember shared: %w", err)
	}
	return nil
}

// RecallShared returns the top-k most relevant shared facts for query.
func (m *Memory) RecallShared(query string) ([]string, error) {
	if m.store == nil {
		return nil, nil
	}
	facts, err := m.store.RecallShared(context.Background(), query, m.cfg.TopK)
	if err != nil {
		return nil, fmt.Errorf("graymatter: recall shared: %w", err)
	}
	return facts, nil
}

// RecallAll merges agent-scoped and shared memory results for agentID,
// deduplicates, and returns at most TopK combined facts.
func (m *Memory) RecallAll(agentID, query string) ([]string, error) {
	if m.store == nil {
		return nil, nil
	}
	facts, err := m.store.RecallAll(context.Background(), agentID, query, m.cfg.TopK)
	if err != nil {
		return nil, fmt.Errorf("graymatter: recall all: %w", err)
	}
	return facts, nil
}

// Extract calls the configured LLM and returns atomic facts distilled from
// llmResponse. Each returned string is a self-contained declarative sentence
// suitable for passing directly to Remember.
//
// Requires an Anthropic API key. Without one, Extract returns the raw response
// as a single-element slice so the caller always receives a usable result.
//
//	facts, _ := mem.Extract(ctx, assistantReply)
//	for _, f := range facts {
//	    mem.Remember("agent", f)
//	}
func (m *Memory) Extract(ctx context.Context, llmResponse string) ([]string, error) {
	if m.store == nil {
		return nil, nil
	}
	facts, err := memory.ExtractFacts(ctx, llmResponse, m.cfg)
	if err != nil {
		return nil, fmt.Errorf("graymatter: extract: %w", err)
	}
	return facts, nil
}

// RememberExtracted combines Extract and Remember in a single call: it extracts
// atomic facts from llmResponse and stores each one for agentID.
// This is the idiomatic replacement for the extractKeyFacts() pattern shown
// in the README.
//
//	mem.RememberExtracted(ctx, "sales-closer", assistantReply)
func (m *Memory) RememberExtracted(ctx context.Context, agentID, llmResponse string) error {
	if m.store == nil {
		return nil
	}
	facts, err := memory.ExtractFacts(ctx, llmResponse, m.cfg)
	if err != nil {
		return fmt.Errorf("graymatter: remember extracted: %w", err)
	}
	for _, f := range facts {
		if f == "" {
			continue
		}
		if err := m.store.Put(ctx, agentID, f); err != nil {
			return fmt.Errorf("graymatter: remember extracted put: %w", err)
		}
		if m.cfg.AsyncConsolidate {
			m.store.LaunchAsyncConsolidate(agentID, m.cfg)
		}
	}
	return nil
}

// Close flushes pending writes and closes the underlying database.
// Always call Close when done; failing to do so may leave gray.db locked.
func (m *Memory) Close() error {
	if m.store == nil {
		return nil
	}
	return m.store.Close()
}

// Store exposes the internal Store for advanced use (CLI, MCP, TUI).
// Callers outside the graymatter package use this to access full CRUD.
func (m *Memory) Store() *memory.Store {
	return m.store
}

// Config returns the active configuration.
func (m *Memory) Config() Config {
	return m.cfg
}
