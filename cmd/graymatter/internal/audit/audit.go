// Package audit persists the agent self-edit trail (memory_reflect actions)
// to the kg_audit bbolt bucket. It is shared by the MCP server (direct mode)
// and the daemon host service (client mode) so both write the same format.
package audit

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucketAudit = []byte("kg_audit")

const (
	// MaxEntries is the ceiling on the audit bucket. Every memory_reflect call
	// appends one entry and nothing removed them, so a long-lived store grew
	// gray.db without bound and slowed every scan over that bucket.
	//
	// Trimming the oldest is the right trade for a self-edit trail: recent
	// history is what anyone investigating actually reads.
	MaxEntries = 10000

	// pruneEvery is how many writes pass between prune passes. Counting the
	// bucket means walking it, so doing that on every self-edit would be a
	// cost paid constantly to catch a condition that arrives slowly. Between
	// passes the bucket can drift up to this far past MaxEntries, which is the
	// deliberate trade.
	pruneEvery = 256

	// keyTimeFormat is RFC3339 with a fixed nine-digit fraction.
	//
	// time.RFC3339Nano trims trailing zeros, and bbolt orders keys by bytes:
	// "…:00Z" sorts after "…:00.5Z" because '.' < 'Z'. Pruning walks the
	// bucket front to back expecting time order, so the fraction has to be
	// fixed width. Still valid RFC3339, so anything parsing these keys keeps
	// working.
	keyTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"
)

// writes counts calls to Write in this process, to pace pruning.
var writes atomic.Uint64

// failures counts audit writes that did not land.
//
// Write used to swallow every error, which meant the trail went silently
// incomplete exactly when something was going wrong. Callers get the error
// now, and this counter is here for the ones that legitimately cannot act on
// it — audit must never fail the operation it records, but it should not be
// able to fail invisibly either.
var failures atomic.Uint64

// Failures reports how many audit writes have failed in this process.
func Failures() uint64 { return failures.Load() }

// Entry is one self-edit event. Timestamps are UTC.
type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Agent     string    `json:"agent"`
	OldText   string    `json:"old_text,omitempty"`
	NewText   string    `json:"new_text"`
	Source    string    `json:"source"`
}

// Write persists entry and returns whatever went wrong.
//
// Callers are expected to record the error rather than abort on it: the
// operation being audited has already happened by the time this runs. What
// changed is that the error is now available to record at all.
func Write(db *bolt.DB, entry Entry) error {
	if db == nil {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		failures.Add(1)
		return fmt.Errorf("audit: encode entry: %w", err)
	}
	key := []byte(entry.Timestamp.UTC().Format(keyTimeFormat) + "_" + entry.Action + "_" + entry.Agent)

	n := writes.Add(1)
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketAudit)
		if err != nil {
			return err
		}
		// Prune before inserting, so the bucket lands exactly on MaxEntries
		// rather than one over it. The first write of the process always
		// prunes: that is what trims a bucket which was already oversized
		// before this version ran. After that, pace it.
		if n == 1 || n%pruneEvery == 0 {
			if err := prune(b, MaxEntries-1); err != nil {
				return err
			}
		}
		return b.Put(key, data)
	})
	if err != nil {
		failures.Add(1)
		return fmt.Errorf("audit: write entry: %w", err)
	}
	return nil
}

// prune drops the oldest entries until at most keep remain.
//
// Keys start with a fixed-width timestamp (see keyTimeFormat), so bbolt's byte
// ordering is time ordering and walking from the front is walking from the
// oldest. Entries written by an older version used RFC3339Nano, whose variable
// fraction can misorder entries inside a single second; that is close enough
// for "which ten thousand are the newest".
func prune(b *bolt.Bucket, keep int) error {
	total := b.Stats().KeyN
	if total <= keep {
		return nil
	}

	excess := total - keep
	doomed := make([][]byte, 0, excess)
	c := b.Cursor()
	for k, _ := c.First(); k != nil && len(doomed) < excess; k, _ = c.Next() {
		// The cursor's key is only valid until the next call, so copy it.
		kc := make([]byte, len(k))
		copy(kc, k)
		doomed = append(doomed, kc)
	}
	for _, k := range doomed {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
