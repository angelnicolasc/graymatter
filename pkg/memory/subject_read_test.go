package memory

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestSubjectScopedSearchPreservesUncitedIdentityScanAndIndexed(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed-%t", indexed), func(t *testing.T) {
			s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: indexed})
			ctx := context.Background()
			a := putSubject(t, s, "A", "routing same text").Fact
			b := putSubject(t, s, "B", "routing same text").Fact
			shared, err := s.PutWithOptionsReturningFact(ctx, SharedAgentID, "routing same text", WriteOptions{Subject: subjectString("A")})
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := s.PutWithOptionsReturningFact(ctx, "other-project", "routing same text", WriteOptions{Subject: subjectString("A")})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.SearchScoped(ctx, subjectNS, []string{"routing", "routing"}, 8, true, RecallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{a.ID: subjectNS, b.ID: subjectNS, shared.ID: SharedAgentID}
			if len(got.Facts) != 3 {
				t.Fatalf("subject identities collapsed: %+v", got)
			}
			for _, f := range got.Facts {
				if want[f.ID] != f.Namespace || f.ID == foreign.ID || len(f.Subjects) != 1 || f.Evidence != nil || f.ShortID == "" {
					t.Fatalf("bad identity %+v", f)
				}
				resolved, err := s.ResolveFactTargets(ctx, f.Namespace, f.ShortID, "id", false)
				if err != nil || resolved[0].ID != f.ID {
					t.Fatalf("displayed prefix ambiguous: %+v %v", f, err)
				}
				delete(want, f.ID)
			}
			if len(want) != 0 || len(got.PerQuery) != 2 || !reflect.DeepEqual(got.PerQuery[0], got.PerQuery[1]) {
				t.Fatalf("per-query identity mapping %+v", got)
			}
			project, err := s.SearchScoped(ctx, subjectNS, []string{"routing"}, 8, false, RecallOptions{})
			if err != nil || len(project.Facts) != 2 {
				t.Fatalf("project scope %+v %v", project, err)
			}
			legacy, err := s.RecallAll(ctx, subjectNS, "routing", 8)
			if err != nil || len(legacy) != 1 {
				t.Fatalf("legacy text dedup changed %v %v", legacy, err)
			}
		})
	}
}

func TestSubjectAgeAndNamespaceIsolation(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	base := s.now()
	old := putSubject(t, s, "policy", "routing old").Fact
	s.now = func() time.Time { return base.Add(49 * time.Hour) }
	got, err := s.SearchScoped(context.Background(), subjectNS, []string{"routing"}, 8, false, RecallOptions{})
	if err != nil || len(got.Facts) != 1 || got.Facts[0].ID != old.ID || got.Facts[0].AgeDays != 49.0/24 {
		t.Fatalf("age %+v %v", got, err)
	}
	s.now = func() time.Time { return base.Add(-time.Hour) }
	got, err = s.SearchScoped(context.Background(), subjectNS, []string{"routing"}, 8, false, RecallOptions{})
	if err != nil || got.Facts[0].AgeDays != 0 {
		t.Fatalf("negative age %+v %v", got, err)
	}
	_, err = s.PutWithOptionsReturningFact(context.Background(), "other", "routing other", WriteOptions{Subject: subjectString("policy")})
	if err != nil {
		t.Fatal(err)
	}
	newer := putSubject(t, s, "policy", "routing current")
	rows, err := s.List("other")
	if err != nil || len(rows) != 1 || rows[0].IsSuperseded() || rows[0].Subjects[0].Revisions != 0 || len(newer.Replaced) != 1 || newer.Replaced[0].ID != old.ID {
		t.Fatalf("subject crossed namespace %+v %+v %v", rows, newer, err)
	}
}

func TestSubjectShortIDsRespectFullIDPriorityAndUTF8(t *testing.T) {
	ids := []string{"abcdefgh", "abcdefgh-more", "漢字漢字a", "漢字漢字b"}
	got := shortFactIDs(ids)
	if got["abcdefgh"] != "abcdefgh" || got["abcdefgh-more"] != "abcdefgh-" || got["漢字漢字a"] != "漢字漢字a" || got["漢字漢字b"] != "漢字漢字b" {
		t.Fatalf("unsafe prefixes %+v", got)
	}
}
