package rpc

import (
	"context"
	"errors"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestScopedBatchRPCLegacyNegotiation(t *testing.T) {
	dir, svc := startLegacyProtocol(t)
	c := dialT(t, dir)
	_, err := c.BatchRecallScoped(context.Background(), "a", []string{"one", "two"}, 5, false, nil)
	if !errors.Is(err, memory.ErrBatchRecallUnsupported) || svc.calls.Load() != 0 || svc.pings.Load() != 1 {
		t.Fatalf("batch negotiation err=%v scalar calls=%d pings=%d", err, svc.calls.Load(), svc.pings.Load())
	}
}

type scopedBatchErrorBackend struct{ Backend }

func (scopedBatchErrorBackend) BatchRecallScoped(_ context.Context, agentID string, queries []string, topK int, all bool, opts *memory.RecallOptions) ([]memory.BatchResult, error) {
	if agentID != "a" || len(queries) != 2 || topK != 3 || !all || opts == nil || opts.ConfidenceWeight == nil || *opts.ConfidenceWeight != 0 {
		return nil, errors.New("batch request fields changed")
	}
	return []memory.BatchResult{
		{Query: queries[0], Facts: []string{"answer"}},
		{Query: queries[1], Err: errors.New("query failed")},
	}, memory.ErrBatchRecallUnsupported
}

func TestScopedBatchRPCPreservesRowsAndDispatchedErrors(t *testing.T) {
	c := confidenceClientForBackend(t, scopedBatchErrorBackend{})
	zero := 0.0
	rows, err := c.BatchRecallScoped(context.Background(), "a", []string{"one", "two"}, 3, true, &memory.RecallOptions{ConfidenceWeight: &zero})
	if err == nil || err.Error() != memory.ErrBatchRecallUnsupported.Error() || errors.Is(err, memory.ErrBatchRecallUnsupported) {
		t.Fatalf("dispatched error was lost or matched the fallback sentinel: %v", err)
	}
	if len(rows) != 2 || rows[0].Query != "one" || len(rows[0].Facts) != 1 || rows[0].Facts[0] != "answer" ||
		rows[1].Query != "two" || rows[1].Err == nil || rows[1].Err.Error() != "query failed" {
		t.Fatalf("batch response lost results or errors: %+v", rows)
	}
}

type scopedBatchLegacyPolicyBackend struct {
	Backend
	ConfidenceRecaller
}

func (scopedBatchLegacyPolicyBackend) BatchRecallScoped(_ context.Context, _ string, queries []string, topK int, _ bool, opts *memory.RecallOptions) ([]memory.BatchResult, error) {
	if topK != 0 || opts == nil || opts.ConfidenceWeight == nil || *opts.ConfidenceWeight != 0 {
		return nil, errors.New("expected zero policy with unmodified product top-k")
	}
	return []memory.BatchResult{{
		Query:     queries[0],
		Retrieval: &memory.RetrievalMetadata{ConfidenceWeight: *opts.ConfidenceWeight},
	}}, nil
}

func TestScopedBatchRPCLegacyPolicyAndWireMetadata(t *testing.T) {
	c := confidenceClientForBackend(t, scopedBatchLegacyPolicyBackend{})
	queries := []string{"policy"}
	zero := 0.0
	for _, opts := range []*memory.RecallOptions{nil, {ConfidenceWeight: &zero}} {
		rows, err := c.BatchRecallScoped(context.Background(), "a", queries, 0, false, opts)
		if err != nil || len(rows) != 1 || (rows[0].Retrieval != nil) != (opts != nil) {
			t.Fatalf("opts=%+v rows=%+v err=%v", opts, rows, err)
		}
	}
}

func TestScopedBatchRPCRawLegacyTopKStaysEmpty(t *testing.T) {
	dir, _ := startServer(t, "")
	c := dialT(t, dir)
	ctx := context.Background()
	if err := c.Put(ctx, "a", "database policy"); err != nil {
		t.Fatal(err)
	}
	for _, topK := range []int{0, -1} {
		rows, err := c.BatchRecallScoped(ctx, "a", []string{"policy"}, topK, false, nil)
		if err != nil || len(rows) != 1 || len(rows[0].Facts) != 0 || rows[0].Retrieval != nil {
			t.Fatalf("topK=%d rows=%+v err=%v", topK, rows, err)
		}
	}
}
