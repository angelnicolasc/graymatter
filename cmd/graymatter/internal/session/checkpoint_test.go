package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func checkpointDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "checkpoints.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket(bucketSessions)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLatestRejectsCorruptCheckpoint(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "only corrupt", true: "valid and corrupt"}[mixed], func(t *testing.T) {
			db := checkpointDB(t)
			if mixed {
				if _, err := Save(db, Checkpoint{AgentID: "a"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				b, err := tx.Bucket(bucketSessions).CreateBucketIfNotExists([]byte("a"))
				if err != nil {
					return err
				}
				return b.Put([]byte("broken"), []byte("invalid JSON"))
			}); err != nil {
				t.Fatal(err)
			}
			cp, err := Latest(db, "a")
			var decodeErr *json.SyntaxError
			if !errors.As(err, &decodeErr) || errors.Is(err, ErrNoCheckpoint) || cp != nil {
				t.Fatalf("Latest = %+v, %v; corruption must prevent a successful resume", cp, err)
			}
		})
	}
}

func TestResumeDistinguishesAbsenceFromStorageFailure(t *testing.T) {
	db := checkpointDB(t)
	if cp, err := Resume(db, "missing"); cp != nil || !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("missing agent = %+v, %v; want ErrNoCheckpoint", cp, err)
	}
	older, err := Save(db, Checkpoint{AgentID: "a", CreatedAt: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Save(db, Checkpoint{AgentID: "a", CreatedAt: time.Unix(2, 0), State: map[string]any{"step": "two"}})
	if err != nil {
		t.Fatal(err)
	}
	if cp, err := Resume(db, "a"); err != nil || cp.ID != newer.ID || cp.State["step"] != "two" {
		t.Fatalf("resume latest = %+v, %v", cp, err)
	}
	for _, cp := range []Checkpoint{older, newer} {
		if err := Delete(db, "a", cp.ID); err != nil {
			t.Fatal(err)
		}
	}
	if cp, err := Resume(db, "a"); cp != nil || !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("empty agent bucket = %+v, %v; want ErrNoCheckpoint", cp, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if cp, err := Resume(db, "a"); cp != nil || !errors.Is(err, bolt.ErrDatabaseNotOpen) || errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("closed database = %+v, %v; want storage error", cp, err)
	}
}
