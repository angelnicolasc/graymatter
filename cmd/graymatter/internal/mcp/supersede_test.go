package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// memory_reflect's update and forget actions used to set Weight = 0 and report
// success. Recall does not read Weight, so the fact kept being returned and
// the agent that had just corrected itself was handed both versions on the
// next search. These tests drive the tools the way an agent does — call, then
// search — because the defect was invisible from inside the handler: it wrote
// exactly what it meant to write.

const (
	staleFact = "Billing runs through Lemon Squeezy"
	freshFact = "Billing runs through Polar"
)

// TestMemoryReflect_UpdateRemovesOldFactFromSearch is the regression test for
// the correction case: after an update, the superseded statement must not come
// back, and the correction must.
func TestMemoryReflect_UpdateRemovesOldFactFromSearch(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	mustAdd(t, s, "billing-agent", staleFact)

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update",
		"agent":  "billing-agent",
		"target": staleFact,
		"text":   freshFact,
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_reflect update: %v / %s", err, resultText(t, res))
	}

	found := search(t, s, "billing-agent", "billing provider")
	if strings.Contains(found, "Lemon Squeezy") {
		t.Errorf("memory_reflect reported the fact updated, then search returned the old one:\n%s", found)
	}
	if !strings.Contains(found, "Polar") {
		t.Errorf("the corrected fact is missing from search results:\n%s", found)
	}
}

// TestMemoryReflect_ForgetRemovesFactFromSearch is the same for the forget
// case, where there is no replacement — the tool answers "Fact suppressed",
// and that has to be true.
func TestMemoryReflect_ForgetRemovesFactFromSearch(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	mustAdd(t, s, "forget-agent", staleFact)

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "forget-agent",
		"target": staleFact,
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_reflect forget: %v / %s", err, resultText(t, res))
	}

	if found := search(t, s, "forget-agent", "billing provider"); strings.Contains(found, "Lemon Squeezy") {
		t.Errorf("memory_reflect reported the fact suppressed, then search returned it:\n%s", found)
	}
}

// TestMemoryReflect_SupersededFactSurvivesInStore holds the append-only
// promise at the tool boundary: suppressed means invisible to retrieval, not
// erased. An audit still has to be able to see what the agent retired and
// what replaced it.
func TestMemoryReflect_SupersededFactSurvivesInStore(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()

	mustAdd(t, s, "audit-agent", staleFact)

	if res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update",
		"agent":  "audit-agent",
		"target": staleFact,
		"text":   freshFact,
	})); err != nil || res.IsError {
		t.Fatalf("memory_reflect update: %v / %s", err, resultText(t, res))
	}

	facts, err := mem.Advanced().List("audit-agent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var tombstoned, live *memory.Fact
	for i := range facts {
		switch {
		case facts[i].Text == staleFact:
			tombstoned = &facts[i]
		case facts[i].Text == freshFact:
			live = &facts[i]
		}
	}
	if tombstoned == nil {
		t.Fatal("the superseded fact was deleted; storage is documented as append-only")
	}
	if !tombstoned.IsSuperseded() {
		t.Error("the superseded fact carries no tombstone, so recall has nothing to filter on")
	}
	if live == nil {
		t.Fatal("the replacement fact was not stored")
	}
	// The tombstone points at the replacement, so an audit can follow the
	// correction rather than just seeing that something was retired.
	if tombstoned.SupersededBy != live.ID {
		t.Errorf("SupersededBy = %q, want the replacement fact's ID %q",
			tombstoned.SupersededBy, live.ID)
	}
}

// TestMemoryReflect_ForgetMarksAgentDecision distinguishes the two ways a fact
// dies. forget has no replacement to point at, so it records that an agent
// made the call.
func TestMemoryReflect_ForgetMarksAgentDecision(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()

	mustAdd(t, s, "marker-agent", staleFact)

	if res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "marker-agent",
		"target": staleFact,
	})); err != nil || res.IsError {
		t.Fatalf("memory_reflect forget: %v / %s", err, resultText(t, res))
	}

	facts, err := mem.Advanced().List("marker-agent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected the forgotten fact to remain in the store, got %d", len(facts))
	}
	if facts[0].SupersededBy != memory.SupersededByAgent {
		t.Errorf("SupersededBy = %q, want %q", facts[0].SupersededBy, memory.SupersededByAgent)
	}
}

// --- helpers ---

func mustAdd(t *testing.T, s *Server, agentID, text string) {
	t.Helper()
	res, err := s.handleMemoryAdd(context.Background(), reflectReq(map[string]any{
		"agent_id": agentID, "text": text,
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_add %q: %v / %s", text, err, resultText(t, res))
	}
}

func search(t *testing.T, s *Server, agentID, query string) string {
	t.Helper()
	res, err := s.handleMemorySearch(context.Background(), reflectReq(map[string]any{
		"agent_id": agentID, "query": query, "top_k": float64(8),
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_search: %v / %s", err, resultText(t, res))
	}
	return resultText(t, res)
}
