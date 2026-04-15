// Package graymatter provides persistent memory for Go AI agents.
//
// Single static binary. Zero infra. Three public functions.
//
//	mem := graymatter.New(".graymatter")
//	mem.Remember(ctx, "agent", "user prefers bullet points")
//	facts, _ := mem.Recall(ctx, "agent", "how should I format this?")
//	// facts is a []string ready to inject into a system prompt
package graymatter

import (
	"context"
	"fmt"
	"log"
	"os"

	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Memory is the primary handle for GrayMatter operations.
// It is safe for concurrent use.
type Memory struct {
	store    *memory.Store
	embedder embedding.Provider
	cfg      Config
	initErr  error // non-nil iff this Memory is in degraded no-op mode
}

// Status describes the runtime health of a Memory handle.
//
// Production callers should branch on Healthy before treating the handle as a
// real persistence layer — a no-op Memory silently accepts every Remember and
// always returns zero results from Recall.
type Status struct {
	// Healthy is true when the underlying store opened successfully.
	Healthy bool
	// Mode is "operational" or "noop".
	Mode string
	// InitError is the error returned by the underlying store at construction
	// time. nil for healthy handles.
	InitError error
	// DataDir is the directory the handle was constructed with.
	DataDir string
}

// New creates a Memory with default configuration rooted at dataDir.
//
// If initialisation fails (e.g. the data dir is unwritable, bbolt is locked,
// or the vector store cannot be opened) New does NOT panic and does NOT return
// an error: it logs the failure to stderr and returns a degraded Memory whose
// methods all become no-ops. This convenience contract is intended for demos,
// prototypes, and test harnesses where the caller wants a single-line setup.
//
// PRODUCTION CALLERS MUST verify the handle before relying on it:
//
//	mem := graymatter.New(".graymatter")
//	if !mem.Healthy() {
//	    log.Fatalf("graymatter: %v", mem.Status().InitError)
//	}
//
// Or, equivalently, use NewWithConfig which surfaces the error directly.
func New(dataDir string) *Memory {
	cfg := DefaultConfig()
	cfg.DataDir = dataDir
	m, err := NewWithConfig(cfg)
	if err != nil {
		log.Printf("graymatter: init error (running in no-op mode): %v", err)
		return &Memory{cfg: cfg, initErr: err}
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
		DataDir:                 cfg.DataDir,
		Embedder:                embedder,
		DecayHalfLife:           cfg.DecayHalfLife,
		MaxAsyncConsolidations:  cfg.MaxAsyncConsolidations,
		OnConsolidateError:      cfg.OnConsolidateError,
		OnVectorIndexError:      cfg.OnVectorIndexError,
		VectorReconcileInterval: cfg.VectorReconcileInterval,
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

// Healthy reports whether this Memory is backed by a real store. Returns false
// for handles produced by New() when the underlying store failed to open.
func (m *Memory) Healthy() bool {
	return m.store != nil
}

// Status returns a snapshot of the Memory's runtime health.
func (m *Memory) Status() Status {
	if m.store != nil {
		return Status{
			Healthy: true,
			Mode:    "operational",
			DataDir: m.cfg.DataDir,
		}
	}
	return Status{
		Healthy:   false,
		Mode:      "noop",
		InitError: m.initErr,
		DataDir:   m.cfg.DataDir,
	}
}

// Remember stores an observation associated with agentID.
// It is safe to call Remember concurrently from multiple goroutines.
//
//	mem.Remember(ctx, "sales-closer", "Maria didn't reply Wednesday. Third touchpoint due Friday.")
func (m *Memory) Remember(ctx context.Context, agentID, text string) error {
	if m.store == nil {
		return nil // no-op mode
	}
	if err := m.store.Put(ctx, agentID, text); err != nil {
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
//	facts, _ := mem.Recall(ctx, "sales-closer", "follow up Maria")
//	systemPrompt += "\n\n## Memory\n" + strings.Join(facts, "\n")
func (m *Memory) Recall(ctx context.Context, agentID, query string) ([]string, error) {
	if m.store == nil {
		return nil, nil // no-op mode
	}
	facts, err := m.store.Recall(ctx, agentID, query, m.cfg.TopK)
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
func (m *Memory) RememberShared(ctx context.Context, text string) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.PutShared(ctx, text); err != nil {
		return fmt.Errorf("graymatter: remember shared: %w", err)
	}
	return nil
}

// RecallShared returns the top-k most relevant shared facts for query.
func (m *Memory) RecallShared(ctx context.Context, query string) ([]string, error) {
	if m.store == nil {
		return nil, nil
	}
	facts, err := m.store.RecallShared(ctx, query, m.cfg.TopK)
	if err != nil {
		return nil, fmt.Errorf("graymatter: recall shared: %w", err)
	}
	return facts, nil
}

// RecallAll merges agent-scoped and shared memory results for agentID,
// deduplicates, and returns at most TopK combined facts.
func (m *Memory) RecallAll(ctx context.Context, agentID, query string) ([]string, error) {
	if m.store == nil {
		return nil, nil
	}
	facts, err := m.store.RecallAll(ctx, agentID, query, m.cfg.TopK)
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
//	    mem.Remember(ctx, "agent", f)
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

// AdvancedStore is the narrow interface exposed to advanced callers (CLI, MCP,
// TUI) that need direct access to CRUD, listing, and raw bbolt operations.
//
// This interface is intentionally minimal: every method here is a public API
// commitment. New advanced features should add methods here only when there is
// no Memory-level equivalent. Internal refactors of *memory.Store remain free
// as long as this contract is preserved.
type AdvancedStore interface {
	// Put writes a fact directly bypassing extraction/consolidation.
	Put(ctx context.Context, agentID, text string) error
	// PutShared writes a fact to the shared namespace.
	PutShared(ctx context.Context, text string) error
	// Recall is the raw store-level recall (bypasses Memory.cfg.TopK).
	Recall(ctx context.Context, agentID, query string, topK int) ([]string, error)
	// RecallShared returns top-K shared facts for query.
	RecallShared(ctx context.Context, query string, topK int) ([]string, error)
	// List returns every fact for an agent, newest first.
	List(agentID string) ([]memory.Fact, error)
	// ListAgents returns every known agent ID.
	ListAgents() ([]string, error)
	// Stats returns aggregate fact statistics for an agent.
	Stats(agentID string) (memory.MemoryStats, error)
	// Delete removes a single fact by ID.
	Delete(agentID, factID string) error
	// UpdateFact persists a modified fact (used by CLI edit commands).
	UpdateFact(agentID string, f memory.Fact) error
	// Consolidate runs synchronous consolidation for an agent.
	Consolidate(ctx context.Context, agentID string, cfg memory.ConsolidateConfig) error
	// PendingVectorCount returns the number of facts waiting to be re-indexed
	// after a previous failure. Non-zero in a quiescent system means the vector
	// backend is unhealthy.
	PendingVectorCount() int
	// DB exposes the raw bbolt handle for the session/checkpoint subsystems.
	// New callers should prefer higher-level methods; this is an escape hatch.
	DB() *bolt.DB
}

// Advanced returns a narrow handle for advanced operations needed by the CLI,
// MCP server, and TUI. Returns nil for a no-op Memory.
//
// Prefer this over the (deprecated) direct concrete-type accessor: it lets us
// refactor the underlying store without breaking the public API surface.
func (m *Memory) Advanced() AdvancedStore {
	if m.store == nil {
		return nil
	}
	return m.store
}

// Config returns the active configuration.
func (m *Memory) Config() Config {
	return m.cfg
}

// _ ensures *memory.Store satisfies AdvancedStore at compile time.
var _ AdvancedStore = (*memory.Store)(nil)
