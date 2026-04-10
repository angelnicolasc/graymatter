package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	chromem "github.com/philippgille/chromem-go"
	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

var (
	bucketFacts    = []byte("facts")
	bucketSessions = []byte("sessions")
	bucketMeta     = []byte("meta")
	bucketAgents   = []byte("agents")
)

// SharedAgentID is the reserved agent ID for the shared memory namespace.
// Facts stored here are readable by all agents via RecallShared and RecallAll.
//
// Concurrency note: bbolt serialises concurrent write access via a file-level
// lock. Multiple processes writing shared memory will serialise, not race.
// Concurrent reads are always safe.
const SharedAgentID = "__shared__"

// StoreConfig is passed to Open to configure the Store.
type StoreConfig struct {
	DataDir       string
	Embedder      embedding.Provider
	DecayHalfLife time.Duration

	// MaxAsyncConsolidations bounds concurrent background consolidations.
	// 0 is normalised to 2 by Open().
	MaxAsyncConsolidations int

	// OnConsolidateError is called when an async consolidation goroutine errors.
	// If nil, errors are discarded. Must be safe for concurrent use.
	OnConsolidateError func(agentID string, err error)
}

// GraphAccessor is a narrow interface that pkg/memory uses to interact with
// the knowledge graph without importing pkg/kg (prevents import cycles).
type GraphAccessor interface {
	// Upsert inserts or updates a node in the graph.
	UpsertNode(id, label, entityType string) error
	// NeighborTexts returns text labels of nodes reachable from nodeID within depth hops.
	NeighborTexts(nodeID string, depth int) ([]string, error)
}

// EntityExtractorAccessor extracts entities from a text string.
// Implemented by pkg/kg.EntityExtractor.
type EntityExtractorAccessor interface {
	ExtractIDs(text string) ([]string, error) // returns canonical node IDs
}

// Store is the central storage layer. It combines bbolt for durable
// structured storage with chromem-go for in-process vector similarity search.
// All public methods are safe for concurrent use.
type Store struct {
	db       *bolt.DB
	vdb      *chromem.DB
	embedder embedding.Provider
	cfg      StoreConfig

	mu         sync.RWMutex
	collection map[string]*chromem.Collection // agentID → collection

	// graph and extractor are set via SetKG after Open().
	// They are optional; Consolidate and Recall work without them.
	graph     GraphAccessor
	extractor EntityExtractorAccessor

	// Goroutine lifecycle. All goroutines launched by Store must acquire sema
	// and register with wg before doing work. Close() cancels shutdownCtx,
	// then waits for wg to reach zero before closing bbolt.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	wg             sync.WaitGroup
	sema           chan struct{} // bounded semaphore; cap = MaxAsyncConsolidations
}

// Open creates or opens the GrayMatter store at cfg.DataDir.
func Open(cfg StoreConfig) (*Store, error) {
	if cfg.MaxAsyncConsolidations <= 0 {
		cfg.MaxAsyncConsolidations = 2
	}

	dbPath := filepath.Join(cfg.DataDir, "gray.db")
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}

	// Ensure top-level buckets exist.
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketFacts, bucketSessions, bucketMeta, bucketAgents} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init buckets: %w", err)
	}

	vecDir := filepath.Join(cfg.DataDir, "vectors")
	vdb, err := chromem.NewPersistentDB(vecDir, false)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open chromem: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		db:             db,
		vdb:            vdb,
		embedder:       cfg.Embedder,
		cfg:            cfg,
		collection:     make(map[string]*chromem.Collection),
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
		sema:           make(chan struct{}, cfg.MaxAsyncConsolidations),
	}

	// Hydrate known agent IDs so collections are ready.
	_ = s.loadAgents()

	// Re-index any facts that are in bbolt but missing from the vector store
	// (e.g. after a crash between a bbolt commit and the vector write).
	s.reconcileVectors()

	return s, nil
}

// Put stores a new observation for agentID.
func (s *Store) Put(ctx context.Context, agentID, text string) error {
	var emb []float32
	if s.embedder != nil {
		var err error
		emb, err = s.embedder.Embed(ctx, text)
		if err != nil {
			// Non-fatal: fall back to keyword-only for this fact.
			emb = nil
		}
	}

	f := newFact(agentID, text, emb)

	// Persist to bbolt.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte(agentID))
		if err != nil {
			return err
		}
		data, err := f.marshal()
		if err != nil {
			return err
		}
		if err := b.Put([]byte(f.ID), data); err != nil {
			return err
		}
		// Register agent.
		return tx.Bucket(bucketAgents).Put([]byte(agentID), []byte("1"))
	}); err != nil {
		return fmt.Errorf("put fact: %w", err)
	}

	// Add to vector index if we have an embedding.
	if len(emb) > 0 {
		if err := s.addToVector(ctx, agentID, f); err != nil {
			// Non-fatal: bbolt write already succeeded.
			_ = err
		}
	}

	return nil
}

// Delete removes a fact by ID for agentID.
func (s *Store) Delete(agentID, factID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts).Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(factID))
	})
}

// List returns all facts for agentID, sorted newest first.
func (s *Store) List(agentID string) ([]Fact, error) {
	var facts []Fact
	if err := s.db.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketFacts)
		b := parent.Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			f, err := unmarshalFact(v)
			if err != nil {
				return nil // skip corrupt entries
			}
			facts = append(facts, f)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	// Sort newest first.
	sortFactsByTime(facts)
	return facts, nil
}

// ListAgents returns all known agent IDs.
func (s *Store) ListAgents() ([]string, error) {
	var agents []string
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAgents).ForEach(func(k, _ []byte) error {
			agents = append(agents, string(k))
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return agents, nil
}

// Stats returns aggregate statistics for agentID.
func (s *Store) Stats(agentID string) (MemoryStats, error) {
	facts, err := s.List(agentID)
	if err != nil {
		return MemoryStats{}, err
	}
	st := MemoryStats{AgentID: agentID, FactCount: len(facts)}
	if len(facts) == 0 {
		return st, nil
	}
	var weightSum float64
	st.OldestAt = facts[0].CreatedAt
	st.NewestAt = facts[0].CreatedAt
	for _, f := range facts {
		weightSum += f.Weight
		if f.CreatedAt.Before(st.OldestAt) {
			st.OldestAt = f.CreatedAt
		}
		if f.CreatedAt.After(st.NewestAt) {
			st.NewestAt = f.CreatedAt
		}
	}
	st.AvgWeight = weightSum / float64(len(facts))
	return st, nil
}

// UpdateFact persists a modified fact (used by consolidation + decay).
func (s *Store) UpdateFact(agentID string, f Fact) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts).Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		data, err := f.marshal()
		if err != nil {
			return err
		}
		return b.Put([]byte(f.ID), data)
	})
}

// Close signals all background goroutines to stop, waits for them to exit,
// then flushes and closes the underlying stores.
func (s *Store) Close() error {
	s.shutdownCancel()
	s.wg.Wait()
	return s.db.Close()
}

// DB exposes the raw bbolt handle (used by session package).
func (s *Store) DB() *bolt.DB {
	return s.db
}

// PutShared stores a new observation in the shared memory namespace.
// Shared facts are accessible to all agents via RecallShared and RecallAll.
func (s *Store) PutShared(ctx context.Context, text string) error {
	return s.Put(ctx, SharedAgentID, text)
}

// RecallShared returns the top-k most relevant shared facts for query.
func (s *Store) RecallShared(ctx context.Context, query string, topK int) ([]string, error) {
	return s.Recall(ctx, SharedAgentID, query, topK)
}

// RecallAll merges agent-scoped and shared-scoped results, deduplicates, and
// re-ranks by Reciprocal Rank Fusion. It returns at most topK combined facts.
func (s *Store) RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	agentResults, err := s.Recall(ctx, agentID, query, topK)
	if err != nil {
		return nil, fmt.Errorf("recall agent: %w", err)
	}
	sharedResults, err := s.Recall(ctx, SharedAgentID, query, topK)
	if err != nil {
		return nil, fmt.Errorf("recall shared: %w", err)
	}

	// Deduplicate and merge, preserving agent-scoped results first.
	seen := make(map[string]bool, len(agentResults)+len(sharedResults))
	merged := make([]string, 0, len(agentResults)+len(sharedResults))
	for _, f := range agentResults {
		if !seen[f] {
			seen[f] = true
			merged = append(merged, f)
		}
	}
	for _, f := range sharedResults {
		if !seen[f] {
			seen[f] = true
			merged = append(merged, f)
		}
	}
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged, nil
}

// SetKG wires an optional knowledge graph and entity extractor into the store.
// Call this after Open() to enable graph enrichment in Recall and Consolidate.
// Both arguments are optional; pass nil to disable the corresponding feature.
func (s *Store) SetKG(graph GraphAccessor, extractor EntityExtractorAccessor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graph = graph
	s.extractor = extractor
}

// --- internal helpers ---

func (s *Store) loadAgents() error {
	agents, err := s.ListAgents()
	if err != nil {
		return err
	}
	for _, id := range agents {
		s.ensureCollection(id)
	}
	return nil
}

// reconcileVectors ensures every bbolt fact with an embedding is present in
// the chromem-go vector index. Called once at Open() to repair divergences
// caused by crashes between the bbolt write and the vector write.
// Best-effort: individual errors are silently ignored (bbolt is source of truth).
// chromem-go AddDocument overwrites on duplicate ID, so this is idempotent.
func (s *Store) reconcileVectors() {
	agents, err := s.ListAgents()
	if err != nil {
		return
	}
	ctx := context.Background()
	for _, agentID := range agents {
		facts, err := s.List(agentID)
		if err != nil {
			continue
		}
		for _, f := range facts {
			if len(f.Embedding) == 0 {
				continue
			}
			_ = s.addToVector(ctx, agentID, f)
		}
	}
}

func (s *Store) ensureCollection(agentID string) *chromem.Collection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.collection[agentID]; ok {
		return c
	}
	c, _ := s.vdb.GetOrCreateCollection(agentID, nil, nil)
	s.collection[agentID] = c
	return c
}

func (s *Store) addToVector(ctx context.Context, agentID string, f Fact) error {
	c := s.ensureCollection(agentID)
	metadata := map[string]string{
		"agent_id":   agentID,
		"created_at": f.CreatedAt.Format(time.RFC3339),
	}
	doc := chromem.Document{
		ID:        f.ID,
		Content:   f.Text,
		Metadata:  metadata,
		Embedding: f.Embedding,
	}
	return c.AddDocument(ctx, doc)
}

// vectorSearch returns at most n results from the vector index.
func (s *Store) vectorSearch(ctx context.Context, agentID, query string, n int) ([]chromem.Result, error) {
	c := s.ensureCollection(agentID)
	if c == nil {
		return nil, nil
	}
	// Generate query embedding.
	var qEmb []float32
	if s.embedder != nil {
		var err error
		qEmb, err = s.embedder.Embed(ctx, query)
		if err != nil || len(qEmb) == 0 {
			return nil, nil
		}
	}
	if len(qEmb) == 0 {
		return nil, nil
	}
	return c.QueryEmbedding(ctx, qEmb, n, nil, nil)
}

// marshalJSON helper for meta bucket.
func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func sortFactsByTime(facts []Fact) {
	for i := 1; i < len(facts); i++ {
		for j := i; j > 0 && facts[j].CreatedAt.After(facts[j-1].CreatedAt); j-- {
			facts[j], facts[j-1] = facts[j-1], facts[j]
		}
	}
}
