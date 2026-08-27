package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestMemoryReflect_AgentIDCanonical pins issue #77 step 3 (the canonical
// flip): agent_id is the canonical spelling, agent is a documented deprecated
// alias, and the XOR (exactly one required) is expressed as anyOf over two
// required-lists because a flat required list would break one caller class or
// the other.
func TestMemoryReflect_AgentIDCanonical(t *testing.T) {
	byName := listToolDefs(t)
	tool, ok := byName["memory_reflect"]
	if !ok {
		t.Fatal("memory_reflect missing")
	}

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string          `json:"required"`
		AnyOf    []json.RawMessage `json:"anyOf"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("decode inputSchema: %v", err)
	}

	aliasProp, ok := schema.Properties["agent"]
	if !ok {
		t.Fatal("deprecated alias agent missing from memory_reflect input schema")
	}
	if !strings.Contains(strings.ToLower(aliasProp.Description), "deprecated") {
		t.Errorf("agent description %q must mark it deprecated", aliasProp.Description)
	}

	canonProp := schema.Properties["agent_id"]
	if !strings.Contains(strings.ToLower(canonProp.Description), "agent whose memory") {
		t.Errorf("agent_id description %q must state the canonical role", canonProp.Description)
	}

	// Canonical flip: required carries only action; the agent requirement is
	// the anyOf XOR, so callers spelling either name validate.
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}
	if !required["action"] {
		t.Error("action must be required")
	}
	if required["agent"] || required["agent_id"] {
		t.Error("neither agent spelling belongs in required; the XOR lives in anyOf")
	}
	if len(schema.AnyOf) != 2 {
		t.Fatalf("anyOf has %d branches, want 2 (agent_id / agent)", len(schema.AnyOf))
	}
	sawAgentID, sawAgent := false, false
	for _, branch := range schema.AnyOf {
		var b struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(branch, &b); err != nil {
			t.Fatalf("decode anyOf branch: %v", err)
		}
		if len(b.Required) == 1 {
			switch b.Required[0] {
			case "agent_id":
				sawAgentID = true
			case "agent":
				sawAgent = true
			}
		}
	}
	if !sawAgentID || !sawAgent {
		t.Errorf("anyOf must offer exactly the agent_id and agent alternatives (got agent_id=%v agent=%v)", sawAgentID, sawAgent)
	}
}

// TestMemoryReflect_AliasPrecedencePinned verifies the runtime rule the schema
// documents: since the flip, agent_id wins when both spellings arrive, while
// the deprecated spelling alone keeps driving every action.
func TestMemoryReflect_AliasPrecedencePinned(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	// agent_id wins: the fact lands under a2's namespace, not a1's. The a1
	// probe must see the empty-state notice — asserting on the fact text alone
	// would false-positive, because the empty-state message quotes the query.
	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "add", "agent": "a1", "agent_id": "a2", "text": "isolation probe xyzzy",
	}))
	if err != nil || res.IsError {
		t.Fatalf("reflect with both spellings failed: %v / %s", err, resultText(t, res))
	}
	for _, probe := range []struct {
		agent     string
		wantFound bool
	}{{"a2", true}, {"a1", false}} {
		res, err := s.handleMemorySearch(ctx, reflectReq(map[string]any{
			"agent_id": probe.agent, "query": "isolation probe xyzzy",
		}))
		if err != nil || res.IsError {
			t.Fatalf("search %q failed: %v / %s", probe.agent, err, resultText(t, res))
		}
		text := resultText(t, res)
		emptyState := strings.Contains(text, "No memories found")
		if probe.wantFound && (emptyState || !strings.Contains(text, "isolation probe xyzzy")) {
			t.Errorf("fact not found under %q: %s", probe.agent, text)
		}
		if !probe.wantFound && !emptyState {
			t.Errorf("agent %q must not recall a2's fact; got: %s", probe.agent, text)
		}
	}

	// The deprecated spelling alone still drives update: supersede a fact
	// stored under a3 via `agent`.
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "add", "agent_id": "a3", "text": "old convention",
	}))
	if err != nil || res.IsError {
		t.Fatalf("seed failed: %v / %s", err, resultText(t, res))
	}
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update", "agent": "a3", "text": "new convention", "target": "old convention",
	}))
	if err != nil || res.IsError {
		t.Fatalf("update via deprecated alias failed: %v / %s", err, resultText(t, res))
	}
	res, err = s.handleMemorySearch(ctx, reflectReq(map[string]any{"agent_id": "a3", "query": "convention"}))
	if err != nil || res.IsError {
		t.Fatalf("search after alias update failed: %v / %s", err, resultText(t, res))
	}
	text := resultText(t, res)
	if strings.Contains(text, "old convention") || !strings.Contains(text, "new convention") {
		t.Errorf("deprecated-alias update did not supersede: %s", text)
	}
}
