package audit

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func testDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "gray.db"), 0o600,
		&bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func countEntries(t *testing.T, db *bolt.DB) int {
	t.Helper()
	n := 0
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAudit)
		if b == nil {
			return nil
		}
		n = b.Stats().KeyN
		return nil
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// seed writes n entries straight into the bucket, bypassing Write so the
// prune pacing counter is not involved.
func seed(t *testing.T, db *bolt.DB, n int, base time.Time) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketAudit)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			ts := base.Add(time.Duration(i) * time.Millisecond)
			key := ts.UTC().Format(keyTimeFormat) + "_add_agent"
			if err := b.Put([]byte(key), []byte(`{"action":"add"}`)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestWrite_StoresTheEntry(t *testing.T) {
	db := testDB(t)
	e := Entry{
		Timestamp: time.Now().UTC(),
		Action:    "forget",
		Agent:     "planner",
		NewText:   "dropped a stale fact",
		Source:    "mcp",
	}
	if err := Write(db, e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := countEntries(t, db); got != 1 {
		t.Errorf("entries = %d, want 1", got)
	}
}

func TestWrite_NilDBIsANoop(t *testing.T) {
	if err := Write(nil, Entry{}); err != nil {
		t.Errorf("Write(nil) = %v, want nil", err)
	}
}

// TestWrite_PrunesOversizedBucket is the H-13 regression test: the bucket had
// no ceiling, so a long-lived store grew gray.db without bound and slowed
// every scan over it.
func TestWrite_PrunesOversizedBucket(t *testing.T) {
	db := testDB(t)

	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	seed(t, db, MaxEntries+500, base)
	if got := countEntries(t, db); got != MaxEntries+500 {
		t.Fatalf("seeded %d entries, want %d", got, MaxEntries+500)
	}

	// The first Write of a process always prunes, which is what trims a bucket
	// that was already oversized before this version ran.
	writes.Store(0)
	newest := Entry{
		Timestamp: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Action:    "add",
		Agent:     "planner",
		NewText:   "the newest entry",
	}
	if err := Write(db, newest); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := countEntries(t, db); got > MaxEntries {
		t.Errorf("entries = %d after pruning, want at most %d", got, MaxEntries)
	}

	// Pruning drops the oldest, and must not drop what just arrived.
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAudit)
		newestKey := newest.Timestamp.UTC().Format(keyTimeFormat) + "_add_planner"
		if b.Get([]byte(newestKey)) == nil {
			t.Error("pruning removed the entry that was just written")
		}
		oldestKey := base.UTC().Format(keyTimeFormat) + "_add_agent"
		if b.Get([]byte(oldestKey)) != nil {
			t.Error("pruning kept the oldest entry instead of dropping it")
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestWrite_StaysBoundedOverManyWrites exercises the paced prune rather than
// the first-write one.
func TestWrite_StaysBoundedOverManyWrites(t *testing.T) {
	db := testDB(t)

	base := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	seed(t, db, MaxEntries, base)

	writes.Store(1) // past the first-write prune
	for i := 0; i < pruneEvery+10; i++ {
		e := Entry{
			Timestamp: base.Add(time.Duration(MaxEntries+i) * time.Millisecond),
			Action:    "add",
			Agent:     fmt.Sprintf("agent-%d", i),
		}
		if err := Write(db, e); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// The pace means it can drift up to one interval past the ceiling before
	// the next pass trims it, which is the deliberate trade.
	if got := countEntries(t, db); got > MaxEntries+pruneEvery {
		t.Errorf("entries = %d, want at most %d", got, MaxEntries+pruneEvery)
	}
}

// TestWrite_ReportsFailures is the other half of H-13: a failing write used to
// leave no trace at all, so the trail went silently incomplete exactly when
// something was going wrong.
func TestWrite_ReportsFailures(t *testing.T) {
	db := testDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	before := Failures()
	err := Write(db, Entry{Timestamp: time.Now().UTC(), Action: "add", Agent: "a"})
	if err == nil {
		t.Fatal("Write on a closed database returned nil")
	}
	if Failures() <= before {
		t.Errorf("failure counter did not move: %d then %d", before, Failures())
	}
}
