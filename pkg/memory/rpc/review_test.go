package rpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type legacyReviewService struct {
	analyzes atomic.Int64
	writes   atomic.Int64
}

func (*legacyReviewService) Ping(_ *PingRequest, resp *PingResponse) error {
	resp.Protocol = Protocol
	resp.Capabilities = []string{MaintenanceOnlyV1, InspectionContextV1}
	return nil
}

func (s *legacyReviewService) ReviewAnalyze(_ *ReviewAnalyzeRequest, _ *ReviewRecordsResponse) error {
	s.analyzes.Add(1)
	return nil
}

func (s *legacyReviewService) ReviewApply(_ *ReviewApplyRequest, _ *ReviewApplyResponse) error {
	s.writes.Add(1)
	return nil
}

func TestC110ReviewRPCLegacyRejectsBeforeDispatch(t *testing.T) {
	service := &legacyReviewService{}
	client := pipeInspectionClient(t, service)
	ctx := context.Background()
	if _, err := client.ReviewAnalyze(ctx, "project", "ollama", "fixture"); !errors.Is(err, ErrReviewUnsupported) {
		t.Fatalf("legacy analysis result = %v", err)
	}
	if err := client.ReviewApply(ctx, "project", "id", memory.ReviewAction{Action: "reject"}); !errors.Is(err, ErrReviewUnsupported) {
		t.Fatalf("legacy decision result = %v", err)
	}
	if service.analyzes.Load() != 0 || service.writes.Load() != 0 || client.ReviewJournalSupported() {
		t.Fatalf("legacy endpoint received unexpected review dispatch")
	}
}

func TestC110ReviewRPCTypedRoundTrip(t *testing.T) {
	dir, server := startServer(t, mustToken(t))
	client := dialT(t, dir)
	if !client.ReviewJournalSupported() {
		t.Fatal("journal capability missing")
	}
	ctx := context.Background()
	store := server.backend.(*memory.Store)
	a, err := store.PutReturningFact(ctx, "project", "Primary evidence")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.PutReturningFact(ctx, "project", "Conflicting evidence")
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ReviewCommit(ctx, memory.ReviewSubmission{
		Namespace: "project", Provider: "fixture", Model: "offline", PromptVersion: "fixture-v1",
		Snapshots: []memory.EvidenceSnapshot{memory.NewEvidenceSnapshot(a), memory.NewEvidenceSnapshot(b)},
		Findings: []memory.ReviewFinding{
			{Kind: memory.ReviewKindConflict, SourceIDs: []string{a.ID, b.ID}, Note: "Conflict fixture"},
		},
	})
	if err != nil || len(records) != 1 {
		t.Fatalf("seed journal = %v, %v", records, err)
	}
	id := records[0].ID
	rows, err := client.ReviewList(ctx, "project")
	if err != nil || len(rows) != 1 || rows[0].ID != id || len(rows[0].Sources) != 0 {
		t.Fatalf("list = %v, %v", rows, err)
	}
	if err := client.ReviewApply(ctx, "project", id, memory.ReviewAction{Action: "purge"}); !errors.Is(err, memory.ErrReviewTransition) {
		t.Fatalf("purge open conflict = %v", err)
	}
	if err := client.ReviewApply(ctx, "project", id, memory.ReviewAction{
		Action: "resolve", Decision: "Primary source governs.", References: []string{a.ID},
	}); err != nil {
		t.Fatal(err)
	}
	row, err := client.ReviewInspect(ctx, "project", id)
	if err != nil || row.Resolution != "resolved" || len(row.Sources) != 2 || row.Decision == "" {
		t.Fatalf("resolved record = %+v, %v", row, err)
	}
	if _, err := client.ReviewInspect(ctx, "different-project", id); !errors.Is(err, memory.ErrReviewNotFound) {
		t.Fatalf("foreign namespace inspect = %v", err)
	}
	stale := errors.New(memory.ErrFactChanged.Error() + ": source changed after request")
	if got := reviewRPCError(stale); !errors.Is(got, memory.ErrFactChanged) {
		t.Fatalf("typed stale source error = %v", got)
	}
}

func TestC110ReviewAnalyzeDeadlineAndContextIsolation(t *testing.T) {
	_, server := startServer(t, "")
	now := time.Now()
	meta := InspectionContext{
		RequestID: "0123456789abcdef0123456789abcdef", DeadlineUnixNano: now.Add(time.Hour).UnixNano(),
		ReviewAnalyze: true,
	}
	if _, _, err := server.beginInspection(meta); err == nil {
		t.Fatal("analysis context extended an ordinary inspection")
	}
	_, ctx, finish, err := server.beginReview(meta, true)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Sub(now) < 85*time.Second || deadline.Sub(now) > 95*time.Second {
		t.Fatalf("analysis deadline = %v, present=%v", deadline, ok)
	}
	if err := server.CancelInspection(&CancelInspectionRequest{Context: meta}, &CancelInspectionResponse{}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("analysis context was not canceled: %v", ctx.Err())
	}
}
