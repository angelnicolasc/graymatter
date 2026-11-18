package memory

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestSubjectExplicitRecoveryFromCorruptSnapshotWithExactCanonicalProof(t *testing.T) {
	ctx := context.Background()
	s := openAtomicRevisionStore(t, StoreConfig{})
	subject := "policy"
	old, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "Previous routing policy", WriteOptions{
		Subject: &subject,
		Source:  subjectString("memo"),
		Quote:   subjectString("Exact literal\r\n\tá 漢字"),
	})
	if err != nil {
		t.Fatal(err)
	}
	version := NewEvidenceSnapshot(old)
	damage := []byte("{invalid snapshot")
	var originalHead subjectHead
	if err := s.db.Update(func(tx *bolt.Tx) error {
		heads, err := subjectHeadsTx(tx, subjectNS)
		if err != nil {
			return err
		}
		originalHead = heads[subject]
		snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, subjectNS, false)
		if err != nil {
			return err
		}
		return snapshots.Put([]byte(version.key()), damage)
	}); err != nil {
		t.Fatal(err)
	}

	result, err := s.SearchScoped(ctx, subjectNS, []string{"routing policy"}, 8, false, RecallOptions{})
	if err != nil || len(result.Facts) != 1 || result.Facts[0].ID != old.ID ||
		len(result.Facts[0].Subjects) != 0 || result.Facts[0].Evidence != nil ||
		result.SubjectIssues == nil || result.SubjectIssues.Total != 1 {
		t.Fatalf("corrupt snapshot was projected or blocked the read: %+v %v", result, err)
	}
	if err := s.Consolidate(ctx, subjectNS, &testConsolidateCfg{halfLife: 720 * time.Hour}); err != nil {
		t.Fatalf("unresolved subject blocked maintenance: %v", err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		heads, err := subjectHeadsTx(tx, subjectNS)
		if err != nil {
			return err
		}
		snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, subjectNS, false)
		if err != nil {
			return err
		}
		if heads[subject] != originalHead || snapshots == nil || !bytes.Equal(snapshots.Get([]byte(version.key())), damage) {
			return fmt.Errorf("read or maintenance changed unresolved evidence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	recovered, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "Current routing policy", WriteOptions{
		Subject: &subject, Replaces: []string{old.ID},
	})
	if err != nil {
		t.Fatalf("explicit recovery with an exact canonical proof failed: %v", err)
	}
	result, err = s.SearchScoped(ctx, subjectNS, []string{"routing policy"}, 8, false, RecallOptions{})
	if err != nil || result.SubjectIssues != nil || len(result.Facts) != 1 || result.Facts[0].ID != recovered.ID ||
		len(result.Facts[0].Subjects) != 1 || result.Facts[0].Subjects[0] != (SubjectProjection{Name: subject, Revisions: 1}) ||
		result.Facts[0].Evidence != nil {
		t.Fatalf("explicit recovery did not establish just the new head: %+v %v", result, err)
	}
	retained, err := s.FetchCitation(ctx, subjectNS, old.ID, version.ContentHash)
	if err != nil || retained.Evidence == nil || retained.Evidence.Source != old.Evidence.Source || retained.Evidence.Quote != old.Evidence.Quote {
		t.Fatalf("recovery lost verified old citation: %+v %v", retained, err)
	}
}
