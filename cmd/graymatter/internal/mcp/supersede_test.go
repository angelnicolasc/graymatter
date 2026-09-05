package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// memory_reflect's update and forget actions used to set Weight = 0 and report
// success. Recall does not read Weight, so the fact kept being returned and
// the agent that had just corrected itself was handed both versions on the
// next search. These tests drive the tools the way an agent does — call, then
// search — because the defect was invisible from inside the handler: it wrote
// exactly what it meant to write.

const (
	staleFact       = "Billing runs through Lemon Squeezy"
	freshFact       = "Billing runs through Polar"
	interleavedFact = "The deployment window is Thursday at 03:00 UTC"
)

type reflectInterleavingBackend struct {
	Backend
	armed         bool
	replacementID string
	interloperID  string
}

func (b *reflectInterleavingBackend) Remember(ctx context.Context, agentID, text string) error {
	if err := b.Backend.Remember(ctx, agentID, text); err != nil {
		return err
	}
	if !b.armed {
		return nil
	}
	b.replacementID = b.newestID(agentID, text)
	return b.interleave(ctx, agentID)
}

// Keep this optional so the same test compiles against the pre-fix Backend:
// that handler calls Remember above, while the fixed handler takes this path.
func (b *reflectInterleavingBackend) PutReturningFact(ctx context.Context, agentID, text string) (memory.Fact, error) {
	writer, ok := b.Backend.(interface {
		PutReturningFact(context.Context, string, string) (memory.Fact, error)
	})
	if !ok {
		return memory.Fact{}, errors.New("backend does not expose PutReturningFact")
	}
	fact, err := writer.PutReturningFact(ctx, agentID, text)
	if err != nil {
		return memory.Fact{}, err
	}
	if b.armed {
		b.replacementID = fact.ID
		err = b.interleave(ctx, agentID)
	}
	return fact, err
}

func (b *reflectInterleavingBackend) interleave(ctx context.Context, agentID string) error {
	b.armed = false
	if err := b.Backend.Remember(ctx, agentID, interleavedFact); err != nil {
		return err
	}
	b.interloperID = b.newestID(agentID, interleavedFact)
	return nil
}

func (b *reflectInterleavingBackend) newestID(agentID, text string) string {
	facts, err := b.Backend.List(agentID)
	if err != nil {
		return ""
	}
	for _, fact := range facts {
		if fact.Text == text {
			return fact.ID
		}
	}
	return ""
}

func newDaemonReflectBackend(t *testing.T) Backend {
	t.Helper()
	dataDir := t.TempDir()

	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(daemon.RunOptions{
			DataDir: dataDir,
			Logf: func(format string, _ ...any) {
				if strings.HasPrefix(format, "graymatter daemon ready:") {
					close(ready)
				}
			},
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not become ready")
	}

	client, err := daemon.ConnectNoSpawn(dataDir)
	if err != nil {
		t.Fatalf("connect daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Shutdown()
		_ = client.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	return client
}

func TestMemoryReflect_UpdateKeepsItsReplacementIdentity(t *testing.T) {
	// Keep both paths provider-free; the invalid scheme prevents an Ollama dial.
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	tests := []struct {
		name string
		open func(*testing.T) Backend
	}{
		{
			name: "direct",
			open: func(t *testing.T) Backend {
				s, _ := newTestServer(t)
				return s.backend
			},
		},
		{name: "daemon_rpc", open: newDaemonReflectBackend},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := tc.open(t)
			const agentID = "concurrent-reflect"
			const oldText = "The deployment window is Wednesday at 03:00 UTC"
			const correctedText = "The deployment window is Tuesday at 03:00 UTC"

			// Arm only after seeding: otherwise the interleaving is consumed by
			// setup and the test does not exercise the vulnerable window.
			if err := base.Remember(ctx, agentID, oldText); err != nil {
				t.Fatalf("seed: %v", err)
			}
			interleaved := &reflectInterleavingBackend{Backend: base, armed: true}
			s := New(interleaved, "test")

			res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
				"action": "update", "agent_id": agentID,
				"target": oldText, "text": correctedText,
			}))
			if err != nil || res.IsError {
				t.Fatalf("memory_reflect update: %v / %s", err, resultText(t, res))
			}
			if interleaved.replacementID == "" || interleaved.interloperID == "" {
				t.Fatalf("interleaving IDs = replacement %q, interloper %q; want both", interleaved.replacementID, interleaved.interloperID)
			}
			if interleaved.replacementID == interleaved.interloperID {
				t.Fatal("replacement and interloper reused one identity")
			}

			facts, err := base.List(agentID)
			if err != nil {
				t.Fatalf("list facts: %v", err)
			}
			byID := make(map[string]memory.Fact, len(facts))
			for _, fact := range facts {
				byID[fact.ID] = fact
			}
			// This guard proves the ID called "replacement" belongs to this
			// invocation's corrected text before testing the tombstone.
			if got := byID[interleaved.replacementID].Text; got != correctedText {
				t.Fatalf("replacement ID carries %q, want corrected text %q", got, correctedText)
			}
			if got := byID[interleaved.interloperID].Text; got != interleavedFact {
				t.Fatalf("interloper ID carries %q, want %q", got, interleavedFact)
			}

			var victim memory.Fact
			for _, fact := range facts {
				if fact.Text == oldText {
					victim = fact
					break
				}
			}
			if victim.ID == "" {
				t.Fatal("seeded victim not found")
			}
			if victim.SupersededBy != interleaved.replacementID {
				t.Fatalf("victim SupersededBy=%q, want this call's replacement %q (interloper %q)",
					victim.SupersededBy, interleaved.replacementID, interleaved.interloperID)
			}
		})
	}
}

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
