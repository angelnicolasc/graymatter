package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestLineageL16BackupValidatesHeadIndex(t *testing.T) {
	ctx := context.Background()
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "original shipment policy")
	head := reviseLineageFact(t, s, "amended shipment policy", source)
	otherSource := putLineageFact(t, s, "original return policy")
	otherHead := reviseLineageFact(t, s, "amended return policy", otherSource)
	bridgeSource := putLineageFact(t, s, "legacy bridge source")
	bridge := reviseLineageFact(t, s, "legacy bridge", bridgeSource)
	successor := putLineageFact(t, s, "legacy bridge successor")
	bridge.SupersededBy = successor.ID
	bridge.Weight = 0
	bridge.LineageProjection = LineageProjection{}
	if err := s.UpdateFact(lineageAgent, bridge); err != nil {
		t.Fatal(err)
	}
	prunedSource := putLineageFact(t, s, "pruned head source")
	pruned := reviseLineageFact(t, s, "pruned head", prunedSource)
	reviseLineageFact(t, s, "typed successor", pruned)
	pruneLineageRow(t, s, pruned.ID)
	original := revisionDBSnapshot(t, s)
	valid := lineageBackupBytes(t, s)

	for _, test := range []struct {
		name    string
		corrupt func(*bolt.Tx, *bolt.Bucket) error
	}{
		{
			name: "missing entry",
			corrupt: func(_ *bolt.Tx, heads *bolt.Bucket) error {
				return heads.Delete([]byte(head.ID))
			},
		},
		{
			name: "retired bridge missing entry",
			corrupt: func(_ *bolt.Tx, heads *bolt.Bucket) error {
				return heads.Delete([]byte(bridge.ID))
			},
		},
		{
			name: "pruned head missing entry",
			corrupt: func(_ *bolt.Tx, heads *bolt.Bucket) error {
				return heads.Delete([]byte(pruned.ID))
			},
		},
		{
			name: "missing namespace",
			corrupt: func(tx *bolt.Tx, _ *bolt.Bucket) error {
				return tx.Bucket(bucketLineageHeads).DeleteBucket([]byte(lineageAgent))
			},
		},
		{
			name: "missing bucket",
			corrupt: func(tx *bolt.Tx, _ *bolt.Bucket) error {
				return tx.DeleteBucket(bucketLineageHeads)
			},
		},
		{
			name: "wrong fact ID",
			corrupt: func(_ *bolt.Tx, heads *bolt.Bucket) error {
				return heads.Put([]byte(head.ID), []byte(NewEvidenceSnapshot(otherHead).key()))
			},
		},
		{
			name: "wrong content hash",
			corrupt: func(_ *bolt.Tx, heads *bolt.Bucket) error {
				key := evidenceIdentityKey(lineageAgent, head.ID, NewEvidenceSnapshot(source).ContentHash)
				return heads.Put([]byte(head.ID), []byte(key))
			},
		},
		{
			name: "divergent head missing entry",
			corrupt: func(tx *bolt.Tx, heads *bolt.Bucket) error {
				if err := writeDivergentBackupHead(tx, head); err != nil {
					return err
				}
				return heads.Delete([]byte(head.ID))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := filepath.Join(t.TempDir(), "corrupt.db")
			if err := os.WriteFile(image, valid, 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := bolt.Open(image, 0o600, nil)
			if err != nil {
				t.Fatal(err)
			}
			updateErr := db.Update(func(tx *bolt.Tx) error {
				heads, err := evidenceNamespace(tx, bucketLineageHeads, lineageAgent, false)
				if err != nil {
					return err
				}
				return test.corrupt(tx, heads)
			})
			if err := errors.Join(updateErr, db.Close()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(image)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "restored.db")
			err = RestoreLineageBackup(ctx, bytes.NewReader(data), target)
			if err == nil || !strings.Contains(err.Error(), "backup lineage head index mismatch") {
				t.Errorf("incoherent lineage restore = %v, want head index mismatch", err)
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("rejected restore left destination: %v", err)
			}
			if !reflect.DeepEqual(original, revisionDBSnapshot(t, s)) {
				t.Fatal("rejected restore changed the original store")
			}
		})
	}
}

func TestLineageL16BackupRestoresLiveHistoryAndOrphanGrace(t *testing.T) {
	ctx := context.Background()
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "historical shared source")
	in := reviewFixture("ollama", "fixture", "backup shared evidence", source)
	in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{source.ID}, Summary: "candidate"}}
	review := commitReviewFixture(t, s, in)[0]
	middle := reviseLineageFact(t, s, "intermediate version", source)
	head := reviseLineageFact(t, s, "authoritative head version", middle)
	pruneLineageRow(t, s, source.ID)
	pruneLineageRow(t, s, middle.ID)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return writeDivergentBackupHead(tx, head)
	}); err != nil {
		t.Fatal(err)
	}

	orphanSource := putLineageFact(t, s, "independent orphan source")
	orphanHead := reviseLineageFact(t, s, "independent orphan head", orphanSource)
	if err := s.Delete(lineageAgent, orphanHead.ID); err != nil {
		t.Fatal(err)
	}

	targetDir := t.TempDir()
	target := filepath.Join(targetDir, filepath.Base(s.db.Path()))
	if err := RestoreLineageBackup(ctx, bytes.NewReader(lineageBackupBytes(t, s)), target); err != nil {
		t.Fatal(err)
	}
	restored := openLineageStore(t, targetDir, false)
	current, err := restored.InspectLineage(ctx, lineageAgent, head.ID, "")
	if err != nil || current.LineageStatus != LineageContentChanged || current.HistoricalHash != NewEvidenceSnapshot(head).ContentHash {
		t.Fatalf("restored divergent head = %+v, %v", current, err)
	}
	pruned, err := restored.InspectLineage(ctx, lineageAgent, middle.ID, "")
	if err != nil || !reflect.DeepEqual(pruned, inspectVersion(t, s, middle)) {
		t.Fatalf("restored pruned head lookup = %+v, %v", pruned, err)
	}
	if !reflect.DeepEqual(inspectReviewFixture(t, s, lineageAgent, review.ID), inspectReviewFixture(t, restored, lineageAgent, review.ID)) {
		t.Fatal("restore changed the journal's shared evidence")
	}
	if err := restored.sweepLineage(lineageAgent, lineageTime.Add(evidenceOrphanGrace-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []Fact{orphanSource, orphanHead} {
		if !reflect.DeepEqual(inspectVersion(t, s, f), inspectVersion(t, restored, f)) {
			t.Fatalf("restore lost or changed orphan %s inside grace", f.ID)
		}
	}
	if err := restored.sweepLineage(lineageAgent, lineageTime.Add(evidenceOrphanGrace)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []Fact{source, middle, head} {
		if !reflect.DeepEqual(inspectVersion(t, s, f), inspectVersion(t, restored, f)) {
			t.Fatalf("restore lost or changed retained version %s after grace", f.ID)
		}
	}
	for _, f := range []Fact{orphanSource, orphanHead} {
		if _, err := restored.InspectLineage(ctx, lineageAgent, f.ID, NewEvidenceSnapshot(f).ContentHash); !errors.Is(err, ErrFactNotFound) {
			t.Fatalf("orphan %s did not expire after its original grace: %v", f.ID, err)
		}
	}
}

func lineageBackupBytes(t *testing.T, s *Store) []byte {
	t.Helper()
	var image bytes.Buffer
	if err := s.Backup(context.Background(), &image); err != nil {
		t.Fatal(err)
	}
	return image.Bytes()
}

func writeDivergentBackupHead(tx *bolt.Tx, head Fact) error {
	// Simulate a canonical rewrite by a writer without lineage fields.
	head.Text = "policy changed by a legacy writer"
	head.LineageProjection = LineageProjection{}
	raw, err := json.Marshal(head)
	if err != nil {
		return err
	}
	return lineageRowBucket(tx, head.AgentID).Put([]byte(head.ID), raw)
}
