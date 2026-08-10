package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// recordingStore captures the method name and arguments of every call.
type recordingStore struct {
	method string
	args   []any
}

func (r *recordingStore) rec(name string, args ...any) { r.method, r.args = name, args }

func (r *recordingStore) Remember(_ context.Context, agentID, text string) error {
	r.rec("Remember", agentID, text)
	return nil
}
func (r *recordingStore) PutShared(_ context.Context, text string) error {
	r.rec("PutShared", text)
	return nil
}
func (r *recordingStore) RecallDefault(_ context.Context, agentID, query string) ([]string, error) {
	r.rec("RecallDefault", agentID, query)
	return []string{"rd"}, nil
}
func (r *recordingStore) Recall(_ context.Context, agentID, query string, topK int) ([]string, error) {
	r.rec("Recall", agentID, query, topK)
	return []string{"r"}, nil
}
func (r *recordingStore) RecallShared(_ context.Context, query string, topK int) ([]string, error) {
	r.rec("RecallShared", query, topK)
	return []string{"rs"}, nil
}
func (r *recordingStore) RecallAll(_ context.Context, agentID, query string, topK int) ([]string, error) {
	r.rec("RecallAll", agentID, query, topK)
	return []string{"ra"}, nil
}
func (r *recordingStore) List(agentID string) ([]memory.Fact, error) {
	r.rec("List", agentID)
	return []memory.Fact{{ID: "f1"}}, nil
}
func (r *recordingStore) ListAgents() ([]string, error) {
	r.rec("ListAgents")
	return []string{"a1"}, nil
}
func (r *recordingStore) Stats(agentID string) (memory.MemoryStats, error) {
	r.rec("Stats", agentID)
	return memory.MemoryStats{FactCount: 7}, nil
}
func (r *recordingStore) Delete(agentID, factID string) error {
	r.rec("Delete", agentID, factID)
	return nil
}
func (r *recordingStore) UpdateFact(agentID string, f memory.Fact) error {
	r.rec("UpdateFact", agentID, f.ID)
	return nil
}
func (r *recordingStore) Consolidate(_ context.Context, agentID string) error {
	r.rec("Consolidate", agentID)
	return nil
}
func (r *recordingStore) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	r.rec("CheckpointSave", cp.ID)
	return session.Checkpoint{ID: "saved"}, nil
}
func (r *recordingStore) CheckpointLoad(agentID, checkpointID string) (*session.Checkpoint, error) {
	r.rec("CheckpointLoad", agentID, checkpointID)
	return &session.Checkpoint{ID: "loaded"}, nil
}
func (r *recordingStore) CheckpointResume(agentID string) (*session.Checkpoint, error) {
	r.rec("CheckpointResume", agentID)
	return &session.Checkpoint{ID: "resumed"}, nil
}
func (r *recordingStore) CheckpointList(agentID string) ([]session.Checkpoint, error) {
	r.rec("CheckpointList", agentID)
	return []session.Checkpoint{{ID: "listed"}}, nil
}
func (r *recordingStore) SessionsList() ([]harness.HarnessSession, error) {
	r.rec("SessionsList")
	return []harness.HarnessSession{{ID: "s1"}}, nil
}
func (r *recordingStore) SessionKill(id string) error {
	r.rec("SessionKill", id)
	return nil
}
func (r *recordingStore) SessionResolve(agentID, sessionID string) (string, error) {
	r.rec("SessionResolve", agentID, sessionID)
	return "resolved", nil
}
func (r *recordingStore) SessionSave(hs harness.HarnessSession) error {
	r.rec("SessionSave", hs.ID)
	return nil
}
func (r *recordingStore) KGNodes() ([]kg.Node, error) {
	r.rec("KGNodes")
	return []kg.Node{{ID: "n1"}}, nil
}
func (r *recordingStore) KGLink(from, to, relation string) error {
	r.rec("KGLink", from, to, relation)
	return nil
}
func (r *recordingStore) AuditWrite(e audit.Entry) error {
	r.rec("AuditWrite", e.Action)
	return nil
}
func (r *recordingStore) TokenSummary(days int) (harness.TokenUsageSummary, error) {
	r.rec("TokenSummary", days)
	return harness.TokenUsageSummary{}, nil
}
func (r *recordingStore) TokenRecord(agent, model string, input, output, cacheRead, cacheWrite uint64) error {
	r.rec("TokenRecord", agent, model, input, output, cacheRead, cacheWrite)
	return nil
}
func (r *recordingStore) IsReadOnly() bool { r.rec("IsReadOnly"); return true }
func (r *recordingStore) Ready() error     { r.rec("Ready"); return nil }
func (r *recordingStore) Close() error     { r.rec("Close"); return nil }

// TestReconnectingStore_DelegatesEveryMethod checks that each wrapper forwards
// its arguments, in order, to the wrapped store.
//
// The wrappers are hand-written passthroughs, and the compiler cannot catch a
// swap between two parameters of the same type: SessionResolve(agentID,
// sessionID), KGLink(from, to, relation) and especially TokenRecord's four
// consecutive uint64s all compile fine with the arguments in the wrong order.
// A cacheRead/cacheWrite swap there would mis-price silently, on the one panel
// whose whole job is reporting money. Every argument below is distinct so a
// transposition fails rather than coincidentally matching.
func TestReconnectingStore_DelegatesEveryMethod(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		call     func(*reconnectingStore)
		wantArgs []any
	}{
		{"Remember", func(r *reconnectingStore) { _ = r.Remember(ctx, "agent-1", "text-2") }, []any{"agent-1", "text-2"}},
		{"PutShared", func(r *reconnectingStore) { _ = r.PutShared(ctx, "shared-1") }, []any{"shared-1"}},
		{"RecallDefault", func(r *reconnectingStore) { _, _ = r.RecallDefault(ctx, "agent-1", "query-2") }, []any{"agent-1", "query-2"}},
		{"Recall", func(r *reconnectingStore) { _, _ = r.Recall(ctx, "agent-1", "query-2", 3) }, []any{"agent-1", "query-2", 3}},
		{"RecallShared", func(r *reconnectingStore) { _, _ = r.RecallShared(ctx, "query-1", 4) }, []any{"query-1", 4}},
		{"RecallAll", func(r *reconnectingStore) { _, _ = r.RecallAll(ctx, "agent-1", "query-2", 5) }, []any{"agent-1", "query-2", 5}},
		{"List", func(r *reconnectingStore) { _, _ = r.List("agent-1") }, []any{"agent-1"}},
		{"ListAgents", func(r *reconnectingStore) { _, _ = r.ListAgents() }, nil},
		{"Stats", func(r *reconnectingStore) { _, _ = r.Stats("agent-1") }, []any{"agent-1"}},
		{"Delete", func(r *reconnectingStore) { _ = r.Delete("agent-1", "fact-2") }, []any{"agent-1", "fact-2"}},
		{"UpdateFact", func(r *reconnectingStore) { _ = r.UpdateFact("agent-1", memory.Fact{ID: "fact-2"}) }, []any{"agent-1", "fact-2"}},
		{"Consolidate", func(r *reconnectingStore) { _ = r.Consolidate(ctx, "agent-1") }, []any{"agent-1"}},
		{"CheckpointSave", func(r *reconnectingStore) { _, _ = r.CheckpointSave(session.Checkpoint{ID: "cp-1"}) }, []any{"cp-1"}},
		{"CheckpointLoad", func(r *reconnectingStore) { _, _ = r.CheckpointLoad("agent-1", "cp-2") }, []any{"agent-1", "cp-2"}},
		{"CheckpointResume", func(r *reconnectingStore) { _, _ = r.CheckpointResume("agent-1") }, []any{"agent-1"}},
		{"CheckpointList", func(r *reconnectingStore) { _, _ = r.CheckpointList("agent-1") }, []any{"agent-1"}},
		{"SessionsList", func(r *reconnectingStore) { _, _ = r.SessionsList() }, nil},
		{"SessionKill", func(r *reconnectingStore) { _ = r.SessionKill("sess-1") }, []any{"sess-1"}},
		{"SessionResolve", func(r *reconnectingStore) { _, _ = r.SessionResolve("agent-1", "sess-2") }, []any{"agent-1", "sess-2"}},
		{"SessionSave", func(r *reconnectingStore) { _ = r.SessionSave(harness.HarnessSession{ID: "sess-1"}) }, []any{"sess-1"}},
		{"KGNodes", func(r *reconnectingStore) { _, _ = r.KGNodes() }, nil},
		{"KGLink", func(r *reconnectingStore) { _ = r.KGLink("from-1", "to-2", "rel-3") }, []any{"from-1", "to-2", "rel-3"}},
		{"AuditWrite", func(r *reconnectingStore) { _ = r.AuditWrite(audit.Entry{Action: "act-1"}) }, []any{"act-1"}},
		{"TokenSummary", func(r *reconnectingStore) { _, _ = r.TokenSummary(30) }, []any{30}},
		{"TokenRecord", func(r *reconnectingStore) {
			_ = r.TokenRecord("agent-1", "model-2", 3, 4, 5, 6)
		}, []any{"agent-1", "model-2", uint64(3), uint64(4), uint64(5), uint64(6)}},
		{"IsReadOnly", func(r *reconnectingStore) { _ = r.IsReadOnly() }, nil},
		{"Ready", func(r *reconnectingStore) { _ = r.Ready() }, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recordingStore{}
			c.call(newReconnectingStore(rec))

			if rec.method != c.name {
				t.Fatalf("called %q on the wrapped store, want %q", rec.method, c.name)
			}
			if c.wantArgs != nil && !reflect.DeepEqual(rec.args, c.wantArgs) {
				t.Errorf("arguments forwarded as %#v, want %#v", rec.args, c.wantArgs)
			}
		})
	}

	// Every method on the interface must appear above, so adding one to
	// cliStore without a delegation case fails here rather than shipping
	// untested.
	covered := make(map[string]bool, len(cases))
	for _, c := range cases {
		covered[c.name] = true
	}
	covered["Close"] = true // lifecycle, asserted separately below
	iface := reflect.TypeOf((*cliStore)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		if name := iface.Method(i).Name; !covered[name] {
			t.Errorf("cliStore.%s has no delegation case; the wrapper is untested", name)
		}
	}
}

func TestReconnectingStore_CloseClosesCurrentHandle(t *testing.T) {
	rec := &recordingStore{}
	if err := newReconnectingStore(rec).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rec.method != "Close" {
		t.Errorf("Close did not reach the wrapped store (last call %q)", rec.method)
	}
}

// TestReconnectingStore_ReturnsWrappedValues checks the return path too: a
// wrapper that forwards arguments correctly but drops or mangles the result is
// just as broken.
func TestReconnectingStore_ReturnsWrappedValues(t *testing.T) {
	rs := newReconnectingStore(&recordingStore{})

	if got, _ := rs.Stats("a"); got.FactCount != 7 {
		t.Errorf("Stats returned %d facts, want the wrapped store's 7", got.FactCount)
	}
	if got, _ := rs.SessionResolve("a", "s"); got != "resolved" {
		t.Errorf("SessionResolve returned %q, want %q", got, "resolved")
	}
	if got, _ := rs.CheckpointResume("a"); got == nil || got.ID != "resumed" {
		t.Errorf("CheckpointResume did not return the wrapped checkpoint: %#v", got)
	}
	if got, _ := rs.List("a"); len(got) != 1 || got[0].ID != "f1" {
		t.Errorf("List returned %#v, want the wrapped store's single fact", got)
	}
	if !rs.IsReadOnly() {
		t.Error("IsReadOnly did not return the wrapped store's value")
	}
}
