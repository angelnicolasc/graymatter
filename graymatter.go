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

	"github.com/angelnicolasc/graymatter/internal/providerurl"
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
// Provider base URLs are selected when the handle is constructed. An invalid
// base URL for an active provider prevents store initialization.
func NewWithConfig(cfg Config) (*Memory, error) {
	cfg.AnthropicBaseURL = providerurl.Select(cfg.AnthropicBaseURL, providerurl.AnthropicEnv, providerurl.AnthropicDefault)
	cfg.OpenAIBaseURL = providerurl.Select(cfg.OpenAIBaseURL, providerurl.OpenAIEnv, providerurl.OpenAIDefault)
	cfg.VoyageBaseURL = providerurl.Select(cfg.VoyageBaseURL, providerurl.VoyageEnv, providerurl.VoyageDefault)
	if cfg.AnthropicAPIKey != "" {
		if _, err := providerurl.Parse("anthropic", cfg.AnthropicBaseURL); err != nil {
			return nil, fmt.Errorf("graymatter: %w", err)
		}
	}

	embedder := embedding.AutoDetect(embedding.Config{
		Mode:          embedding.Mode(cfg.EmbeddingMode),
		OllamaURL:     cfg.OllamaURL,
		OllamaModel:   cfg.OllamaModel,
		VoyageAPIKey:  cfg.VoyageAPIKey,
		VoyageBaseURL: cfg.VoyageBaseURL,
		OpenAIAPIKey:  cfg.OpenAIAPIKey,
		OpenAIBaseURL: cfg.OpenAIBaseURL,
		OpenAIModel:   cfg.OpenAIModel,
	})
	if initialized, ok := embedder.(interface{ InitializationError() error }); ok {
		if err := initialized.InitializationError(); err != nil {
			return nil, fmt.Errorf("graymatter: %w", err)
		}
	}
	if !cfg.ReadOnly || cfg.StrictWrite {
		if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
			return nil, fmt.Errorf("graymatter: create data dir: %w", err)
		}
	}

	store, err := memory.Open(memory.StoreConfig{
		MaxQuoteBytes:           cfg.MaxQuoteBytes,
		DataDir:                 cfg.DataDir,
		Embedder:                embedder,
		DecayHalfLife:           cfg.DecayHalfLife,
		MaxAsyncConsolidations:  cfg.MaxAsyncConsolidations,
		OnConsolidateError:      cfg.OnConsolidateError,
		OnVectorIndexError:      cfg.OnVectorIndexError,
		VectorReconcileInterval: cfg.VectorReconcileInterval,
		ReadOnly:                cfg.ReadOnly,
		StrictWrite:             cfg.StrictWrite,
		SignalWeights:           cfg.SignalWeights,
		ConfidenceWeight:        cfg.ConfidenceWeight,
		MinRelevance:            cfg.MinRelevance,
		StemKeywords:            cfg.StemKeywords,
		UsageAliasLearning:      cfg.UsageAliasLearning,
		UsageAliasAffinityMin:   cfg.UsageAliasAffinityMin,
		CandidateRetrieval:      cfg.CandidateRetrieval,
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

// RememberWithOptions stores text and its explicit confidence atomically and
// returns its durable identity. Automatic consolidation starts after commit.
func (m *Memory) RememberWithOptions(ctx context.Context, agentID, text string, options memory.WriteOptions) (memory.Fact, error) {
	return m.rememberWithOptions(ctx, agentID, text, options, m.cfg.AsyncConsolidate)
}

func (m *Memory) rememberWithOptions(ctx context.Context, agentID, text string, options memory.WriteOptions, consolidate bool) (memory.Fact, error) {
	if err := options.Validate(); err != nil {
		return memory.Fact{}, err
	}
	if m.store == nil {
		return memory.Fact{}, fmt.Errorf("graymatter: remember: unavailable store: %w", m.initErr)
	}
	fact, err := m.store.PutWithOptionsReturningFact(ctx, agentID, text, options)
	if err != nil {
		return memory.Fact{}, fmt.Errorf("graymatter: remember: %w", err)
	}
	if consolidate {
		m.store.LaunchAsyncConsolidate(agentID, m.cfg)
	}
	return fact, nil
}

// RememberSharedWithOptions writes confidence metadata in the shared namespace.
func (m *Memory) RememberSharedWithOptions(ctx context.Context, text string, options memory.WriteOptions) (memory.Fact, error) {
	// RememberShared's established policy does not launch consolidation.
	return m.rememberWithOptions(ctx, memory.SharedAgentID, text, options, false)
}

// RecallWithOptions applies a per-call confidence policy using configured TopK.
func (m *Memory) RecallWithOptions(ctx context.Context, agentID, query string, options memory.RecallOptions) (memory.RecallResult, error) {
	if err := options.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	if m.store == nil {
		return memory.RecallResult{}, fmt.Errorf("graymatter: recall: unavailable store: %w", m.initErr)
	}
	result, err := m.store.RecallWithOptions(ctx, agentID, query, m.cfg.TopK, options)
	if err != nil {
		return memory.RecallResult{}, fmt.Errorf("graymatter: recall: %w", err)
	}
	return result, nil
}

// RecallExplainWithOptions includes reconstructible ranking receipts.
func (m *Memory) RecallExplainWithOptions(ctx context.Context, agentID, query string, options memory.RecallOptions) (memory.RecallExplainResult, error) {
	if err := options.Validate(); err != nil {
		return memory.RecallExplainResult{}, err
	}
	if m.store == nil {
		return memory.RecallExplainResult{}, fmt.Errorf("graymatter: recall explain: unavailable store: %w", m.initErr)
	}
	result, err := m.store.RecallExplainWithOptions(ctx, agentID, query, m.cfg.TopK, options)
	if err != nil {
		return memory.RecallExplainResult{}, fmt.Errorf("graymatter: recall explain: %w", err)
	}
	return result, nil
}

// RecallSharedWithOptions applies the policy to shared memory.
func (m *Memory) RecallSharedWithOptions(ctx context.Context, query string, options memory.RecallOptions) (memory.RecallResult, error) {
	return m.RecallWithOptions(ctx, memory.SharedAgentID, query, options)
}

// RecallAllWithOptions applies the policy independently to both namespaces.
func (m *Memory) RecallAllWithOptions(ctx context.Context, agentID, query string, options memory.RecallOptions) (memory.RecallResult, error) {
	if err := options.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	if m.store == nil {
		return memory.RecallResult{}, fmt.Errorf("graymatter: recall all: unavailable store: %w", m.initErr)
	}
	result, err := m.store.RecallAllWithOptions(ctx, agentID, query, m.cfg.TopK, options)
	if err != nil {
		return memory.RecallResult{}, fmt.Errorf("graymatter: recall all: %w", err)
	}
	return result, nil
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

// Consolidate runs decay, pruning, and configured knowledge-graph maintenance
// for agentID. Automatic generative compaction is disabled.
//
// Remember schedules this maintenance when Config.AsyncConsolidate is true.
// Call Consolidate directly to run maintenance synchronously.
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
	result, err := memory.ExtractFactsWithProvenance(ctx, llmResponse, m.cfg)
	if err != nil {
		return fmt.Errorf("graymatter: remember extracted: %w", err)
	}
	options := memory.WriteOptions{}
	if result.Source == memory.ExtractionLLM {
		confidence := "unverified"
		options.Confidence = &confidence
	}
	for _, f := range result.Facts {
		if f == "" {
			continue
		}
		if _, err := m.store.PutWithOptionsReturningFact(ctx, agentID, f, options); err != nil {
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
	// SetKG wires an optional knowledge graph and entity extractor into the
	// underlying store (pass-through of Store.SetKG). Callers that want the
	// auto-population behaviour construct the graph and extractor themselves
	// and hand them in here.
	SetKG(graph memory.GraphAccessor, extractor memory.EntityExtractorAccessor)
	// PutConfident stores a fact with an explicit epistemic confidence
	// (verified|inferred|unverified) — pass-through of Store.PutConfident.
	PutConfident(ctx context.Context, agentID, text, confidence string) error
	// IsReadOnly reports whether the store was opened in read-only mode.
	// Mutating methods (Put, Delete, UpdateFact) return ErrStoreReadOnly when true.
	IsReadOnly() bool
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
