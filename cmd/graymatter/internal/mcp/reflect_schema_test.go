package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestMemoryReflect_AgentIDInSchema pins issue #77 step 1: the alias is part
// of the declared schema, not just a runtime accommodation. A client reading
// tools/list must be able to discover that agent_id is accepted; the runtime
// alias alone (TestMemoryReflect_AgentIDAlias) can never express that.
func TestMemoryReflect_AgentIDInSchema(t *testing.T) {
	byName := listToolDefs(t)
	tool, ok := byName["memory_reflect"]
	if !ok {
		t.Fatal("memory_reflect missing")
	}

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("decode inputSchema: %v", err)
	}

	aliasProp, ok := schema.Properties["agent_id"]
	if !ok {
		t.Fatal("agent_id missing from memory_reflect input schema")
	}
	if !strings.Contains(strings.ToLower(aliasProp.Description), "alias") {
		t.Errorf("agent_id description %q must state it is an alias", aliasProp.Description)
	}

	// Precedence is contractual: agent wins when both are set. The schema
	// must not promise otherwise, and `agent` stays required until the
	// canonical flip (ADR-013 defers that to a dedicated release).
	canonicalRequired := false
	for _, r := range schema.Required {
		if r == "agent" {
			canonicalRequired = true
		}
		if r == "agent_id" {
			t.Error("agent_id must not be required; it is the alias, not the canonical")
		}
	}
	if !canonicalRequired {
		t.Error("agent missing from required; canonical flip is a dedicated release (ADR-013)")
	}
}

// TestMemoryReflect_AliasPrecedencePinned verifies the runtime rule the schema
// documents: when both spellings arrive, agent wins. agent_id alone must also
// keep working for every mutating action, not just add (the alias test only
// covered add and forget).
func TestMemoryReflect_AliasPrecedencePinned(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	// agent wins: the fact lands under a1's namespace, not a2's. The a2 probe
	// must see the empty-state notice — asserting on the fact text alone would
	// false-positive, because the empty-state message quotes the query.
	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "add", "agent": "a1", "agent_id": "a2", "text": "isolation probe xyzzy",
	}))
	if err != nil || res.IsError {
		t.Fatalf("reflect with both spellings failed: %v / %s", err, resultText(t, res))
	}
	for _, probe := range []struct {
		agent     string
		wantFound bool
	}{{"a1", true}, {"a2", false}} {
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
			t.Errorf("agent %q must not recall a1's fact; got: %s", probe.agent, text)
		}
	}

	// agent_id alone drives update too: supersede a fact stored via the alias.
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "add", "agent_id": "a3", "text": "old convention",
	}))
	if err != nil || res.IsError {
		t.Fatalf("seed via alias failed: %v / %s", err, resultText(t, res))
	}
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update", "agent_id": "a3", "text": "new convention", "target": "old convention",
	}))
	if err != nil || res.IsError {
		t.Fatalf("update via alias failed: %v / %s", err, resultText(t, res))
	}
	res, err = s.handleMemorySearch(ctx, reflectReq(map[string]any{"agent_id": "a3", "query": "convention"}))
	if err != nil || res.IsError {
		t.Fatalf("search after alias update failed: %v / %s", err, resultText(t, res))
	}
	text := resultText(t, res)
	if strings.Contains(text, "old convention") || !strings.Contains(text, "new convention") {
		t.Errorf("alias-driven update did not supersede: %s", text)
	}
}
