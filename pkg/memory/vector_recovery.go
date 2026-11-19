package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
)

// unavailableVectorStore preserves canonical reads while a native index cannot
// be opened. Failed deliveries keep their bbolt intents for the next open.
type unavailableVectorStore struct {
	cause error
}

func (v unavailableVectorStore) AddDocument(context.Context, string, string, string, []float32, map[string]string) error {
	return v.cause
}

func (v unavailableVectorStore) Query(context.Context, string, []float32, int) ([]VectorResult, error) {
	return nil, v.cause
}

func (v unavailableVectorStore) EnsureCollection(string) error { return v.cause }
func (unavailableVectorStore) Close() error                    { return nil }

func openNativeVectors(dataDir string, db *bolt.DB, readOnly bool) (VectorStore, error) {
	open := newChromemVectorStore
	if readOnly {
		open = newReadOnlyChromemVectorStore
	}
	vectors, err := open(dataDir)
	if err == nil {
		if err := repairMissingRevisionVectors(db, vectors, readOnly); err != nil {
			return unavailableVectorStore{cause: err}, nil
		}
		return vectors, nil
	}

	var recoverable bool
	if readErr := db.View(func(tx *bolt.Tx) error {
		recoverable = nativeVectorRecoveryNeeded(tx)
		return nil
	}); readErr != nil {
		return nil, errors.Join(err, readErr)
	}
	if !recoverable {
		return nil, err
	}
	if !readOnly {
		repairErr := quarantineNativeVectors(dataDir, db)
		if repairErr == nil {
			reopened, reopenErr := open(dataDir)
			if reopenErr == nil {
				return reopened, nil
			}
			repairErr = fmt.Errorf("open rebuilt vector index: %w", reopenErr)
		}
		err = errors.Join(err, repairErr)
	}
	return unavailableVectorStore{cause: fmt.Errorf("native vector index unavailable: %w", err)}, nil
}

// A revision sequence survives a successful delivery. It authorizes repair
// even when the pending marker reached disk but the derived file did not.
func nativeVectorRecoveryNeeded(tx *bolt.Tx) bool {
	root := tx.Bucket(bucketPendingVector)
	if root == nil {
		return false
	}
	c := root.Cursor()
	for agent, _ := c.First(); agent != nil; agent, _ = c.Next() {
		queue := root.Bucket(agent)
		if queue != nil {
			first, _ := queue.Cursor().First()
			if queue.Sequence() != 0 || first != nil {
				return true
			}
		}
	}
	return false
}

type nativeVectorGap struct {
	agent, id string
}

// A healthy derived database can still be missing a delivered vector after
// a host crash. Inventory the actual document IDs in revised namespaces.
// The store is not yet serving and these lookups are strictly in memory.
func repairMissingRevisionVectors(db *bolt.DB, vectors *chromemVectorStore, readOnly bool) error {
	var missing []nativeVectorGap
	if err := db.View(func(tx *bolt.Tx) error {
		facts, pending := tx.Bucket(bucketFacts), tx.Bucket(bucketPendingVector)
		if facts == nil || pending == nil {
			return nil
		}
		return pending.ForEach(func(agentID, _ []byte) error {
			queue := pending.Bucket(agentID)
			agent := facts.Bucket(agentID)
			if queue == nil || queue.Sequence() == 0 || agent == nil {
				return nil
			}
			namespace := string(agentID)
			collection := vectors.db.GetCollection(namespace, nil)
			return forEachCanonicalEmbedding(agent, agentID, func(id []byte) error {
				factID := string(id)
				if collection != nil {
					if _, err := collection.GetByID(context.Background(), factID); err == nil {
						return nil
					}
				}
				missing = append(missing, nativeVectorGap{namespace, factID})
				return nil
			})
		})
	}); err != nil {
		return fmt.Errorf("inventory native revision vectors: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	if readOnly {
		return fmt.Errorf("native vector index is missing %d live revision-namespace documents", len(missing))
	}
	return db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketPendingVector)
		if root == nil {
			return errors.New("native vector recovery queue is missing")
		}
		for _, gap := range missing {
			queue := root.Bucket([]byte(gap.agent))
			if queue == nil {
				return errors.New("native vector recovery namespace is missing")
			}
			if err := queue.Put([]byte(gap.id), []byte{1}); err != nil {
				return err
			}
		}
		return nil
	})
}

// quarantineNativeVectors runs only after a writable canonical open owns the
// file lock. All live vector deliveries are durable before the index directory
// moves. A restart before the move can repeat the queueing, and a restart
// after the move can recreate the index and replay the same queue.
func quarantineNativeVectors(dataDir string, db *bolt.DB) error {
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	vectors := filepath.Join(root, "vectors")
	info, err := os.Lstat(vectors)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("vector recovery requires a real index directory")
	}
	// Reject aliases to another directory, including a symlink in a parent
	// path. Renaming the directory does not follow any links within it.
	resolved, err := filepath.EvalSymlinks(vectors)
	if err != nil {
		return err
	}
	if relative, err := filepath.Rel(vectors, resolved); err != nil || relative != "." {
		return errors.New("vector recovery cannot move a redirected index directory")
	}
	if err := db.Update(queueNativeVectorRecovery); err != nil {
		return fmt.Errorf("queue native vector recovery: %w", err)
	}

	quarantine, err := os.MkdirTemp(root, "vectors.quarantine-")
	if err != nil {
		return err
	}
	moved := false
	defer func() {
		if !moved {
			_ = os.Remove(quarantine)
		}
	}()
	current, err := os.Lstat(vectors)
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		return errors.New("vector index directory changed during recovery")
	}
	destination := filepath.Join(quarantine, "vectors")
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return errors.New("vector quarantine destination is not empty")
	}
	if err := os.Rename(vectors, destination); err != nil {
		return fmt.Errorf("quarantine native vector index: %w", err)
	}
	moved = true
	return nil
}

func queueNativeVectorRecovery(tx *bolt.Tx) error {
	facts, pending := tx.Bucket(bucketFacts), tx.Bucket(bucketPendingVector)
	if facts == nil || pending == nil {
		return errors.New("canonical vector recovery buckets are missing")
	}
	return facts.ForEach(func(agentID, _ []byte) error {
		agent := facts.Bucket(agentID)
		if agent == nil {
			return nil
		}
		var queue *bolt.Bucket
		return forEachCanonicalEmbedding(agent, agentID, func(id []byte) error {
			if queue == nil {
				var err error
				queue, err = pending.CreateBucketIfNotExists(agentID)
				if err != nil {
					return err
				}
			}
			return queue.Put(id, []byte{1})
		})
	})
}

func forEachCanonicalEmbedding(agent *bolt.Bucket, agentID []byte, visit func([]byte) error) error {
	return agent.ForEach(func(id, raw []byte) error {
		fact, err := unmarshalFact(raw)
		if err != nil {
			return fmt.Errorf("decode fact %q: %w", id, err)
		}
		if fact.ID != string(id) || fact.AgentID != string(agentID) {
			return fmt.Errorf("canonical fact identity mismatch for %q", id)
		}
		if fact.IsSuperseded() || fact.IsAlias() || len(fact.Embedding) == 0 {
			return nil
		}
		return visit(id)
	})
}
