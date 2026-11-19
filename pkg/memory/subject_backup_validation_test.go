package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestSubjectClosedHeadHashValidationAfterGC(t *testing.T) {
	ctx := context.Background()
	s := openAtomicRevisionStore(t, StoreConfig{})
	fact := putSubject(t, s, "policy", "old policy").Fact
	if err := s.Delete(subjectNS, fact.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepLineage(subjectNS, s.now().Add(evidenceOrphanGrace+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectLineage(ctx, subjectNS, fact.ID, NewEvidenceSnapshot(fact).ContentHash); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("fixture still has retained snapshot: %v", err)
	}
	var valid bytes.Buffer
	if err := s.Backup(ctx, &valid); err != nil {
		t.Fatal(err)
	}
	// A well-formed closed reference remains restorable after legitimate GC.
	if err := RestoreLineageBackup(ctx, bytes.NewReader(valid.Bytes()), filepath.Join(t.TempDir(), "valid.db")); err != nil {
		t.Fatalf("valid closed marker after GC rejected: %v", err)
	}
	for _, hash := range []string{"not-a-hash", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		t.Run(hash, func(t *testing.T) {
			if err := s.db.Update(func(tx *bolt.Tx) error {
				bucket, err := evidenceNamespace(tx, bucketSubjectHeads, subjectNS, false)
				if err != nil {
					return err
				}
				var head subjectHead
				if err := json.Unmarshal(bucket.Get([]byte("policy")), &head); err != nil {
					return err
				}
				if head.State != "closed" {
					t.Fatal("fixture must remain closed")
				}
				head.Reference.ContentHash = hash
				raw, err := json.Marshal(head)
				if err != nil {
					return err
				}
				return bucket.Put([]byte("policy"), raw)
			}); err != nil {
				t.Fatal(err)
			}
			var corrupt bytes.Buffer
			if err := s.Backup(ctx, &corrupt); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "rejected.db")
			if err := RestoreLineageBackup(ctx, bytes.NewReader(corrupt.Bytes()), destination); err == nil {
				t.Fatal("restore accepted a malformed closed subject reference")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatalf("invalid backup installed a destination: %v", err)
			}
		})
	}
}
