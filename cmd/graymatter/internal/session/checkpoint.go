// Package session provides agent checkpoint and recovery for GrayMatter.
// Checkpoints are serialised to bbolt under the "sessions" bucket, keyed by
// agentID/checkpointID. They store arbitrary agent state alongside the last
// N messages from a conversation, enabling full session recovery after a crash.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
)

var bucketSessions = []byte("sessions")

// ErrNoCheckpoint means the agent has no saved checkpoints. Storage and
// decoding failures do not match this sentinel.
var ErrNoCheckpoint = errors.New("no checkpoints")

// Message is a single turn in an agent conversation, stored in a checkpoint.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Checkpoint captures a full snapshot of agent state at a point in time.
type Checkpoint struct {
	ID        string            `json:"id"`
	AgentID   string            `json:"agent_id"`
	CreatedAt time.Time         `json:"created_at"`
	State     map[string]any    `json:"state,omitempty"`
	Messages  []Message         `json:"messages,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Save persists a checkpoint to bbolt. If cp.ID is empty a new ULID is assigned.
func Save(db *bolt.DB, cp Checkpoint) (Checkpoint, error) {
	if cp.ID == "" {
		cp.ID = ulid.Make().String()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	data, err := json.Marshal(cp)
	if err != nil {
		return cp, fmt.Errorf("marshal checkpoint: %w", err)
	}

	return cp, db.Update(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketSessions)
		b, err := parent.CreateBucketIfNotExists([]byte(cp.AgentID))
		if err != nil {
			return err
		}
		return b.Put([]byte(cp.ID), data)
	})
}

// Load retrieves a specific checkpoint by ID.
func Load(db *bolt.DB, agentID, checkpointID string) (*Checkpoint, error) {
	var cp Checkpoint
	err := db.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketSessions)
		b := parent.Bucket([]byte(agentID))
		if b == nil {
			return fmt.Errorf("no checkpoints for agent %q", agentID)
		}
		data := b.Get([]byte(checkpointID))
		if data == nil {
			return fmt.Errorf("checkpoint %q not found", checkpointID)
		}
		return json.Unmarshal(data, &cp)
	})
	if err != nil {
		return nil, err
	}
	return &cp, nil
}

// List returns all checkpoints for agentID sorted newest first. An unreadable
// checkpoint is an error: silently skipping it could resume an older state.
func List(db *bolt.DB, agentID string) ([]Checkpoint, error) {
	var checkpoints []Checkpoint
	err := db.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketSessions)
		b := parent.Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var cp Checkpoint
			if err := json.Unmarshal(v, &cp); err != nil {
				return fmt.Errorf("decode checkpoint %q for agent %q: %w", k, agentID, err)
			}
			checkpoints = append(checkpoints, cp)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})
	return checkpoints, nil
}

// Latest returns the most recently created checkpoint for agentID.
func Latest(db *bolt.DB, agentID string) (*Checkpoint, error) {
	cps, err := List(db, agentID)
	if err != nil {
		return nil, err
	}
	if len(cps) == 0 {
		return nil, fmt.Errorf("%w for agent %q", ErrNoCheckpoint, agentID)
	}
	return &cps[0], nil
}

// Delete removes a checkpoint permanently.
func Delete(db *bolt.DB, agentID, checkpointID string) error {
	return db.Update(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketSessions)
		b := parent.Bucket([]byte(agentID))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(checkpointID))
	})
}
