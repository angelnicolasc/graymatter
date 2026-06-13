// Package audit persists the agent self-edit trail (memory_reflect actions)
// to the kg_audit bbolt bucket. It is shared by the MCP server (direct mode)
// and the daemon host service (client mode) so both write the same format.
package audit

import (
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucketAudit = []byte("kg_audit")

// Entry is one self-edit event. Timestamps are UTC.
type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Agent     string    `json:"agent"`
	OldText   string    `json:"old_text,omitempty"`
	NewText   string    `json:"new_text"`
	Source    string    `json:"source"`
}

// Write persists entry best-effort: audit must never fail the operation it
// records, so errors are deliberately swallowed.
func Write(db *bolt.DB, entry Entry) {
	if db == nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	key := []byte(entry.Timestamp.Format(time.RFC3339Nano) + "_" + entry.Action + "_" + entry.Agent)
	_ = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketAudit)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}
