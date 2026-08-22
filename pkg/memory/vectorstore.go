package memory

import (
	"context"
	"path/filepath"
	"sync"

	chromem "github.com/philippgille/chromem-go"
)

// VectorResult is a single result from a vector similarity query.
type VectorResult struct {
	ID         string
	Content    string
	Similarity float32
}

// VectorStore is the pluggable vector search backend.
// The default implementation wraps chromem-go (zero-infra, pure-Go, persistent).
// Future implementations can wrap Qdrant, Weaviate, pgvector, etc.
//
// Implementations must be safe for concurrent use.
type VectorStore interface {
	// AddDocument upserts a document with its pre-computed embedding.
	AddDocument(ctx context.Context, collection, id, content string, embedding []float32, metadata map[string]string) error

	// Query returns at most n documents ranked by cosine similarity to embedding.
	Query(ctx context.Context, collection string, embedding []float32, n int) ([]VectorResult, error)

	// EnsureCollection creates the collection if it does not exist.
	EnsureCollection(collection string) error

	// Close flushes and releases any resources held by the store.
	Close() error
}

// chromemVectorStore wraps chromem-go to satisfy VectorStore.
//
// collections is a plain map behind mu. The interface contract above promises
// concurrent safety and the store delivers on it: Put calls addToVector
// outside the store mutex, the daemon serves every RPC in its own goroutine,
// and the background reconcile loop touches the same map. Without mu, two
// agents writing at once hit "concurrent map read and map write", which is a
// fatal error no recover() can catch — the daemon dies and takes every
// client's memory with it.
type chromemVectorStore struct {
	db          *chromem.DB
	mu          sync.Mutex
	collections map[string]*chromem.Collection
}

// newChromemVectorStore opens or creates a persistent chromem-go DB at dataDir/vectors.
func newChromemVectorStore(dataDir string) (*chromemVectorStore, error) {
	vecDir := filepath.Join(dataDir, "vectors")
	db, err := chromem.NewPersistentDB(vecDir, false)
	if err != nil {
		return nil, err
	}
	return &chromemVectorStore{
		db:          db,
		collections: make(map[string]*chromem.Collection),
	}, nil
}

func (c *chromemVectorStore) EnsureCollection(name string) error {
	_, err := c.collection(name)
	return err
}

// collection returns the named collection, creating it on first use. The lock
// is held across GetOrCreateCollection so two callers racing on a new
// collection agree on which handle wins.
func (c *chromemVectorStore) collection(name string) (*chromem.Collection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if col, ok := c.collections[name]; ok {
		return col, nil
	}
	col, err := c.db.GetOrCreateCollection(name, nil, nil)
	if err != nil {
		return nil, err
	}
	c.collections[name] = col
	return col, nil
}

func (c *chromemVectorStore) AddDocument(ctx context.Context, collection, id, content string, embedding []float32, metadata map[string]string) error {
	col, err := c.collection(collection)
	if err != nil {
		return err
	}
	return col.AddDocument(ctx, chromem.Document{
		ID:        id,
		Content:   content,
		Metadata:  metadata,
		Embedding: embedding,
	})
}

func (c *chromemVectorStore) Query(ctx context.Context, collection string, embedding []float32, n int) ([]VectorResult, error) {
	col, err := c.collection(collection)
	if err != nil {
		return nil, err
	}
	raw, err := col.QueryEmbedding(ctx, embedding, n, nil, nil)
	if err != nil {
		return nil, err
	}
	results := make([]VectorResult, len(raw))
	for i, r := range raw {
		results[i] = VectorResult{
			ID:         r.ID,
			Content:    r.Content,
			Similarity: r.Similarity,
		}
	}
	return results, nil
}

func (c *chromemVectorStore) Close() error {
	// chromem-go does not require an explicit close; data is flushed on write.
	return nil
}
