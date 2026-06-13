package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	graymatter "github.com/angelnicolasc/graymatter"
)

// newTestServer returns an MCP Server backed by a real Memory in a temp dir,
// through the DirectBackend (the same code path --no-daemon uses).
func newTestServer(t *testing.T) (*Server, *graymatter.Memory) {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return New(NewDirectBackend(mem, nil)), mem
}

func reflectReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
}

func TestMemoryAdd_AndSearch(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	// add requires agent_id and text
	if res, _ := s.handleMemoryAdd(ctx, reflectReq(map[string]any{"text": "x"})); !res.IsError {
		t.Error("memory_add without agent_id should error")
	}
	if res, _ := s.handleMemoryAdd(ctx, reflectReq(map[string]any{"agent_id": "a1"})); !res.IsError {
		t.Error("memory_add without text should error")
	}

	res, err := s.handleMemoryAdd(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "text": "the sky is blue",
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_add failed: %v / %s", err, resultText(t, res))
	}

	// search finds it
	res, err = s.handleMemorySearch(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "query": "sky", "top_k": float64(5),
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_search failed: %v / %s", err, resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "sky is blue") {
		t.Errorf("search result missing the fact: %s", resultText(t, res))
	}

	// search validation
	if res, _ := s.handleMemorySearch(ctx, reflectReq(map[string]any{"query": "x"})); !res.IsError {
		t.Error("memory_search without agent_id should error")
	}
	if res, _ := s.handleMemorySearch(ctx, reflectReq(map[string]any{"agent_id": "a1"})); !res.IsError {
		t.Error("memory_search without query should error")
	}

	// empty result is a clean message, not an error
	res, _ = s.handleMemorySearch(ctx, reflectReq(map[string]any{
		"agent_id": "nobody", "query": "nothing here",
	}))
	if res.IsError || !strings.Contains(resultText(t, res), "No memories found") {
		t.Errorf("expected clean empty-state, got: %s", resultText(t, res))
	}
}

func TestCheckpoint_SaveResume(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	if res, _ := s.handleCheckpointSave(ctx, reflectReq(map[string]any{})); !res.IsError {
		t.Error("checkpoint_save without agent_id should error")
	}
	// bad JSON state is rejected
	if res, _ := s.handleCheckpointSave(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "state": "{not json",
	})); !res.IsError {
		t.Error("checkpoint_save with invalid JSON state should error")
	}

	res, err := s.handleCheckpointSave(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "state": `{"step":3}`,
	}))
	if err != nil || res.IsError {
		t.Fatalf("checkpoint_save failed: %v / %s", err, resultText(t, res))
	}

	res, err = s.handleCheckpointResume(ctx, reflectReq(map[string]any{"agent_id": "a1"}))
	if err != nil || res.IsError {
		t.Fatalf("checkpoint_resume failed: %v / %s", err, resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "restored") {
		t.Errorf("resume result unexpected: %s", resultText(t, res))
	}

	// resume for an unknown agent errors
	if res, _ := s.handleCheckpointResume(ctx, reflectReq(map[string]any{"agent_id": "ghost"})); !res.IsError {
		t.Error("checkpoint_resume for unknown agent should error")
	}
}

func TestMemoryReflect_LinkRequiresKG(t *testing.T) {
	// DirectBackend with no KG linker: link must report unavailability.
	s, _ := newTestServer(t)
	res, _ := s.handleMemoryReflect(context.Background(), reflectReq(map[string]any{
		"action": "link", "agent": "a1", "text": "node-a", "target": "node-b",
	}))
	if !res.IsError || !strings.Contains(resultText(t, res), "knowledge graph") {
		t.Errorf("expected KG-unavailable error, got: %s", resultText(t, res))
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type %T", res.Content[0])
	}
	return tc.Text
}

// factWeight returns the weight of the fact with the given text, or -1 if absent.
func factWeight(t *testing.T, mem *graymatter.Memory, agentID, text string) float64 {
	t.Helper()
	facts, err := mem.Advanced().List(agentID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range facts {
		if f.Text == text {
			return f.Weight
		}
	}
	return -1
}

func TestMemoryReflect_ForgetViaText(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()
	const fact = "Workaround for Node 14 bug (project now on Node 18)"

	if err := mem.Remember(ctx, "a1", fact); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// The exact call shape from the docs that PR #10 reported as broken:
	// forget with the fact in text, no target.
	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "a1",
		"text":   fact,
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("forget via text should succeed, got error: %s", resultText(t, res))
	}
	if w := factWeight(t, mem, "a1", fact); w != 0 {
		t.Errorf("fact weight after forget = %v, want 0", w)
	}
}

func TestMemoryReflect_ForgetViaTarget(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()
	const fact = "stale fact"

	if err := mem.Remember(ctx, "a1", fact); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "a1",
		"target": fact,
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("forget via target should succeed, got error: %s", resultText(t, res))
	}
	if w := factWeight(t, mem, "a1", fact); w != 0 {
		t.Errorf("fact weight after forget = %v, want 0", w)
	}
}

func TestMemoryReflect_ForgetTargetWinsOverText(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()

	if err := mem.Remember(ctx, "a1", "fact A"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := mem.Remember(ctx, "a1", "fact B"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "a1",
		"text":   "fact A",
		"target": "fact B",
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("forget should succeed, got error: %s", resultText(t, res))
	}
	if w := factWeight(t, mem, "a1", "fact B"); w != 0 {
		t.Errorf("target fact B weight = %v, want 0 (target must win)", w)
	}
	if w := factWeight(t, mem, "a1", "fact A"); w == 0 {
		t.Errorf("text fact A was forgotten, but target should have won")
	}
}

func TestMemoryReflect_Validation(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{
			name:    "forget without text or target",
			args:    map[string]any{"action": "forget", "agent": "a1"},
			wantErr: "fact to forget",
		},
		{
			name:    "add without text",
			args:    map[string]any{"action": "add", "agent": "a1"},
			wantErr: "text",
		},
		{
			name:    "update without target",
			args:    map[string]any{"action": "update", "agent": "a1", "text": "new"},
			wantErr: "target",
		},
		{
			name:    "update without text",
			args:    map[string]any{"action": "update", "agent": "a1", "target": "old"},
			wantErr: "text",
		},
		{
			name:    "link without text",
			args:    map[string]any{"action": "link", "agent": "a1", "target": "node-b"},
			wantErr: "text",
		},
		{
			name:    "missing agent",
			args:    map[string]any{"action": "add", "text": "x"},
			wantErr: "agent",
		},
		{
			name:    "unknown action",
			args:    map[string]any{"action": "destroy", "agent": "a1", "text": "x"},
			wantErr: "unknown action",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleMemoryReflect(ctx, reflectReq(tc.args))
			if err != nil {
				t.Fatalf("handleMemoryReflect: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected tool error, got success: %s", resultText(t, res))
			}
			if got := resultText(t, res); !strings.Contains(got, tc.wantErr) {
				t.Errorf("error %q does not mention %q", got, tc.wantErr)
			}
		})
	}
}

func TestMemoryReflect_UpdateSupersedes(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()
	const oldFact = "API base URL is https://api.v1.example.com"
	const newFact = "API base URL is https://api.v2.example.com"

	if err := mem.Remember(ctx, "a1", oldFact); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update",
		"agent":  "a1",
		"text":   newFact,
		"target": oldFact,
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("update should succeed, got error: %s", resultText(t, res))
	}
	if w := factWeight(t, mem, "a1", oldFact); w != 0 {
		t.Errorf("old fact weight = %v, want 0", w)
	}
	if w := factWeight(t, mem, "a1", newFact); w <= 0 {
		t.Errorf("new fact weight = %v, want > 0", w)
	}
}
