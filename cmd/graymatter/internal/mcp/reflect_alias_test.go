package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestMemoryReflect_AgentIDAlias pins the parameter alias: the schema names
// the field `agent`, but every other GrayMatter tool uses `agent_id` and
// models generalize across a toolset. Before the alias an agent that sent
// agent_id here got a bare "agent is required" — a silent stop in practice,
// because nothing on screen explains that one tool spells it differently.
func TestMemoryReflect_AgentIDAlias(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "canonical agent", args: map[string]any{"action": "add", "agent": "a1", "text": "alias canonical"}},
		{name: "agent_id alias", args: map[string]any{"action": "add", "agent_id": "a2", "text": "alias accepted"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleMemoryReflect(ctx, reflectReq(tc.args))
			if err != nil || res.IsError {
				t.Fatalf("reflect add failed: %v / %s", err, resultText(t, res))
			}
		})
	}

	for _, tc := range []struct{ agentID, want string }{
		{"a1", "alias canonical"},
		{"a2", "alias accepted"},
	} {
		res, err := s.handleMemorySearch(ctx, reflectReq(map[string]any{
			"agent_id": tc.agentID, "query": tc.want,
		}))
		if err != nil || res.IsError {
			t.Fatalf("search %q failed: %v / %s", tc.agentID, err, resultText(t, res))
		}
		if !strings.Contains(resultText(t, res), tc.want) {
			t.Errorf("fact stored under %q not found: %s", tc.agentID, resultText(t, res))
		}
	}
}

// TestMemoryReflect_AliasReachesEveryAction proves the alias is read from the
// single extraction point (not re-derived per action): forget routed by
// agent_id must retire exactly the matching fact.
func TestMemoryReflect_AliasReachesForget(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	res, err := s.handleMemoryAdd(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "text": "stale fact",
	}))
	if err != nil || res.IsError {
		t.Fatalf("seed add: %v / %s", err, resultText(t, res))
	}

	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget", "agent_id": "a1", "text": "stale fact",
	}))
	if err != nil || res.IsError {
		t.Fatalf("forget via alias failed: %v / %s", err, resultText(t, res))
	}

	res, err = s.handleMemorySearch(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "query": "stale",
	}))
	if err != nil || res.IsError {
		t.Fatalf("search after forget failed: %v / %s", err, resultText(t, res))
	}
	if strings.Contains(resultText(t, res), "stale fact") {
		t.Error("forgotten fact is still live")
	}
}

// TestMemoryReflect_MissingAgentStillErrors keeps the failure mode explicit:
// neither spelling present means a clear error, never a silent default.
func TestMemoryReflect_MissingAgentStillErrors(t *testing.T) {
	s, _ := newTestServer(t)

	res, err := s.handleMemoryReflect(context.Background(), reflectReq(map[string]any{
		"action": "add", "text": "orphan",
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error when neither agent nor agent_id is set")
	}
	if got := resultText(t, res); !strings.Contains(got, "agent") {
		t.Errorf("error %q does not mention the missing parameter", got)
	}
}
