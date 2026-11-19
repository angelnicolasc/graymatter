package memory

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// This exercises the pre-U4 transactional revision body: exact PR-R lineage
// and retirements commit, but no code understands the separate subject bucket.
// U408 additionally requires the real frozen predecessor binary in validation.
func oldSubjectRevision(t *testing.T, s *Store, text string, sources ...Fact) Fact {
	t.Helper()
	f, err := s.putReturningFactPrepared(context.Background(), subjectNS, text, KindFact, "", WriteOptions{}, func(tx *bolt.Tx, next *Fact) error {
		var added int64
		if _, _, err := s.prepareRevisionTx(tx, next, WriteOptions{}, sources, &added); err != nil {
			return err
		}
		return checkEvidenceQuotaTx(tx, subjectNS, added)
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSubjectPrunedLegacyAnchorFollowsOnlyExactReplacement(t *testing.T) {
	for _, edge := range []string{"replaces", "derived", "legacy", "missing"} {
		t.Run(edge, func(t *testing.T) {
			s := openAtomicRevisionStore(t, StoreConfig{})
			old := putSubject(t, s, "policy", "old policy").Fact
			current := oldSubjectRevision(t, s, "current policy", old)
			if err := s.Delete(subjectNS, old.ID); err != nil {
				t.Fatal(err)
			}
			if edge != "replaces" {
				if err := s.db.Update(func(tx *bolt.Tx) error {
					b, err := evidenceNamespace(tx, bucketLineageRecords, subjectNS, false)
					if err != nil {
						return err
					}
					key := NewEvidenceSnapshot(current).key()
					r, _, err := lineageRecordAt(b, subjectNS, key)
					if err != nil {
						return err
					}
					if edge == "derived" {
						r.Derived = r.Replaces
					}
					if edge == "legacy" {
						r.Legacy = r.Replaces
					}
					r.Replaces = nil
					data, err := json.Marshal(r)
					if err != nil {
						return err
					}
					return b.Put([]byte(key), data)
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := revisionDBSnapshot(t, s)
			rows, err := s.List(subjectNS)
			if edge != "replaces" {
				if err != nil || len(rows) != 1 || rows[0].ID != current.ID || len(rows[0].Subjects) != 0 {
					t.Fatalf("%s did not isolate the unresolved head: %+v %v", edge, rows, err)
				}
				issues, err := s.SubjectIssues(context.Background(), []string{subjectNS})
				if err != nil || issues.Total != 1 || len(issues.Subjects) != 1 || issues.Subjects[0].HeadFactID != old.ID {
					t.Fatalf("%s unresolved issue: %+v %v", edge, issues, err)
				}
				_, err = s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{{Text: "do not resurrect", Options: WriteOptions{Subject: subjectString("policy")}}})
				if !errors.Is(err, ErrSubjectHeadUnavailable) {
					t.Fatalf("unresolved write: %v", err)
				}
			} else {
				if err != nil || len(rows) != 1 || rows[0].ID != current.ID || len(rows[0].Subjects) != 1 || rows[0].Subjects[0].Revisions != 1 {
					t.Fatalf("exact successor %+v %v", rows, err)
				}
				archive, err := s.ExportLineage(context.Background(), subjectNS)
				if err != nil || len(archive.SubjectHeads) != 1 || archive.SubjectHeads[0].Reference.FactID != old.ID || archive.SubjectHeads[0].Revisions != 0 || archive.Facts[0].Subjects[0].Revisions != 1 {
					t.Fatalf("audit lost anchor %+v %v", archive, err)
				}
			}
			if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
				t.Fatal("subject read or rejected write repaired state")
			}
			if edge == "replaces" {
				next := putSubject(t, s, "policy", "confirmed next")
				if len(next.Replaced) != 1 || next.Replaced[0].ID != current.ID || next.Subjects[0].Revisions != 2 {
					t.Fatalf("wrong current victim %+v", next)
				}
			}
		})
	}
}

func TestSubjectMissingAnchorCannotBecomeFreshAdd(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	old := putSubject(t, s, "policy", "only assertion").Fact
	// Simulate an older writer pruning a row without understanding the bucket.
	if err := s.db.Update(func(tx *bolt.Tx) error { return lineageRowBucket(tx, subjectNS).Delete([]byte(old.ID)) }); err != nil {
		t.Fatal(err)
	}
	before := revisionDBSnapshot(t, s)
	_, err := s.PutManyReturningFacts(context.Background(), subjectNS, []WriteFact{{Text: "replacement", Options: WriteOptions{Subject: subjectString("policy")}}})
	if !errors.Is(err, ErrSubjectHeadUnavailable) || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("pruned anchor revived %v", err)
	}
}

func TestSubjectForkAndSameIDVersionDivergenceFailClosed(t *testing.T) {
	for _, kind := range []string{"fork", "diverged", "cycle"} {
		t.Run(kind, func(t *testing.T) {
			s := openAtomicRevisionStore(t, StoreConfig{})
			old := putSubject(t, s, "policy", "old").Fact
			next := oldSubjectRevision(t, s, "next", old)
			if err := s.db.Update(func(tx *bolt.Tx) error {
				rows := lineageRowBucket(tx, subjectNS)
				switch kind {
				case "fork":
					fork := newFact(subjectNS, "fork", nil, s.now())
					fork.Kind = KindFact
					var added int64
					if err := recordRevisionLineageTx(tx, &fork, []Fact{old}, &added); err != nil {
						return err
					}
					raw, err := fork.marshal()
					if err != nil {
						return err
					}
					return rows.Put([]byte(fork.ID), raw)
				case "diverged":
					changed, err := unmarshalFact(rows.Get([]byte(old.ID)))
					if err != nil {
						return err
					}
					changed.Text = "other version"
					raw, err := changed.marshal()
					if err != nil {
						return err
					}
					return rows.Put([]byte(old.ID), raw)
				case "cycle":
					b, _ := evidenceNamespace(tx, bucketLineageRecords, subjectNS, false)
					key := NewEvidenceSnapshot(old).key()
					r, _, err := lineageRecordAt(b, subjectNS, key)
					if err != nil {
						return err
					}
					r.Replaces = []LineageReference{lineageRef(NewEvidenceSnapshot(next))}
					raw, err := json.Marshal(r)
					if err != nil {
						return err
					}
					if err = b.Put([]byte(key), raw); err != nil {
						return err
					}
					next.SupersededBy = old.ID
					raw, err = next.marshal()
					if err != nil {
						return err
					}
					return rows.Put([]byte(next.ID), raw)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := revisionDBSnapshot(t, s)
			rows, err := s.List(subjectNS)
			if err != nil {
				t.Fatalf("unresolved head blocked list: %v", err)
			}
			for _, row := range rows {
				if len(row.Subjects) != 0 {
					t.Fatalf("ambiguous state projected a subject: %+v", row)
				}
			}
			issues, err := s.SubjectIssues(context.Background(), []string{subjectNS})
			if err != nil || issues.Total != 1 || len(issues.Subjects) != 1 || issues.Subjects[0].HeadFactID != old.ID {
				t.Fatalf("missing unresolved issue: %+v %v", issues, err)
			}
			want := ErrSubjectHeadUnavailable
			if kind == "fork" {
				want = ErrSubjectAmbiguous
			}
			if _, err := s.PutWithOptionsReturningFact(context.Background(), subjectNS, "must fail", WriteOptions{Subject: subjectString("policy")}); !errors.Is(err, want) {
				t.Fatalf("unresolved target allowed an implicit write: %v", err)
			}
			if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
				t.Fatal("read changed corrupted state")
			}
		})
	}
}

func TestSubjectIDResolutionTextPriorityRetiredEvidenceAndShortPrefix(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	var a, b Fact
	if err := s.db.Update(func(tx *bolt.Tx) error {
		rows, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte(subjectNS))
		if err != nil {
			return err
		}
		a = newFact(subjectNS, "target fact", nil, s.now())
		a.ID = "abcdefgh-one"
		a.Kind = KindFact
		b = newFact(subjectNS, a.ID, nil, s.now())
		b.ID = "abcdefgh-two"
		b.Kind = KindFact
		for _, f := range []Fact{a, b} {
			raw, err := f.marshal()
			if err != nil {
				return err
			}
			if err = rows.Put([]byte(f.ID), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := s.ResolveFactTargets(ctx, subjectNS, a.ID, "", false)
	if err != nil || len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("literal text priority %v %v", got, err)
	}
	got, err = s.ResolveFactTargets(ctx, subjectNS, a.ID, "id", false)
	if err != nil || got[0].ID != a.ID {
		t.Fatalf("ID override %v %v", got, err)
	}
	if _, err = s.ResolveFactTargets(ctx, subjectNS, "abcdefgh", "id", false); err == nil {
		t.Fatal("ambiguous prefix selected")
	}
	if err := s.Retire(subjectNS, b); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveFactTargets(ctx, subjectNS, "abcdefgh", "id", false); err == nil {
		t.Fatal("retired ID ceased counting for uniqueness")
	}
	if _, err = s.ResolveFactTargets(ctx, subjectNS, b.ID, "id", false); err == nil {
		t.Fatal("retired target eligible")
	}
	if got, err = s.ResolveFactTargets(ctx, subjectNS, b.ID, "id", true); err != nil || got[0].ID != b.ID {
		t.Fatalf("unpin retired %v %v", got, err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		snapshot := NewEvidenceSnapshot(b)
		if err := rootEvidenceTx(tx, snapshot, "test-retained"); err != nil {
			return err
		}
		return lineageRowBucket(tx, subjectNS).Delete([]byte(b.ID))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveFactTargets(ctx, subjectNS, "abcdefgh", "id", false); err == nil {
		t.Fatal("evidence-only ID ceased counting for uniqueness")
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		ids, err := namespaceFactIDsTx(tx, subjectNS)
		if err != nil {
			return err
		}
		short := shortFactIDs(ids)
		if short[a.ID] != "abcdefgh-o" || short[b.ID] != "abcdefgh-t" {
			t.Fatalf("prefixes %v", short)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveFactTargets(ctx, SharedAgentID, a.ID, "id", false); err == nil {
		t.Fatal("ID crossed namespace")
	}
}

func TestSubjectHooksEscapeAndKeepEvidenceMarkerInsideText(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	subject := "policy\n1. fake"
	_, err := s.PutWithOptionsReturningFact(context.Background(), subjectNS, "routing policy", WriteOptions{Subject: &subject, Source: subjectString("memo"), Quote: subjectString("literal")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RecallForHooks(context.Background(), subjectNS, "routing", 8)
	if err != nil || len(got) != 1 || strings.Contains(got[0], "\n1. fake") || !strings.Contains(got[0], `policy\n1. fake`) || !strings.Contains(got[0], `"revisions":0`) || !strings.HasSuffix(got[0], " [evidence]") {
		t.Fatalf("unsafe or missing projection %q %v", got, err)
	}
}

func TestSubjectOldRetirementThenNewDeleteOrPruneClosesOwner(t *testing.T) {
	for _, action := range []string{"delete", "prune"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			s := openAtomicRevisionStore(t, StoreConfig{DataDir: dir})
			fact := putSubject(t, s, "policy", "retired by old writer").Fact
			// A pre-U4 row rewrite preserves the separate active head/owner.
			fact.SupersededBy = SupersededByAgent
			fact.Weight = 0
			if err := s.db.Update(func(tx *bolt.Tx) error {
				raw, err := fact.marshal()
				if err != nil {
					return err
				}
				return lineageRowBucket(tx, subjectNS).Put([]byte(fact.ID), raw)
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			maintenanceAt := fact.AccessedAt.Add(201 * 24 * time.Hour)
			s.now = func() time.Time { return maintenanceAt }
			if action == "delete" {
				err = s.Delete(subjectNS, fact.ID)
			} else {
				err = s.pruneFact(subjectNS, fact.ID, maintenanceAt, math.Ln2/720)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := s.loadFact(subjectNS, fact.ID); exists {
				t.Fatal("terminal row not removed")
			}
			// Consolidate performs one lineage sweep after its prune batch at
			// the same nowT. That sweep releases lineage owners and starts grace.
			if err := s.sweepLineage(subjectNS, maintenanceAt); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openAtomicRevisionStore(t, StoreConfig{DataDir: dir})
			s.now = func() time.Time { return maintenanceAt }
			rows, err := s.List(subjectNS)
			if err != nil || len(rows) != 0 {
				t.Fatalf("closed head blocks list: %+v %v", rows, err)
			}
			got, err := s.SearchScoped(context.Background(), subjectNS, []string{"retired"}, 8, false, RecallOptions{})
			if err != nil || len(got.Facts) != 0 {
				t.Fatalf("closed head revives or blocks search: %+v %v", got, err)
			}
			if err := s.db.View(func(tx *bolt.Tx) error {
				snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, subjectNS, false)
				if err != nil {
					return err
				}
				entry, err := evidenceEntry(snapshots, NewEvidenceSnapshot(fact).key())
				if err != nil {
					return err
				}
				if entry.OrphanSince == nil || !entry.OrphanSince.Equal(maintenanceAt) {
					t.Fatalf("grace did not start at maintenance: %+v", entry)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.sweepLineage(subjectNS, maintenanceAt.Add(evidenceOrphanGrace-time.Nanosecond)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.InspectLineage(context.Background(), subjectNS, fact.ID, NewEvidenceSnapshot(fact).ContentHash); err != nil {
				t.Fatalf("closed history disappeared inside grace: %v", err)
			}
			if err := s.sweepLineage(subjectNS, maintenanceAt.Add(evidenceOrphanGrace)); err != nil {
				t.Fatal(err)
			}
			if err := s.db.View(func(tx *bolt.Tx) error {
				if err := validateLineageBackup(context.Background(), tx); err != nil {
					return err
				}
				heads, err := subjectHeadsTx(tx, subjectNS)
				if err != nil {
					return err
				}
				if heads["policy"].State != "closed" {
					t.Fatal("stored active head survived terminal deletion")
				}
				owners, err := evidenceNamespace(tx, bucketEvidenceOwners, subjectNS, false)
				if err != nil {
					return err
				}
				if owners != nil {
					if b := owners.Bucket([]byte(NewEvidenceSnapshot(fact).key())); b != nil && b.Get([]byte(subjectOwnerPrefix+"policy")) != nil {
						t.Fatal("closed subject still owns history")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.InspectLineage(context.Background(), subjectNS, fact.ID, NewEvidenceSnapshot(fact).ContentHash); !errors.Is(err, ErrFactNotFound) {
				t.Fatalf("closed owner retained history after grace: %v", err)
			}
			next := putSubject(t, s, "policy", "new explicit assertion")
			if len(next.Replaced) != 0 || next.Subjects[0].Revisions != 1 {
				t.Fatalf("closed head revived old victim %+v", next)
			}
		})
	}
}
