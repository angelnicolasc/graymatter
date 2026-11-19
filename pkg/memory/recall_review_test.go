package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

func TestReviewC111RecallPreservesRankingAndLimitsNoticesToSelectedFacts(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword(), CandidateRetrieval: indexed})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			const namespace = "release"
			const query = "quartz"
			verified, unverified := "verified", "unverified"
			first, err := s.PutWithOptionsReturningFact(ctx, namespace, "Keep the quartz route open.", WriteOptions{Confidence: &verified})
			if err != nil {
				t.Fatal(err)
			}
			second, err := s.PutWithOptionsReturningFact(ctx, namespace, "Close the traffic route.", WriteOptions{Confidence: &unverified})
			if err != nil {
				t.Fatal(err)
			}
			opts := RecallOptions{MinConfidence: &verified}
			before, err := s.RecallWithOptions(ctx, namespace, query, 8, opts)
			if err != nil || len(before.ReviewNotices) != 0 || !slices.Equal(before.Facts, []string{first.Text}) {
				t.Fatalf("initial recall = %+v, %v", before, err)
			}
			records, err := s.ReviewCommit(ctx, ReviewSubmission{
				Namespace: namespace, Provider: "ollama", Model: "fixture", PromptVersion: "review-v1",
				Snapshots: []EvidenceSnapshot{NewEvidenceSnapshot(first), NewEvidenceSnapshot(second)},
				Findings:  []ReviewFinding{{Kind: "conflict", SourceIDs: []string{first.ID, second.ID}, Note: "UNTRUSTED_PROVIDER_NOTE"}},
			})
			if err != nil || len(records) != 1 {
				t.Fatalf("commit conflict = %+v, %v", records, err)
			}
			expectNotice := func(notices []ReviewNotice, state string) {
				t.Helper()
				if len(notices) != 1 || notices[0].ID != records[0].ID || notices[0].SourceState != state ||
					!slices.Equal(notices[0].SourceIDs, []string{first.ID}) {
					t.Fatalf("notices = %+v, want %s and only returned ID %s", notices, state, first.ID)
				}
			}
			after, err := s.RecallWithOptions(ctx, namespace, query, 8, opts)
			if err != nil || !slices.Equal(after.Facts, before.Facts) {
				t.Fatalf("journal affected ranking = %+v, %v", after, err)
			}
			expectNotice(after.ReviewNotices, "current")

			receipts, err := s.RecallExplainWithOptions(ctx, namespace, query, 8, opts)
			if err != nil || len(receipts.Receipts) != 1 || receipts.Receipts[0].Text != first.Text {
				t.Fatalf("explain = %+v, %v", receipts, err)
			}
			expectNotice(receipts.Receipts[0].ReviewNotices, "current")
			batch, err := s.BatchRecallWithOptions(ctx, namespace, []string{query, "route"}, 8, opts)
			if err != nil || len(batch) != 2 {
				t.Fatalf("batch = %+v, %v", batch, err)
			}
			for _, result := range batch {
				expectNotice(result.ReviewNotices, "current")
			}

			if err := s.Delete(namespace, second.ID); err != nil {
				t.Fatal(err)
			}
			after, err = s.RecallWithOptions(ctx, namespace, query, 8, opts)
			if err != nil || !slices.Equal(after.Facts, before.Facts) {
				t.Fatalf("recall after source delete = %+v, %v", after, err)
			}
			expectNotice(after.ReviewNotices, "missing")
			if prose := ReviewNoticesText(after.ReviewNotices); strings.Contains(prose, "UNTRUSTED_PROVIDER_NOTE") || strings.Contains(prose, second.ID) {
				t.Fatalf("unexpected data in recall notice: %q", prose)
			}
			if err := s.ReviewApply(ctx, namespace, records[0].ID, ReviewAction{
				Action: "resolve", Decision: "Verified the live route.", References: []string{first.ID},
			}); err != nil {
				t.Fatal(err)
			}
			closed, err := s.RecallWithOptions(ctx, namespace, query, 8, opts)
			if err != nil || len(closed.ReviewNotices) != 0 || !slices.Equal(closed.Facts, before.Facts) {
				t.Fatalf("recall after explicit resolution = %+v, %v", closed, err)
			}
		})
	}
}

func TestReviewC111NoticeBudgetAndInvalidIDs(t *testing.T) {
	var groups [][]ReviewNotice
	for i := 0; i < 8; i++ {
		var ids []string
		for j := 0; j < 15; j++ {
			ids = append(ids, fmt.Sprintf("fact-%d", j))
		}
		groups = append(groups, []ReviewNotice{{ID: fmt.Sprintf("conflict-%d", i), SourceIDs: ids, SourceState: "current"}})
	}
	groups = append(groups, []ReviewNotice{
		{ID: "conflict-0", SourceIDs: []string{"extra"}, SourceState: "missing"},
		{ID: "provider\ninstruction", SourceIDs: []string{"fact-0"}, SourceState: "changed"},
	})
	got := MergeReviewNotices(groups...)
	if len(got) != 4 || got[0].SourceState != "missing" || len(got[0].SourceIDs) != 12 {
		t.Fatalf("bounded merged notices = %+v", got)
	}
	if text := ReviewNoticesText(got); strings.Count(text, "- Conflict") != 4 || len(text) > 4096 || strings.Contains(text, "instruction") {
		t.Fatalf("unbounded or unexpected notice text: %q", text)
	}
}
