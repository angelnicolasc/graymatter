package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestConfidenceMCPEmptyResultsKeepSchemaAndTextPolicy(t *testing.T) {
	s, _ := confidenceFullTestServer(t)
	add := confidenceToolCall(t, s, "memory_add", map[string]any{"agent_id": "excluded", "text": "concealed unverified fact", "confidence": "unverified"})
	if add["isError"] == true {
		t.Fatalf("seed failed: %#v", add)
	}
	for _, namespace := range []string{"absent", "excluded"} {
		for _, mode := range []string{"plain", "explain", "batch"} {
			t.Run(namespace+"/"+mode, func(t *testing.T) {
				tool := "memory_search"
				args := map[string]any{"agent_id": namespace, "query": "concealed", "min_confidence": "verified", "confidence_weight": 0.0}
				if mode == "explain" {
					args["explain"] = true
				} else if mode == "batch" {
					tool = "memory_search_batch"
					delete(args, "query")
					args["queries"] = []string{"concealed", "fact"}
				}
				result := confidenceToolCall(t, s, tool, args)
				if result["isError"] == true {
					t.Fatalf("empty result failed: %#v", result)
				}
				payload := result["structuredContent"].(map[string]any)
				if payload["count"] != float64(0) {
					t.Fatalf("excluded fact leaked: %#v", payload)
				}
				metadata := payload["retrieval"].(map[string]any)
				if metadata["min_confidence"] != "verified" || metadata["confidence_weight"] != float64(0) || metadata["policy"] != "confidence-v1" || metadata["kg"] != "suppressed_min_confidence" {
					t.Fatalf("empty result lost effective policy: %#v", metadata)
				}
				validateStructuredAgainstToolSchema(t, tool, payload)
				wire, _ := json.Marshal(result["content"])
				for _, want := range []string{"No memories found", "Retrieval policy", "min_confidence=verified", "confidence_weight=0", "suppressed_min_confidence"} {
					if !strings.Contains(string(wire), want) {
						t.Errorf("empty %s text lost %q: %s", mode, want, wire)
					}
				}
			})
		}
	}
}

// Embedding the historical interface deliberately hides concrete optional
// capabilities. A rejected option must not fall through to legacy retrieval
// or writing, even when that backend could produce a plausible old response.
type confidenceUnsupportedBackend struct {
	Backend
	calls atomic.Int32
}

func (b *confidenceUnsupportedBackend) Remember(context.Context, string, string) error {
	b.calls.Add(1)
	return nil
}
func (b *confidenceUnsupportedBackend) Recall(context.Context, string, string, int) ([]string, error) {
	b.calls.Add(1)
	return []string{"legacy result"}, nil
}
func (b *confidenceUnsupportedBackend) RecallDetailed(context.Context, string, string, int) ([]string, string, error) {
	b.calls.Add(1)
	return []string{"legacy result"}, "", nil
}
func (b *confidenceUnsupportedBackend) RecallExplain(context.Context, string, string, int) ([]memory.RecallReceipt, error) {
	b.calls.Add(1)
	return nil, nil
}

func TestConfidenceMCPInvalidAndUnsupportedOptionsHaveNoEffects(t *testing.T) {
	_, mem := confidenceFullTestServer(t)
	if err := mem.Remember(context.Background(), "a", "original fact"); err != nil {
		t.Fatal(err)
	}
	backend := &confidenceUnsupportedBackend{Backend: NewDirectBackend(mem, nil)}
	s := New(backend, "test", WithFullCatalog())
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"memory_add", map[string]any{"agent_id": "a", "text": "new fact", "confidence": ""}},
		{"memory_search", map[string]any{"agent_id": "a", "query": "fact", "confidence_weight": 0.6}},
		{"memory_search_batch", map[string]any{"agent_id": "a", "queries": []string{"fact"}, "min_confidence": nil}},
		{"memory_add", map[string]any{"agent_id": "a", "text": "new fact", "confidence": "verified"}},
		{"memory_reflect", map[string]any{"agent_id": "a", "action": "add", "text": "new fact", "confidence": "verified"}},
		{"memory_reflect", map[string]any{"agent_id": "a", "action": "update", "target": "original fact", "text": "new fact", "confidence": "verified"}},
		{"memory_search", map[string]any{"agent_id": "a", "query": "fact", "confidence_weight": 0.0}},
		{"memory_search", map[string]any{"agent_id": "a", "query": "fact", "min_confidence": "verified", "explain": true}},
		{"memory_search_batch", map[string]any{"agent_id": "a", "queries": []string{"fact", "original"}, "min_confidence": "verified"}},
	} {
		result := confidenceToolCall(t, s, call.tool, call.args)
		if result["isError"] != true || result["structuredContent"] != nil {
			t.Fatalf("rejected option became success: %#v", result)
		}
	}
	if backend.calls.Load() != 0 {
		t.Fatalf("rejected options reached legacy backend: %d calls", backend.calls.Load())
	}
	facts, err := mem.Advanced().List("a")
	if err != nil || len(facts) != 1 || facts[0].Text != "original fact" || facts[0].AccessCount != 0 || facts[0].IsSuperseded() {
		t.Fatalf("rejected options changed canonical state: %+v / %v", facts, err)
	}
}

type confidenceFailedRecallBackend struct {
	Backend
	calls atomic.Int32
}

func (b *confidenceFailedRecallBackend) RecallWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallResult, error) {
	b.calls.Add(1)
	return memory.RecallResult{}, errors.New("explicit backend failure")
}
func (*confidenceFailedRecallBackend) RecallExplainWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallExplainResult, error) {
	return memory.RecallExplainResult{}, errors.New("explicit backend failure")
}

func TestConfidenceMCPAllFailedBatchIsAnError(t *testing.T) {
	_, mem := confidenceFullTestServer(t)
	backend := &confidenceFailedRecallBackend{Backend: NewDirectBackend(mem, nil)}
	s := New(backend, "test", WithFullCatalog())
	result := confidenceToolCall(t, s, "memory_search_batch", map[string]any{"agent_id": "a", "queries": []string{"first", "second"}, "confidence_weight": 0.2})
	wire, _ := json.Marshal(result)
	if result["isError"] != true || result["structuredContent"] != nil || backend.calls.Load() != 2 || !strings.Contains(string(wire), "All recall queries failed") || !strings.Contains(string(wire), "explicit backend failure") {
		t.Fatalf("all-failed batch disguised as empty success: %s; calls=%d", wire, backend.calls.Load())
	}
}

type confidenceFlappingDefaultBackend struct {
	Backend
	recaller confidenceRecaller
	defaults atomic.Int32
	ready    atomic.Int32
	weights  chan float64
}

func (b *confidenceFlappingDefaultBackend) DefaultConfidenceWeight() float64 {
	if b.defaults.Add(1) == 1 {
		return .1
	}
	return .4
}
func (b *confidenceFlappingDefaultBackend) Ready() error {
	b.ready.Add(1)
	return nil
}
func (b *confidenceFlappingDefaultBackend) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if opts.ConfidenceWeight == nil {
		return memory.RecallResult{}, errors.New("batch did not bind an effective weight")
	}
	b.weights <- *opts.ConfidenceWeight
	return b.recaller.RecallWithOptions(ctx, agentID, query, topK, opts)
}
func (b *confidenceFlappingDefaultBackend) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	return b.recaller.RecallExplainWithOptions(ctx, agentID, query, topK, opts)
}

func TestConfidenceMCPBatchResolvesConfiguredPolicyOnce(t *testing.T) {
	_, mem := confidenceFullTestServerWithWeight(t, .2)
	verified := "verified"
	if _, err := mem.RememberWithOptions(context.Background(), "a", "policy archive routing", memory.WriteOptions{Confidence: &verified}); err != nil {
		t.Fatal(err)
	}
	for _, filtered := range []bool{false, true} {
		name := "default"
		if filtered {
			name = "filter"
		}
		t.Run(name, func(t *testing.T) {
			direct := NewDirectBackend(mem, nil)
			backend := &confidenceFlappingDefaultBackend{Backend: direct, recaller: direct, weights: make(chan float64, 3)}
			s := New(backend, "test", WithFullCatalog())
			args := map[string]any{"agent_id": "a", "queries": []string{"policy", "archive", "routing"}}
			if filtered {
				args["min_confidence"] = verified
			}
			result := confidenceToolCall(t, s, "memory_search_batch", args)
			if result["isError"] == true {
				t.Fatalf("batch failed: %#v", result)
			}
			payload := result["structuredContent"].(map[string]any)
			if metadata := payload["retrieval"].(map[string]any); metadata["confidence_weight"] != .1 {
				t.Fatalf("global receipt did not use bound policy: %#v", metadata)
			}
			validateStructuredAgainstToolSchema(t, "memory_search_batch", payload)
			if backend.defaults.Load() != 1 || backend.ready.Load() != 1 || len(backend.weights) != 3 {
				t.Fatalf("batch resolved defaults per query: default=%d ready=%d queries=%d", backend.defaults.Load(), backend.ready.Load(), len(backend.weights))
			}
			for i := 0; i < 3; i++ {
				if weight := <-backend.weights; weight != .1 {
					t.Fatalf("query used a different policy from its batch: %g", weight)
				}
			}
			wire, _ := json.Marshal(result["content"])
			if !strings.Contains(string(wire), "confidence_weight=0.1") {
				t.Fatalf("batch text did not show bound effective policy: %s", wire)
			}
		})
	}
}
