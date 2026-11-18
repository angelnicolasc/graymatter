package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func confidenceTestServer(t *testing.T) (*Server, *graymatter.Memory) {
	return confidenceTestServerWithWeight(t, 0)
}

func confidenceTestServerWithWeight(t *testing.T, weight float64) (*Server, *graymatter.Memory) {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = false
	cfg.ConfidenceWeight = weight
	cfg.OpenAIAPIKey, cfg.VoyageAPIKey, cfg.AnthropicAPIKey = "", "", ""
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return New(NewDirectBackend(mem, nil), "test"), mem
}

func TestConfidenceMCPConfiguredDefaultAndExplicitZero(t *testing.T) {
	s, _ := confidenceTestServerWithWeight(t, 0.2)
	for _, label := range []string{"verified", "unverified"} {
		result := confidenceToolCall(t, s, "memory_add", map[string]any{"agent_id": "a", "text": "policy " + label, "confidence": label})
		if result["isError"] == true {
			t.Fatalf("add: %#v", result)
		}
	}
	for _, explain := range []bool{false, true} {
		result := confidenceToolCall(t, s, "memory_search", map[string]any{"agent_id": "a", "query": "policy", "explain": explain})
		payload := result["structuredContent"].(map[string]any)
		metadata := payload["retrieval"].(map[string]any)
		if metadata["confidence_weight"] != 0.2 {
			t.Fatalf("configured default omitted from receipt: %#v", payload)
		}
		if explain && payload["explained"].([]any)[0].(map[string]any)["ranking"] == nil {
			t.Fatalf("configured default has no ranking receipt: %#v", payload)
		}
	}
	batch := confidenceToolCall(t, s, "memory_search_batch", map[string]any{"agent_id": "a", "queries": []string{"policy", "verified"}})
	if metadata := batch["structuredContent"].(map[string]any)["retrieval"].(map[string]any); metadata["confidence_weight"] != 0.2 {
		t.Fatalf("batch lost configured default: %#v", batch)
	}
	result := confidenceToolCall(t, s, "memory_search", map[string]any{"agent_id": "a", "query": "policy", "confidence_weight": 0.0, "min_confidence": "verified"})
	payload := result["structuredContent"].(map[string]any)
	if payload["count"] != float64(1) || payload["retrieval"].(map[string]any)["confidence_weight"] != float64(0) {
		t.Fatalf("explicit zero lost filter or opt-out: %#v", payload)
	}
}

type confidenceLateFailureBackend struct {
	Backend
	writer  confidenceWriter
	reviser confidenceReviser
	last    memory.Fact
}

func (b *confidenceLateFailureBackend) PutWithOptionsReturningFact(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	fact, err := b.writer.PutWithOptionsReturningFact(ctx, agentID, text, opts)
	b.last = fact
	if err != nil {
		return fact, err
	}
	return fact, errors.New("required post-commit phase failed")
}

func (b *confidenceLateFailureBackend) ReviseFactsWithOptions(ctx context.Context, agentID, text string, opts memory.WriteOptions, victims ...memory.Fact) (memory.Fact, error) {
	fact, err := b.reviser.ReviseFactsWithOptions(ctx, agentID, text, opts, victims...)
	b.last = fact
	if err != nil {
		return fact, err
	}
	return fact, errors.New("backend follow-up failed")
}

func TestConfidenceMCPLateFailureReportsKnownCommittedIdentity(t *testing.T) {
	_, mem := confidenceTestServer(t)
	direct := NewDirectBackend(mem, nil)
	backend := &confidenceLateFailureBackend{Backend: direct, writer: direct, reviser: direct}
	s := New(backend, "test")
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"memory_add", map[string]any{"agent_id": "a", "text": "original policy", "confidence": "unverified"}},
		{"memory_reflect", map[string]any{"agent_id": "a", "action": "add", "text": "other policy", "confidence": "verified"}},
		{"memory_reflect", map[string]any{"agent_id": "a", "action": "update", "target": "original policy", "text": "corrected policy"}},
	} {
		result := confidenceToolCall(t, s, call.tool, call.args)
		wire, _ := json.Marshal(result)
		committed := "after committing"
		if call.args["action"] == "update" {
			committed = "Update committed as"
		}
		if result["isError"] != true || backend.last.ID == "" || !strings.Contains(string(wire), backend.last.ID) || !strings.Contains(string(wire), committed) {
			t.Fatalf("late failure hid durable identity or reported success: %s / %+v", wire, backend.last)
		}
	}
}

func confidenceToolCall(t *testing.T, s *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), request))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil || envelope.Error != nil {
		t.Fatalf("protocol error: %s / %v", response, err)
	}
	return envelope.Result
}

func TestConfidenceMCPRuntimeValidationHasNoEffects(t *testing.T) {
	s, mem := confidenceTestServer(t)
	for _, value := range []any{nil, "", " verified", "Verified", "unknown", 1, true, []any{"verified"}, map[string]any{}} {
		for _, tool := range []string{"memory_add", "memory_reflect"} {
			args := map[string]any{"agent_id": "a", "text": "must not persist", "confidence": value}
			if tool == "memory_reflect" {
				args["action"] = "add"
			}
			if result := confidenceToolCall(t, s, tool, args); result["isError"] != true {
				t.Fatalf("%s accepted confidence %#v: %#v", tool, value, result)
			}
		}
	}
	for _, value := range []any{nil, "", "verified ", true, 2, []any{}} {
		for _, tool := range []string{"memory_search", "memory_search_batch"} {
			args := map[string]any{"agent_id": "a", "query": "test", "queries": []any{"test"}, "min_confidence": value}
			if result := confidenceToolCall(t, s, tool, args); result["isError"] != true {
				t.Fatalf("%s accepted minimum %#v", tool, value)
			}
		}
	}
	for _, value := range []any{nil, "0.1", true, -0.01, 0.51, []any{}} {
		if result := confidenceToolCall(t, s, "memory_search", map[string]any{"agent_id": "a", "query": "test", "confidence_weight": value}); result["isError"] != true {
			t.Fatalf("accepted weight %#v", value)
		}
	}
	for _, weight := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		result, _ := s.handleMemorySearch(context.Background(), reflectReq(map[string]any{"agent_id": "a", "query": "test", "confidence_weight": weight}))
		if !result.IsError {
			t.Fatalf("accepted non-finite weight %v", weight)
		}
	}
	for _, action := range []string{"pin", "unpin", "forget", "link"} {
		if result := confidenceToolCall(t, s, "memory_reflect", map[string]any{"agent_id": "a", "action": action, "text": "test", "target": "test", "confidence": "verified"}); result["isError"] != true {
			t.Fatalf("accepted confidence for %s", action)
		}
	}
	for _, tool := range []string{"memory_alias", "checkpoint_save", "checkpoint_resume", "memory_add"} {
		args := map[string]any{"agent_id": "a", "text": "test", "term": "x", "equivalents": []string{"y"}, "confidence_weight": 0.0}
		if result := confidenceToolCall(t, s, tool, args); result["isError"] != true {
			t.Fatalf("wrong tool %s silently ignored option", tool)
		}
	}
	facts, err := mem.Advanced().List("a")
	if err != nil || len(facts) != 0 {
		t.Fatalf("invalid calls changed store: %+v / %v", facts, err)
	}
}

func TestConfidenceMCPWritesFiltersReceiptsAndBatchSchemas(t *testing.T) {
	s, mem := confidenceTestServer(t)
	for _, label := range []string{"verified", "inferred", "unverified"} {
		out := confidenceToolCall(t, s, "memory_add", map[string]any{"agent_id": "a", "text": "policy " + label, "confidence": label})
		if out["isError"] == true {
			t.Fatalf("write: %#v", out)
		}
		payload := out["structuredContent"].(map[string]any)
		if payload["fact_id"] == "" || payload["confidence"] != label {
			t.Fatalf("write receipt: %#v", payload)
		}
	}
	for _, explain := range []bool{false, true} {
		out := confidenceToolCall(t, s, "memory_search", map[string]any{"agent_id": "a", "query": "policy", "min_confidence": "verified", "confidence_weight": 0.2, "explain": explain})
		if out["isError"] == true {
			t.Fatalf("search: %#v", out)
		}
		payload := out["structuredContent"].(map[string]any)
		if payload["count"] != float64(1) || payload["retrieval"] == nil {
			t.Fatalf("filtered payload: %#v", payload)
		}
		if explain {
			receipt := payload["explained"].([]any)[0].(map[string]any)
			if receipt["ranking"] == nil {
				t.Fatalf("missing ranking: %#v", receipt)
			}
		}
		wire, _ := json.Marshal(out)
		if !strings.Contains(string(wire), "suppressed_min_confidence") || !strings.Contains(string(wire), "Retrieval policy") {
			t.Fatalf("text/JSON policy mismatch: %s", wire)
		}
		validateStructuredAgainstToolSchema(t, "memory_search", payload)
	}
	out := confidenceToolCall(t, s, "memory_search_batch", map[string]any{"agent_id": "a", "queries": []any{"policy", "other vocabulary"}, "min_confidence": "verified", "confidence_weight": 0.0})
	payload := out["structuredContent"].(map[string]any)
	if payload["retrieval"] == nil || len(payload["merged"].([]any)) != 1 {
		t.Fatalf("batch lost options: %#v", payload)
	}
	validateStructuredAgainstToolSchema(t, "memory_search_batch", payload)
	out = confidenceToolCall(t, s, "memory_reflect", map[string]any{"agent_id": "a", "action": "update", "target": "policy unverified", "text": "corrected policy"})
	if out["isError"] == true {
		t.Fatalf("revision: %#v", out)
	}
	facts, _ := mem.Advanced().List("a")
	for _, f := range facts {
		if f.Text == "corrected policy" && f.Confidence != "unverified" {
			t.Fatalf("revision promoted confidence: %+v", f)
		}
	}
	out = confidenceToolCall(t, s, "memory_search", map[string]any{"agent_id": "empty", "query": "policy", "min_confidence": "verified", "confidence_weight": 0.0, "explain": true})
	payload = out["structuredContent"].(map[string]any)
	if payload["count"] != float64(0) || payload["retrieval"] == nil {
		t.Fatalf("empty metadata missing: %#v", payload)
	}
	validateStructuredAgainstToolSchema(t, "memory_search", payload)
}
