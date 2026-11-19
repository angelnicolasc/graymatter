package mcp

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type scopedBatchMCPBackend struct {
	Backend
	calls   int
	failure error
}

func (b *scopedBatchMCPBackend) Recall(context.Context, string, string, int) ([]string, error) {
	return nil, errors.New("unexpected legacy recall")
}

func (b *scopedBatchMCPBackend) BatchRecallScoped(_ context.Context, agentID string, queries []string, topK int, all bool, opts *memory.RecallOptions) ([]memory.BatchResult, error) {
	b.calls++
	if b.failure != nil {
		return nil, b.failure
	}
	if agentID != "a" || len(queries) != 2 || topK != 1 || all || opts == nil || opts.ConfidenceWeight == nil || *opts.ConfidenceWeight != .2 {
		return nil, errors.New("batch request fields changed")
	}
	return []memory.BatchResult{
		{Query: queries[0], Facts: []string{"first", "second"}},
		{Query: queries[1], Err: errors.New("second query failed")},
	}, nil
}

func TestScopedBatchMCPTruncatesAndRetainsPerQueryErrors(t *testing.T) {
	backend := &scopedBatchMCPBackend{}
	s := New(backend, "test", WithFullCatalog())
	result := confidenceToolCall(t, s, "memory_search_batch", map[string]any{
		"agent_id": "a", "queries": []string{"one", "two"}, "top_k": 1, "confidence_weight": .2,
	})
	payload, ok := result["structuredContent"].(map[string]any)
	if !ok || result["isError"] == true || backend.calls != 1 {
		t.Fatalf("expected one successful batch dispatch: %+v, calls=%d", result, backend.calls)
	}
	validateStructuredAgainstToolSchema(t, "memory_search_batch", payload)
	if merged := payload["merged"].([]any); !slices.Equal(merged, []any{"first"}) {
		t.Fatalf("unexpected merged results: %+v", merged)
	}
	rows := payload["per_query"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["query"] != "one" || rows[1].(map[string]any)["error"] != "second query failed" {
		t.Fatalf("batch lost results or errors: %+v", rows)
	}
}

func TestScopedBatchMCPTransportFailureKeepsAllFailedFormat(t *testing.T) {
	backend := &scopedBatchMCPBackend{failure: io.EOF}
	result := confidenceToolCall(t, New(backend, "test", WithFullCatalog()), "memory_search_batch", map[string]any{
		"agent_id": "a", "queries": []string{"one", "two"},
	})
	if result["isError"] != true || result["structuredContent"] != nil || backend.calls != 1 {
		t.Fatalf("expected one failed batch dispatch: %+v, calls=%d", result, backend.calls)
	}
	content := result["content"].([]any)
	const want = `All recall queries failed: "one": EOF; "two": EOF`
	if len(content) != 1 || content[0].(map[string]any)["text"] != want {
		t.Fatalf("batch lost all-query failure text: %+v", content)
	}
}
