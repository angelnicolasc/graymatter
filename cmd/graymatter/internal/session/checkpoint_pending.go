package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// PendingCheckpoint is a read-only pointer, never a copy of durable facts or
// task state. CreatedAt is the checkpoint's saved timestamp, not an access.
type PendingCheckpoint struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// PeekPending inspects the checkpoint Resume would select without resuming it.
// An empty or explicitly completed latest state does not expose older work.
// Hook markers do not hide useful checkpoints. Absence is (nil, nil); corrupt
// or unavailable storage remains an error. The only transaction is db.View.
func PeekPending(db *bolt.DB, agentID string) (*PendingCheckpoint, error) {
	if db == nil {
		return nil, errors.New("checkpoint store not initialised")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("checkpoint agent_id must be non-empty")
	}
	var checkpoints []Checkpoint
	err := db.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketSessions)
		if parent == nil {
			return errors.New("checkpoint store not initialised")
		}
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
	// Keep precisely List's ordering, including ties, so a pointer never names
	// another checkpoint than the subsequent legacy Resume operation.
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt) })
	for _, cp := range checkpoints {
		if isHookMarker(cp) {
			continue
		}
		if !IsPending(cp) {
			return nil, nil
		}
		return &PendingCheckpoint{ID: cp.ID, CreatedAt: cp.CreatedAt}, nil
	}
	return nil, nil
}

// IsPending reports whether the selected checkpoint contains unfinished work.
func IsPending(cp Checkpoint) bool {
	if len(cp.State) == 0 && len(cp.Messages) == 0 {
		return false
	}
	if completed, ok := cp.State["completed"].(bool); ok && completed {
		return false
	}
	status, _ := cp.State["status"].(string)
	switch status {
	case "complete", "completed", "done":
		return false
	}
	return true
}
