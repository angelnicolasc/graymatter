package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

var (
	bucketFacts         = []byte("facts")
	bucketSessions      = []byte("sessions")
	bucketMeta          = []byte("meta")
	bucketAgents        = []byte("agents")
	bucketPendingVector = []byte("pending_vector")
	// bucketKGExtracted is the extraction watermark (A7): key
	// "<agentID>\x00<factID>" → text signature of the last successfully
	// extracted version of the fact, so consolidation passes are incremental.
	bucketKGExtracted = []byte("kg_extracted")

	// ErrStoreReadOnly is returned by mutating methods when the store was opened
	// in read-only mode (e.g. another process holds the write lock).
	ErrStoreReadOnly = errors.New("store is read-only")
)

// boltOpenTimeout is the maximum time bolt.Open waits to acquire the file lock.
// Overridable in tests to speed up lock-contention scenarios.
var boltOpenTimeout = 2 * time.Second

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

	// VectorBackend overrides the default chromem-go vector store.
	// If nil, a persistent chromem-go instance is created under DataDir/vectors.
	// Use this to plug in Qdrant, Weaviate, pgvector, or any VectorStore impl.
	VectorBackend VectorStore

	// MaxAsyncConsolidations bounds concurrent background consolidations.
	// 0 is normalised to 2 by Open().
	MaxAsyncConsolidations int

	// OnConsolidateError is called when an async consolidation goroutine errors.
	// If nil, errors are discarded. Must be safe for concurrent use.
	OnConsolidateError func(agentID string, err error)

	// OnVectorIndexError is called when an inline vector upsert fails after the
	// bbolt write has already committed. The fact remains in the pending-vector
	// queue and will be retried by the background reconciler. Must be safe for
	// concurrent use.
	OnVectorIndexError func(agentID, factID string, err error)

	// VectorReconcileInterval controls how often the background reconciler
	// drains the pending-vector queue. 0 disables the background loop entirely
	// (Open() still runs one drain at startup).
	VectorReconcileInterval time.Duration

	// ReadOnly opens the store in read-only mode from the start, skipping all
	// mutating operations. When false (default), Open() automatically falls back
	// to read-only if the write lock is held by another process.
	ReadOnly bool

	// StrictWrite disables the automatic read-only fallback: if the write lock
	// cannot be acquired, Open fails immediately with the lock-holder error
	// instead of degrading. The daemon uses this — a store owner that silently
	// came up read-only would break every connected client. Mutually exclusive
	// with ReadOnly (StrictWrite wins).
	StrictWrite bool

	// SignalWeights sets how much each retrieval signal contributes to the
	// fused ranking. nil — the zero value, and what every caller before
	// v0.10.0 passes — means DefaultSignalWeights(), which is bit-for-bit the
	// behaviour that was hardcoded until then.
	//
	// Set it to run the ranking as something other than a hybrid: all weight
	// on Recency turns Recall into a sliding window over the K most recent
	// facts, all weight on Keyword ignores age entirely. See
	// docs/decisions/006-configurable-signal-weights.md.
	SignalWeights *SignalWeights

	// MinRelevance drops results scoring below this fraction of the best
	// score in the same result set. 0 — the zero value — disables the cut
	// and returns exactly topK, which is the pre-v0.10.0 contract.
	//
	// The threshold is relative because RRF scores are not comparable across
	// stores: the same fact scores differently depending on how many facts
	// were ranked alongside it, so an absolute cutoff would quietly mean
	// something different as a store grows. 0.5 keeps results at least half as
	// strong as the best match; the best match always survives.
	MinRelevance float64

	// OnRecall, if non-nil, is called after each Recall with timing and count.
	OnRecall func(agentID, query string, resultCount int, duration time.Duration)

	// OnPut, if non-nil, is called after each successful Put.
	OnPut func(agentID, factID string, duration time.Duration)

	// Logger receives structured log events. Uses log.Default() if nil.
	Logger interface {
		Printf(format string, v ...any)
	}
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

// EntityRef carries the identity and classification of one extracted entity.
type EntityRef struct {
	ID         string
	Label      string
	EntityType string
}

// EntityLink is a co-mention relationship between two extracted entities.
// Sources carries the fact IDs that produced the link (set by consolidation).
type EntityLink struct {
	From     string
	To       string
	Relation string
	Sources  []string
}

// TypedEntityExtractor is an optional capability: extractors that preserve
// the label and entity type of each entity, and produce co-mention links,
// implement this in addition to EntityExtractorAccessor. Consolidation uses
// it when present; when absent, the legacy ID-only path runs unchanged.
//
// Added in v0.12.0.
type TypedEntityExtractor interface {
	ExtractTyped(text string) ([]EntityRef, []EntityLink, error)
}

// EdgeWriter is an optional graph capability: the ability to create edges.
// The knowledge-graph adapter implements it; consolidation uses it only when
// the extractor also implements TypedEntityExtractor.
//
// Added in v0.12.0.
type EdgeWriter interface {
	// LinkEdges persists the given co-mention links, attributing them to
	// sourceFactID so every connection keeps its receipts.
	LinkEdges(links []EntityLink, sourceFactID string) error
}

// Store is the central storage layer. It combines bbolt for durable
// structured storage with a pluggable VectorStore for similarity search.
// All public methods are safe for concurrent use.
type Store struct {
	db       *bolt.DB
	vectors  VectorStore
	embedder embedding.Provider
	cfg      StoreConfig
	readOnly bool

	mu sync.RWMutex

	// now reads the wall clock. It exists so tests can freeze time.
	//
	// Three things the store computes are functions of "what time is it":
	// a new fact's CreatedAt/AccessedAt, the recency signal in Recall, and
	// the decay exponent in Consolidate. On the real clock all three drift
	// between runs, which makes byte-for-byte comparison of store output
	// impossible — and the drift is not float noise: decay is 2^(-dt/30d),
	// so a second of separation between writing a corpus and consolidating
	// it moves every weight by ~2.7e-7 relative. That is hundreds of times
	// larger than any plausible rounding tolerance, so a golden test would
	// flake rather than hold.
	//
	// Set to time.Now by Open() and never nil. Assign before any concurrent
	// use; it is not guarded, because production never writes it.
	//
	// Deliberately NOT used for the elapsed-time measurements that feed
	// OnRecall and OnPut. Those are stopwatches, not clock reads — a frozen
	// clock would report every operation as taking zero.
	now func() time.Time

	// debugRanking, if non-nil, receives the fused ranking from Recall before
	// topK truncation. Test-only seam; production never sets it, and nothing
	// reads it outside pkg/memory. See the call site in recall.go for why the
	// golden fixture needs the scores and not just the resulting order.
	debugRanking func(query string, ranked []scored)

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
	db, readOnly, err := openBoltDB(dbPath, cfg.ReadOnly, cfg.StrictWrite)
	if err != nil {
		return nil, err
	}

	if !readOnly {
		// Ensure top-level buckets exist.
		if err := db.Update(func(tx *bolt.Tx) error {
			for _, name := range [][]byte{bucketFacts, bucketSessions, bucketMeta, bucketAgents, bucketPendingVector, bucketKGExtracted} {
				if _, err := tx.CreateBucketIfNotExists(name); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init buckets: %w", err)
		}
	}

	// Use the caller-supplied vector backend, or default to chromem-go.
	vectors := cfg.VectorBackend
	if vectors == nil {
		v, err := newChromemVectorStore(cfg.DataDir)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open vector store: %w", err)
		}
		vectors = v
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		db:             db,
		vectors:        vectors,
		embedder:       cfg.Embedder,
		cfg:            cfg,
		readOnly:       readOnly,
		now:            time.Now,
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
		sema:           make(chan struct{}, cfg.MaxAsyncConsolidations),
	}

	// Hydrate known agent IDs so collections are ready.
	_ = s.loadAgents()

	if !readOnly {
		// Validate embedding dimensions against the stored value; warn on mismatch.
		if cfg.Embedder != nil {
			s.checkEmbedDimensions(cfg.Embedder)
		}

		// Drain any vector writes that did not complete on the previous run
		// (crash between bbolt commit and vector upsert, or transient failures).
		s.reconcileVectors()

		// Background reconcile loop: retries pending vectors on a cadence so the
		// inconsistency window collapses to at most VectorReconcileInterval rather
		// than "until next process restart". Disabled when the interval is 0.
		if cfg.VectorReconcileInterval > 0 {
			s.wg.Add(1)
			go s.vectorReconcileLoop(cfg.VectorReconcileInterval)
		}
	}

	return s, nil
}

// openBoltDB opens the bbolt database at path. If forceRO is true it opens
// directly in read-only mode. Otherwise it attempts a normal write open; on
// lock timeout it falls back to read-only and returns isReadOnly=true —
// unless strictWrite is set, in which case the lock timeout is surfaced
// immediately (daemon mode must never come up degraded). If both modes fail
// a descriptive error is returned.
func openBoltDB(path string, forceRO, strictWrite bool) (db *bolt.DB, isReadOnly bool, err error) {
	if forceRO && !strictWrite {
		db, err = bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: boltOpenTimeout})
		if err != nil {
			return nil, false, fmt.Errorf("open bbolt read-only: %w", err)
		}
		return db, true, nil
	}

	db, err = bolt.Open(path, 0o600, &bolt.Options{Timeout: boltOpenTimeout})
	if err == nil {
		return db, false, nil
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		return nil, false, fmt.Errorf("open bbolt: %w", err)
	}
	if strictWrite {
		return nil, false, fmt.Errorf(
			"gray.db is locked by another process (strict-write open refused to degrade): %w", err)
	}

	// Write lock held by another process; fall back to read-only.
	db, err = bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: boltOpenTimeout})
	if err != nil {
		return nil, false, fmt.Errorf(
			"gray.db is locked by another process and could not be opened read-only either: %w", err)
	}
	return db, true, nil
}

// IsReadOnly reports whether the store was opened in read-only mode.
func (s *Store) IsReadOnly() bool { return s.readOnly }

// Put stores a new observation for agentID.
//
// Durability model:
//  1. Compute embedding (best-effort; on failure the fact is keyword-only
//     and the failure is recorded in the store's embedding health, so a
//     broken backend is visible via EmbeddingHealth / doctor --embeddings
//     instead of being indistinguishable from an empty store).
//  2. Single bbolt transaction commits the fact AND, if an embedding exists,
//     a marker in bucketPendingVector. The marker is the durable "this still
//     needs to land in the vector store" intent.
//  3. Inline vector upsert. On success the marker is cleared. On failure the
//     marker remains and the background reconciler will retry it; the caller
//     still sees nil because bbolt is the source of truth.
//
// PutConfident stores a fact with an explicit epistemic confidence:
// "verified", "inferred" or "unverified" ("" defaults to inferred). The
// value is metadata for humans and exports; it never affects ranking,
// decay or pruning.
//
// Added in v0.12.0.
func (s *Store) PutConfident(ctx context.Context, agentID, text, confidence string) error {
	switch confidence {
	case "", "verified", "inferred", "unverified":
	default:
		return fmt.Errorf("confidence must be verified|inferred|unverified, got %q", confidence)
	}
	// The write path hands back the fact it just committed, so attaching the
	// marker is one direct update. The previous implementation re-listed the
	// whole agent and text-scanned for the new arrival - O(N) per confident
	// write and racy against concurrent writers.
	f, err := s.putReturningFact(ctx, agentID, text)
	if err != nil {
		return err
	}
	if f.Confidence != "" {
		return nil // a same-text write raced ahead and already stamped one
	}
	f.Confidence = confidence
	return s.UpdateFact(agentID, f)
}

// This closes the crash window between the bbolt write and the vector write:
// after a crash, reconcileVectors() at Open() drains the pending bucket.
func (s *Store) Put(ctx context.Context, agentID, text string) error {
	_, err := s.putReturningFact(ctx, agentID, text)
	return err
}

// putReturningFact is the single durable write path: it commits the fact and
// returns exactly what landed, so callers never have to find their own write
// again by scanning.
func (s *Store) putReturningFact(ctx context.Context, agentID, text string) (Fact, error) {
	if s.readOnly {
		return Fact{}, ErrStoreReadOnly
	}
	start := time.Now()

	var emb []float32
	var embedErr error
	if s.embedder != nil {
		emb, embedErr = s.embedder.Embed(ctx, text)
		if embedErr != nil {
			emb = nil
		}
	}

	f := newFact(agentID, text, emb, s.now())
	hasEmbedding := len(emb) > 0

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
		if err := tx.Bucket(bucketAgents).Put([]byte(agentID), []byte("1")); err != nil {
			return err
		}
		if hasEmbedding {
			pb, err := tx.Bucket(bucketPendingVector).CreateBucketIfNotExists([]byte(agentID))
			if err != nil {
				return err
			}
			if err := pb.Put([]byte(f.ID), []byte{1}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return Fact{}, fmt.Errorf("put fact: %w", err)
	}

	if hasEmbedding {
		s.recordEmbedDimensions(len(emb))
		if err := s.addToVector(ctx, agentID, f); err != nil {
			if s.cfg.OnVectorIndexError != nil {
				s.cfg.OnVectorIndexError(agentID, f.ID, err)
			}
		} else {
			s.clearPendingVector(agentID, f.ID)
		}
	} else if embedErr != nil {
		// The fact is durably keyword-only. Record the degradation so the
		// failure surfaces in EmbeddingHealth instead of vanishing; a
		// diagnostic counter must never fail the write it describes.
		recordEmbedDegraded(s.db, embedErr)
	}

	if s.cfg.OnPut != nil {
		s.cfg.OnPut(agentID, f.ID, time.Since(start))
	}
	return f, nil
}

// Delete removes a fact by ID for agentID, together with its KG extraction
// watermark: a signature for a deleted fact is unbounded garbage, and a
// future re-add of the same text lands under a new ID (new key) so it must
// extract again.
func (s *Store) Delete(agentID, factID string) error {
	if s.readOnly {
		return ErrStoreReadOnly
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketFacts)
		b := parent.Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		if err := b.Delete([]byte(factID)); err != nil {
			return err
		}
		if kb := tx.Bucket(bucketKGExtracted); kb != nil {
			return kb.Delete([]byte(agentID + "\x00" + factID))
		}
		return nil
	})
}

// List returns all facts for agentID, sorted newest first.
func (s *Store) List(agentID string) ([]Fact, error) {
	var facts []Fact
	if err := s.db.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketFacts)
		if parent == nil {
			// Read-only open of an empty or pre-bucketing database: there are
			// no facts by definition, and a crash here would turn a harmless
			// inspection into a process kill.
			return nil
		}
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
		b := tx.Bucket(bucketAgents)
		if b == nil {
			// A read-only open can land on an empty or pre-bucketing database
			// (no writer has run yet); "no agents" is the truth there, not a
			// reason to crash the process that merely looked.
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
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

// touchFacts persists access-metadata bumps for a recall batch in ONE
// transaction. Best-effort by contract: a lost bump is statistical noise on
// decay/recency curves measured in days, never a correctness event, so an
// error here degrades to a log-free no-op rather than failing the recall
// that already returned its results.
func (s *Store) touchFacts(facts []Fact) {
	if len(facts) == 0 {
		return
	}
	_ = s.db.Update(func(tx *bolt.Tx) error {
		for i := range facts {
			parent := tx.Bucket(bucketFacts)
			if parent == nil {
				return nil
			}
			b := parent.Bucket([]byte(facts[i].AgentID))
			if b == nil {
				continue
			}
			data, err := facts[i].marshal()
			if err != nil {
				continue
			}
			// Update, never create: if the fact vanished mid-recall (forget
			// racing the batch), a Put here would resurrect it - the exact
			// class of bug the UpdateFact guard closed.
			if b.Get([]byte(facts[i].ID)) == nil {
				continue
			}
			if err := b.Put([]byte(facts[i].ID), data); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpdateFact persists a modified fact (used by consolidation + decay).
func (s *Store) UpdateFact(agentID string, f Fact) error {
	if s.readOnly {
		return ErrStoreReadOnly
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts).Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		// Update, never create.
		//
		// Recall bumps the access counter of every fact it returns from a
		// detached goroutine, so a Delete can land between the caller reading
		// a fact and that write arriving. bolt's Put does not care whether the
		// key still exists, so the writeback used to bring the fact back —
		// after the store had already reported it deleted.
		//
		// That made forget unreliable on every path built on it: `graymatter
		// forget`, DELETE /forget, and memory_reflect's forget and update, all
		// of which recall or list before they delete. Nothing creates a fact
		// through here — Put is the creation path — so refusing to write a key
		// that is gone costs nothing and closes the window.
		if b.Get([]byte(f.ID)) == nil {
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
	_ = s.vectors.Close()
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
//
// The two inputs are each a fused ranking (whatever Recall produced for that
// namespace); their ranks are combined with the same k=60 RRF constant Recall
// uses internally, so neither namespace structurally outranks the other: a
// shared fact ranked 1 for the query beats an agent fact ranked 8. Before
// this, results were concatenated agent-first and truncated, which starved
// the shared namespace whenever the agent list filled its topK.
func (s *Store) RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	agentResults, err := s.Recall(ctx, agentID, query, topK)
	if err != nil {
		return nil, fmt.Errorf("recall agent: %w", err)
	}
	sharedResults, err := s.Recall(ctx, SharedAgentID, query, topK)
	if err != nil {
		return nil, fmt.Errorf("recall shared: %w", err)
	}
	return fuseRecallResults(agentResults, sharedResults, topK), nil
}

// fuseRecallResults merges two recall rankings with Reciprocal Rank Fusion
// (k=60, Cormack/Clarke/Buettcher SIGIR 2009 — the same constant and
// rationale as the per-agent fusion in recall.go). A text appearing in both
// lists accumulates both contributions, which also deduplicates it.
//
// The result order is total, per the api-stability determinism guarantee:
// fused score descending, then agent-list position ascending (a tie means
// the agent-scoped copy is the more specific of the two), then shared-list
// position, then text — so equal scores resolve identically on every call.
func fuseRecallResults(agentResults, sharedResults []string, topK int) []string {
	const k = 60.0

	agentRank := make(map[string]int, len(agentResults))
	for i, t := range agentResults {
		agentRank[t] = i + 1
	}
	sharedRank := make(map[string]int, len(sharedResults))
	for i, t := range sharedResults {
		sharedRank[t] = i + 1
	}

	fused := make(map[string]float64, len(agentResults)+len(sharedResults))
	add := func(ranks map[string]int) {
		for t, r := range ranks {
			fused[t] += 1 / (k + float64(r))
		}
	}
	add(agentRank)
	add(sharedRank)

	type entry struct {
		text string
		in   map[string]int // which list(s) placed it, text → 1-based rank
		sc   float64
	}
	entries := make([]entry, 0, len(fused))
	for t, sc := range fused {
		e := entry{text: t, sc: sc, in: make(map[string]int, 2)}
		if r, ok := agentRank[t]; ok {
			e.in["agent"] = r
		}
		if r, ok := sharedRank[t]; ok {
			e.in["shared"] = r
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.sc != b.sc {
			return a.sc > b.sc
		}
		ra, oka := a.in["agent"]
		rb, okb := b.in["agent"]
		switch {
		case oka && okb && ra != rb:
			return ra < rb
		case oka != okb:
			return oka // agent-listed text wins ties over shared-only
		}
		sa, oksa := a.in["shared"]
		sb, oksb := b.in["shared"]
		switch {
		case oksa && oksb && sa != sb:
			return sa < sb
		case oksa != oksb:
			return oksa
		}
		return a.text < b.text
	})

	if topK > len(entries) {
		topK = len(entries)
	}
	result := make([]string, 0, topK)
	for _, e := range entries[:topK] {
		result = append(result, e.text)
	}
	return result
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
		_ = s.vectors.EnsureCollection(id)
	}
	return nil
}

// reconcileVectors drains the pending-vector bucket: for every (agentID, factID)
// marker, it loads the fact from bbolt and re-attempts the vector upsert. On
// success the marker is cleared; on failure it stays and the next tick retries.
//
// O(pending) rather than O(total): callers that never crash see this as a no-op.
// AddDocument is idempotent so retries are safe even after partial successes.
func (s *Store) reconcileVectors() {
	pending := s.snapshotPendingVectors()
	if len(pending) == 0 {
		return
	}
	ctx := s.shutdownCtx
	for agentID, factIDs := range pending {
		for _, factID := range factIDs {
			if ctx.Err() != nil {
				return
			}
			f, ok := s.loadFact(agentID, factID)
			if !ok {
				// Fact was deleted between marker write and reconcile; drop the marker.
				s.clearPendingVector(agentID, factID)
				continue
			}
			if len(f.Embedding) == 0 {
				s.clearPendingVector(agentID, factID)
				continue
			}
			if err := s.addToVector(ctx, agentID, f); err != nil {
				if s.cfg.OnVectorIndexError != nil {
					s.cfg.OnVectorIndexError(agentID, factID, err)
				}
				continue
			}
			s.clearPendingVector(agentID, factID)
		}
	}
}

// vectorReconcileLoop runs reconcileVectors on a fixed cadence until shutdown.
func (s *Store) vectorReconcileLoop(interval time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			return
		case <-t.C:
			s.reconcileVectors()
		}
	}
}

// PendingVectorCount returns the number of facts currently waiting to be
// indexed in the vector store. A non-zero value after a quiescent period
// indicates a persistent embedder/vector-store failure worth investigating.
func (s *Store) PendingVectorCount() int {
	count := 0
	_ = s.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketPendingVector)
		if root == nil {
			return nil
		}
		return root.ForEach(func(k, v []byte) error {
			if v != nil {
				return nil // not a sub-bucket
			}
			sub := root.Bucket(k)
			if sub == nil {
				return nil
			}
			stats := sub.Stats()
			count += stats.KeyN
			return nil
		})
	})
	return count
}

func (s *Store) clearPendingVector(agentID, factID string) {
	_ = s.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketPendingVector)
		if root == nil {
			return nil
		}
		sub := root.Bucket([]byte(agentID))
		if sub == nil {
			return nil
		}
		return sub.Delete([]byte(factID))
	})
}

func (s *Store) snapshotPendingVectors() map[string][]string {
	out := make(map[string][]string)
	_ = s.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketPendingVector)
		if root == nil {
			return nil
		}
		return root.ForEach(func(k, v []byte) error {
			if v != nil {
				return nil
			}
			sub := root.Bucket(k)
			if sub == nil {
				return nil
			}
			agentID := string(k)
			_ = sub.ForEach(func(fk, _ []byte) error {
				out[agentID] = append(out[agentID], string(fk))
				return nil
			})
			return nil
		})
	})
	return out
}

func (s *Store) loadFact(agentID, factID string) (Fact, bool) {
	var f Fact
	var ok bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts).Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(factID))
		if raw == nil {
			return nil
		}
		parsed, err := unmarshalFact(raw)
		if err != nil {
			return nil
		}
		f = parsed
		ok = true
		return nil
	})
	return f, ok
}

// checkEmbedDimensions reads the stored embedding dimension from the meta bucket
// and warns if the current provider's dimension differs. On first use it records
// the current dimension so future opens can detect provider switches.
func (s *Store) checkEmbedDimensions(emb embedding.Provider) {
	const metaKeyDims = "embed_dims"
	currentDims := emb.Dimensions()
	if currentDims <= 0 {
		return // provider doesn't know its dims yet (e.g. Ollama before first call)
	}

	_ = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		stored := b.Get([]byte(metaKeyDims))
		if stored == nil {
			val, _ := json.Marshal(currentDims)
			return b.Put([]byte(metaKeyDims), val)
		}
		var storedDims int
		if err := json.Unmarshal(stored, &storedDims); err != nil {
			return nil
		}
		if storedDims != currentDims {
			log.Printf("graymatter: WARNING embedding dimension mismatch: stored=%d current=%d (provider=%s). "+
				"Vector search results may be inaccurate. Consider re-indexing your data.",
				storedDims, currentDims, emb.Name())
		}
		return nil
	})
}

// recordEmbedDimensions writes the embedding dimension to meta if not already set.
// Called the first time a fact with an embedding is persisted.
func (s *Store) recordEmbedDimensions(dims int) {
	const metaKeyDims = "embed_dims"
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b.Get([]byte(metaKeyDims)) != nil {
			return nil // already recorded
		}
		val, _ := json.Marshal(dims)
		return b.Put([]byte(metaKeyDims), val)
	})
}

func (s *Store) addToVector(ctx context.Context, agentID string, f Fact) error {
	metadata := map[string]string{
		"agent_id":   agentID,
		"created_at": f.CreatedAt.Format(time.RFC3339),
	}
	return s.vectors.AddDocument(ctx, agentID, f.ID, f.Text, f.Embedding, metadata)
}

// vectorSearch returns at most n results from the vector index.
func (s *Store) vectorSearch(ctx context.Context, agentID, query string, n int) ([]VectorResult, error) {
	if s.embedder == nil {
		return nil, nil
	}
	qEmb, err := s.embedder.Embed(ctx, query)
	if err != nil || len(qEmb) == 0 {
		return nil, nil
	}
	return s.vectors.Query(ctx, agentID, qEmb, n)
}

// marshalJSON helper for meta bucket.
func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func sortFactsByTime(facts []Fact) {
	sort.SliceStable(facts, func(i, j int) bool {
		return facts[i].CreatedAt.After(facts[j].CreatedAt)
	})
}
