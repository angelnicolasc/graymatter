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
	// bucketReformPending holds one pendingMiss per agent, for usage-alias
	// learning: the content
	// terms of the latest query whose match came back weak, waiting for the
	// strong query that answers it. Key agentID → JSON.
	bucketReformPending = []byte("reform_pending")
	// bucketReformPairs accumulates reformulation-pair evidence: key
	// "<agentID>\x00<termA>\x00<termB>" (terms sorted) → JSON count+lastAt.
	// A pair promoted at the pre-registered threshold becomes a usage alias.
	bucketReformPairs = []byte("reform_pairs")

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

	// StemKeywords folds English morphology into the keyword signal, so a
	// question about "backups" reaches a fact about "backup retention" and one
	// about a "pager rotation" reaches "rotations were stretched". Three of the
	// eight probes the keyword ranker missed on the revision harness failed on
	// exactly that and nothing else.
	//
	// The zero value is off, and the PRODUCT default is on: graymatter.Config
	// sets it (GRAYMATTER_STEM_KEYWORDS=0 opts out). The two disagree on
	// purpose — a zero-value StoreConfig means "nothing configured", and every
	// suite in this package pins ranking against that unconfigured baseline.
	//
	// It ships on because the delta was measured on a corpus it was not
	// designed against: 25/35 -> 29/35 at 5k, 10k and 30k facts, winning four
	// queries and losing none. The strict-subset property is the revert
	// criterion (benchmarks/retrieval_quality/stem_ab_test.go).
	//
	// The cost is the scan path, measured at +61-90% across two machines. On
	// the candidate-set path it is free on both: the index stems the corpus at
	// write time and only the query at read time. Pure Go — no model, no download, no cgo
	// (pkg/memory/stem.go).
	StemKeywords bool

	// UsageAliasLearning lets the store promote its own vocabulary
	// from observed reformulation pairs (weak match followed by a strong one
	// from the same agent), with no agent action and no server-side semantics
	// — the LLM's reformulation is the semantic decision, the store only
	// counts evidence and promotes on the second independent observation.
	//
	// Off by default, and it stayed off where StemKeywords did not: measured
	// against real agent reformulations rather than a scripted one, opening
	// the affinity gate is worth +2 families out of 40 and promotes about one
	// junk pair in three. The mechanism earned its place in the tree; it did
	// not earn the default. See pkg/memory/usagealias.go
	// for the guardrails (k=2, one pending miss per agent, TTL, df filters,
	// AliasSource="usage" so autonomy cannot masquerade as curation).
	UsageAliasLearning bool

	// UsageAliasAffinityMin sets how much lexical affinity an unknown word
	// needs with the working word before a usage alias promotes: the minimum
	// common leading prefix, in characters.
	//
	// 0 (the zero value) normalizes to 3 at Open: the conservative
	// morphology-only gate, where co-occurrence evidence cannot distinguish a
	// real bridge from sentence scaffolding, so without affinity the store
	// would promote "who = payments" from a repeated question shape.
	// -1 disables the gate: the store then learns the synonym class from
	// single-gap evidence — the class real agents actually reformulate by,
	// at a pollution risk measured by classifying every promoted pair over a
	// blind evaluation corpus. Explicit positive values set a custom prefix
	// threshold.
	UsageAliasAffinityMin int

	// CandidateRetrieval routes recall through the candidate-set index
	// (pkg/memory/index.go) instead of loading and re-tokenising every fact
	// per query. The ranking is identical by construction and by test — the
	// index changes what gets read, never what gets scored — and the store
	// falls back to the scan whenever the index cannot answer.
	//
	// The zero value is off, and the PRODUCT default is on: both of its gates
	// went green on two machines (the numbers live on graymatter.Config). The
	// two disagree on purpose, as they do on StemKeywords — a zero-value
	// StoreConfig means "nothing configured", and every suite in this package
	// pins ranking against that unconfigured baseline.
	CandidateRetrieval bool

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
	// idxVerified remembers which agents this process has already checked the
	// expensive way (a full key count against the facts bucket). The check
	// exists to catch a write path that bypassed index maintenance — a
	// code-level mistake — so paying for it once per process is enough, and
	// paying for it per recall would reintroduce the O(N) the index removes.
	idxVerified   map[string]bool
	idxVerifiedMu sync.Mutex

	// spine caches each agent's ordered recency spine against the index write
	// counter. See recall_indexed.go.
	spine      map[string]spineSnapshot
	partitions map[string]partitionSnapshot
	spineMu    sync.RWMutex

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
	// UsageAliasAffinityMin normalizes 0 (the zero value) to 3 — the measured
	// conservative gate — so the zero-value StoreConfig keeps the
	// morphology-only behaviour every evaluation run used. -1 disables the gate
	// entirely: the synonym-class measured mode, at a documented pollution
	// cost.
	if cfg.UsageAliasAffinityMin == 0 {
		cfg.UsageAliasAffinityMin = 3
	}
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
		// Detect an embedding-provider switch and self-heal: every live fact
		// is queued for re-indexing under the new provider before the store
		// starts serving, and the synchronous drain below finishes the pass
		// so no mixed-dimension window ever reaches a query.
		if cfg.Embedder != nil {
			s.handleEmbedderLifecycle(cfg.Embedder)
		}

		// Drain any vector writes that did not complete on the previous run
		// (crash between bbolt commit and vector upsert, or transient failures)
		// - and, after a provider switch, the full re-index queued above.
		s.reconcileVectors()

		// Expire reformulation pairs that never reached the promotion
		// threshold within their evidence window: stale evidence must
		// not promote vocabulary months later.
		s.pruneReformPairs()

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
	_, err := s.PutReturningFact(ctx, agentID, text)
	return err
}

// PutReturningFact writes a fact and returns the exact value committed. It is
// the identity-preserving counterpart to Put for callers that must persist a
// reference to their own write without rediscovering it through List.
func (s *Store) PutReturningFact(ctx context.Context, agentID, text string) (Fact, error) {
	return s.putReturningFact(ctx, agentID, text)
}

// putReturningFact is the single durable write path: it commits the fact and
// returns exactly what landed, so callers never have to find their own write
// again by scanning.
func (s *Store) putReturningFact(ctx context.Context, agentID, text string) (Fact, error) {
	return s.putReturningFactKind(ctx, agentID, text, KindFact, "")
}

// putReturningFactKind is putReturningFact with the fact's kind and alias
// source. Alias facts skip the embedder on purpose: they are lexical glue,
// never injectable, and a vector entry for one could only ever contribute a
// rank nobody reads.
func (s *Store) putReturningFactKind(ctx context.Context, agentID, text, kind, source string) (Fact, error) {
	if s.readOnly {
		return Fact{}, ErrStoreReadOnly
	}
	start := time.Now()

	var emb []float32
	var embedErr error
	if kind != KindAlias && s.embedder != nil {
		emb, embedErr = s.embedder.Embed(ctx, text)
		if embedErr != nil {
			emb = nil
		}
	}

	f := newFact(agentID, text, emb, s.now())
	f.Kind = kind
	f.AliasSource = source
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
		// Candidate-set index. Inside the fact's own transaction, so the
		// index can never be durable while the fact is not, or the reverse.
		//
		// Only when the store actually reads through it. Maintaining it
		// unconditionally would have been simpler and was the first shape,
		// but it made every write on every store pay for a path that is off
		// by default — measured at roughly double the Put latency — and a
		// store nobody opted in for should cost exactly what it cost before
		// this file existed. Turning the flag on later is safe without it:
		// the count will not match, and the first recall rebuilds.
		if s.cfg.CandidateRetrieval {
			if err := idxAddFact(tx, f, s.cfg.StemKeywords); err != nil {
				return err
			}
			if err := idxBumpCount(tx, agentID, +1, s.cfg.StemKeywords); err != nil {
				return err
			}
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
		// Read before deleting: the index entries to erase are derived from
		// the stored text, so once the fact is gone there is nothing left to
		// derive them from.
		if s.cfg.CandidateRetrieval {
			if raw := b.Get([]byte(factID)); raw != nil {
				if old, err := unmarshalFactLite(raw); err == nil {
					if err := idxRemoveFact(tx, old, s.cfg.StemKeywords); err != nil {
						return err
					}
				}
			}
		}
		if err := b.Delete([]byte(factID)); err != nil {
			return err
		}
		if s.cfg.CandidateRetrieval {
			if err := idxBumpCount(tx, agentID, -1, s.cfg.StemKeywords); err != nil {
				return err
			}
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

// listLite is List for the recall pipeline: same facts, same order, decoded
// with the lite decoder that skips the fields ranking never reads (see
// factLite). Every caller is inside this package, so the lite/full agreement
// test in recall_lite_test.go is what keeps the two decoders honest.
func (s *Store) listLite(agentID string) ([]Fact, error) {
	var facts []Fact
	if err := s.db.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketFacts)
		if parent == nil {
			return nil
		}
		b := parent.Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			f, err := unmarshalFactLite(v)
			if err != nil {
				return nil // skip corrupt entries, same as List
			}
			facts = append(facts, f)
			return nil
		})
	}); err != nil {
		return nil, err
	}
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
			key := []byte(facts[i].ID)
			// Update, never create: if the fact vanished mid-recall (forget
			// racing the batch), a Put here would resurrect it - the exact
			// class of bug the UpdateFact guard closed.
			raw := b.Get(key)
			if raw == nil {
				continue
			}
			// Read-modify-write the CURRENT stored fact, not the recall-time
			// snapshot: a concurrent consolidation may have changed weight,
			// tombstone state or pinned flag between List and this write, and
			// the snapshot must not stomp it. The old shape marshalled the
			// snapshot back whole — correct only while writes were serialised
			// by luck, and it made the lite decode path impossible besides.
			current, uerr := unmarshalFact(raw)
			if uerr != nil {
				continue
			}
			// Update, never resurrect: the same race with the tombstone half
			// — a consolidation cycle can supersede the fact between Recall's
			// filter and this writeback, and the stale snapshot must not
			// clear the tombstone. See UpdateFact for the full story.
			if current.IsSuperseded() {
				continue
			}
			current.AccessCount++
			current.AccessedAt = facts[i].AccessedAt
			data, err := current.marshal()
			if err != nil {
				continue
			}
			if err := b.Put(key, data); err != nil {
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
		//
		if b.Get([]byte(f.ID)) == nil {
			return nil
		}
		// Update, never resurrect (the tombstone half of the same race,
		// found by the agent-lifecycle simulation): decay and access-tracking
		// write back snapshots taken BEFORE the write, so a tombstone that
		// lands while a consolidation cycle is in flight used to be silently
		// overwritten by the cycle's decay pass — the superseded fact came
		// back from the dead with SupersededBy="". When the stored fact is
		// tombstoned and the incoming snapshot is not, the tombstone wins and
		// the stale write is dropped: every legitimate caller here either
		// carries the tombstone forward or operates on a live fact, and no
		// path is allowed to un-retire anything.
		if raw := b.Get([]byte(f.ID)); raw != nil {
			current, uerr := unmarshalFact(raw)
			if uerr == nil && current.IsSuperseded() && !f.IsSuperseded() {
				return nil
			}
		}
		data, err := f.marshal()
		if err != nil {
			return err
		}
		// Candidate-set index. This is the path a revision takes — the
		// tombstone is an UpdateFact — and it is also the path a CreatedAt
		// rewrite takes, so both the term postings and the fact's place in the
		// recency spine can move here. Erase the entries derived from what is
		// stored, then write the ones the new version implies; doing it in
		// that order is what keeps a rewritten text from stranding postings
		// under its old vocabulary.
		if !s.cfg.CandidateRetrieval {
			return b.Put([]byte(f.ID), data)
		}
		if raw := b.Get([]byte(f.ID)); raw != nil {
			if prev, uerr := unmarshalFactLite(raw); uerr == nil {
				if err := idxRemoveFact(tx, prev, s.cfg.StemKeywords); err != nil {
					return err
				}
			}
		}
		if err := b.Put([]byte(f.ID), data); err != nil {
			return err
		}
		if err := idxAddFact(tx, f, s.cfg.StemKeywords); err != nil {
			return err
		}
		return idxBumpCount(tx, agentID, 0, s.cfg.StemKeywords)
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
	// The active dimension decides whether a fact's stored embedding is
	// still valid: a length mismatch means the provider switched after this
	// vector was written, and reusing it would poison similarity search.
	targetDims, _ := s.storedEmbedDims()
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
			if f.IsSuperseded() {
				// Retired facts never reach the index; drop the stale intent.
				s.clearPendingVector(agentID, factID)
				continue
			}
			if len(f.Embedding) == 0 && (targetDims <= 0 || s.embedder == nil) {
				s.clearPendingVector(agentID, factID)
				continue
			}
			if err := s.reindexFact(ctx, agentID, &f, targetDims); err != nil {
				if s.cfg.OnVectorIndexError != nil {
					s.cfg.OnVectorIndexError(agentID, factID, err)
				}
				continue // marker stays: retried on the next cadence tick
			}
			s.clearPendingVector(agentID, factID)
		}
	}
}

// reindexFact lands one fact in the vector store under the active dimension.
// When the stored embedding's length disagrees with targetDims - or is
// absent while a provider exists - the fact is re-embedded fresh and the new
// vector is persisted, so later retries never repeat the embedding work.
func (s *Store) reindexFact(ctx context.Context, agentID string, f *Fact, targetDims int) error {
	if targetDims > 0 && len(f.Embedding) != targetDims && s.embedder != nil {
		fresh, err := s.embedder.Embed(ctx, f.Text)
		if err != nil {
			return fmt.Errorf("re-embed for switched provider: %w", err)
		}
		if len(fresh) == 0 {
			return fmt.Errorf("re-embed returned no vector")
		}
		f.Embedding = fresh
		if err := s.UpdateFact(agentID, *f); err != nil {
			return fmt.Errorf("persist re-embedded fact: %w", err)
		}
	}
	return s.addToVector(ctx, agentID, *f)
}

// storedEmbedDims returns the dimension recorded for this store and whether
// one has been recorded at all.
func (s *Store) storedEmbedDims() (int, bool) {
	const metaKeyDims = "embed_dims"
	var dims int
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(metaKeyDims)); v != nil {
			return json.Unmarshal(v, &dims)
		}
		return nil
	})
	if err != nil || dims <= 0 {
		return 0, false
	}
	return dims, true
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

// handleEmbedderLifecycle reconciles the active provider with the store's
// recorded embedding state. First provider ever seen: record its dimensions.
// Same dimensions as before: nothing to do. Different dimensions - the
// signature of a provider switch: queue every live fact for re-indexing and
// adopt the new dimension durably. The caller's synchronous reconcileVectors
// drain then re-embeds the queue before the store serves a single query, so
// stale vectors from the old provider can never poison search results.
func (s *Store) handleEmbedderLifecycle(emb embedding.Provider) {
	const metaKeyDims = "embed_dims"
	current := emb.Dimensions()
	if current <= 0 {
		return // provider doesn't know its dims yet (e.g. Ollama before first call)
	}

	var stored int
	haveStored := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(metaKeyDims)); v != nil {
			if err := json.Unmarshal(v, &stored); err == nil {
				haveStored = true
			}
		}
		return nil
	})
	if !haveStored {
		s.recordEmbedDimensions(current)
		return
	}
	if stored == current {
		return
	}

	log.Printf("graymatter: embedding provider switch detected (stored dims %d -> %d via %s). "+
		"Re-indexing all live facts under the new provider before serving.",
		stored, current, emb.Name())

	queued := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketMeta)
		if mb == nil {
			return fmt.Errorf("meta bucket missing")
		}
		val, _ := json.Marshal(current)
		// Adopt the new dimension durably first: a crash mid-reindex must not
		// lose the intent, and the reconciler compares against this value.
		if err := mb.Put([]byte(metaKeyDims), val); err != nil {
			return err
		}
		parent := tx.Bucket(bucketFacts)
		pendingRoot := tx.Bucket(bucketPendingVector)
		if parent == nil || pendingRoot == nil {
			return nil // nothing stored yet
		}
		return parent.ForEach(func(agentKey, _ []byte) error {
			agentBucket := parent.Bucket(agentKey)
			if agentBucket == nil {
				return nil
			}
			pending, err := pendingRoot.CreateBucketIfNotExists(agentKey)
			if err != nil {
				return err
			}
			return agentBucket.ForEach(func(factKey, factVal []byte) error {
				if factVal == nil {
					return nil
				}
				f, err := unmarshalFact(factVal)
				if err != nil || f.IsSuperseded() {
					return nil // retired facts never reach the index
				}
				queued++
				return pending.Put(factKey, []byte{1})
			})
		})
	})
	if err != nil {
		log.Printf("graymatter: reindex queueing failed (dims adopted; retried on next open): %v", err)
		return
	}
	log.Printf("graymatter: %d fact(s) queued for re-index under %s", queued, emb.Name())
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
