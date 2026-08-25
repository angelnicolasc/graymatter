package mcp

// W1 (ADR-010): pin/unpin via memory_reflect. A pinned fact is exempt from
// decay, pruning and summarisation; pinning a superseded fact is rejected.

import (
	"context"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestMemoryReflect_PinUnpin(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	const text = "Security policy: API keys never live in config files."

	if res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "add", "agent": "sec", "text": text,
	})); err != nil || res.IsError {
		t.Fatalf("add: %v / %s", err, resultText(t, res))
	}

	// Pin.
	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "pin", "agent": "sec", "text": text,
	}))
	if err != nil || res.IsError {
		t.Fatalf("pin: %v / %s", err, resultText(t, res))
	}
	facts, err := s.backend.List("sec")
	if err != nil || len(facts) != 1 {
		t.Fatalf("list after pin: %v / %d facts", err, len(facts))
	}
	if !facts[0].Pinned || facts[0].PinnedAt.IsZero() {
		t.Errorf("fact not pinned: %+v", facts[0])
	}

	// Pinning a superseded fact is rejected.
	if err := s.supersedeFact("sec", facts[0], memory.SupersededByAgent); err != nil {
		t.Fatal(err)
	}
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "pin", "agent": "sec", "target": text,
	}))
	if err != nil || !res.IsError {
		t.Errorf("pinning a superseded fact must fail; got err=%v / %s", err, resultText(t, res))
	}

	// Unpin by target (the target-or-text convention, target wins).
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "unpin", "agent": "sec", "target": text,
	}))
	if err != nil || res.IsError {
		t.Fatalf("unpin: %v / %s", err, resultText(t, res))
	}
	after, _ := s.backend.List("sec")
	for _, f := range after {
		if f.Text == text && (f.Pinned || !f.PinnedAt.IsZero()) {
			t.Errorf("fact still pinned after unpin: %+v", f)
		}
	}
	if !strings.Contains(resultText(t, res), "unpin") {
		t.Errorf("unpin confirmation unexpected: %s", resultText(t, res))
	}
}

func TestMemoryReflect_PinRequiresFact(t *testing.T) {
	s, _ := newTestServer(t)
	res, err := s.handleMemoryReflect(context.Background(), reflectReq(map[string]any{
		"action": "pin", "agent": "sec",
	}))
	if err != nil || !res.IsError {
		t.Fatalf("pin without a fact must be a tool error; got %v / %s", err, resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "required") {
		t.Errorf("error should say the fact is required: %s", resultText(t, res))
	}
}
