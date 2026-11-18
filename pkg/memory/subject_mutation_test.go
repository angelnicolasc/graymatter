package memory

import (
	"context"
	"testing"
)

func TestSubjectPinnedIDReplacesUsesCompleteMutationSnapshot(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	ctx := context.Background()
	old, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "policy old", WriteOptions{Subject: subjectString("policy"), Confidence: subjectString("unverified")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned(subjectNS, true, old); err != nil {
		t.Fatal(err)
	}
	receipts, err := s.PutManyReturningReceipts(ctx, subjectNS, []WriteFact{{Text: "policy current", Options: WriteOptions{Replaces: []string{old.ID}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Fact.Pinned || !receipts[0].Fact.PinnedAt.IsZero() || receipts[0].Fact.Confidence != "unverified" || len(receipts[0].Replaced) != 1 || receipts[0].Replaced[0].ID != old.ID || receipts[0].Subjects[0].Revisions != 1 {
		t.Fatalf("incorrect pinned ID replacement %+v", receipts)
	}
}

func TestSubjectTargetResolverPreservesPinSnapshotForRevisionAndCuration(t *testing.T) {
	for _, selection := range []string{"id", "text", "auto-text"} {
		for _, mutation := range []string{"revise", "curate"} {
			t.Run(selection+"/"+mutation, func(t *testing.T) {
				s := openAtomicRevisionStore(t, StoreConfig{})
				ctx := context.Background()
				old := putSubject(t, s, "policy", "policy old").Fact
				if err := s.SetPinned(subjectNS, true, old); err != nil {
					t.Fatal(err)
				}
				target, targetType := old.Text, selection
				if selection == "id" {
					target = old.ID
				}
				if selection == "auto-text" {
					targetType = ""
				}
				selected, err := s.ResolveFactTargets(ctx, subjectNS, target, targetType, false)
				if err != nil {
					t.Fatal(err)
				}
				if len(selected) != 1 || selected[0].ID != old.ID || !selected[0].Pinned || !selected[0].PinnedAt.Equal(s.now()) || !selected[0].CreatedAt.Equal(old.CreatedAt) {
					t.Fatalf("incomplete mutation snapshot %+v", selected)
				}
				var replacement Fact
				if mutation == "revise" {
					replacement, err = s.ReviseFactsWithOptions(ctx, subjectNS, "policy current", WriteOptions{}, selected...)
				} else {
					replacement, err = s.CurateFact(ctx, subjectNS, old.ID, "revise", "policy current", selected[0])
				}
				if err != nil {
					t.Fatal(err)
				}
				if replacement.ID == old.ID || replacement.Pinned || !replacement.PinnedAt.IsZero() || len(replacement.Subjects) != 1 || replacement.Subjects[0].Revisions != 1 {
					t.Fatalf("invalid revision %+v", replacement)
				}
				retired, ok := s.loadFact(subjectNS, old.ID)
				if !ok || retired.SupersededBy != replacement.ID || !retired.Pinned || !retired.PinnedAt.Equal(selected[0].PinnedAt) {
					t.Fatalf("source metadata lost %+v", retired)
				}
			})
		}
	}
}
