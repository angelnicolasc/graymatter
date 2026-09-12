package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// These tests exercise checkpoint_resume end to end through the JSON-RPC
// transport, because the contract they protect is client-visible: tools/list
// advertises a union output schema (ADR-015), and a strict client validates
// every structuredContent payload against it — including the absence result
// that must serialize its false marker rather than an empty object. Handler
// tests alone cannot prove that; the wire can.
//
// The union cannot be evaluated by the hand-rolled key/type checks in
// structured_contract_test.go, so a real JSON Schema engine compiles the
// advertised schema here. The helper is reused by that file.

// checkpointResumeRPCResult is the subset of a tools/call response these
// tests inspect. StructuredContent stays raw so an absent key (len 0) is
// distinguishable from an empty object.
type checkpointResumeRPCResult struct {
	Result *struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`
}

func callToolJSONRPC(t *testing.T, s *Server, id int, name string, arguments map[string]any) checkpointResumeRPCResult {
	t.Helper()

	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		t.Fatalf("marshal tools/call: %v", err)
	}
	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), request))
	if err != nil {
		t.Fatalf("marshal tools/call response: %v", err)
	}

	var response checkpointResumeRPCResult
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode tools/call response: %v\n%s", err, raw)
	}
	if response.Result == nil {
		t.Fatalf("tools/call returned no result: %s", raw)
	}
	return response
}

// compileOutputSchema compiles a raw outputSchema from tools/list so payloads
// can be validated with real branch selection (oneOf requires it).
func compileOutputSchema(t *testing.T, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()

	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode output schema: %v\n%s", err, raw)
	}
	compiler := jsonschema.NewCompiler()
	const location = "memory://tool-output.json"
	if err := compiler.AddResource(location, document); err != nil {
		t.Fatalf("add output schema: %v", err)
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		t.Fatalf("compile output schema: %v\n%s", err, raw)
	}
	return schema
}

// validateStructuredAgainstToolSchema validates a handler payload against the
// outputSchema the server actually advertises for toolName in tools/list.
func validateStructuredAgainstToolSchema(t *testing.T, toolName string, structured any) {
	t.Helper()

	tool, ok := listToolDefs(t)[toolName]
	if !ok {
		t.Fatalf("%s: missing from tools/list", toolName)
	}
	schema := compileOutputSchema(t, tool.OutputSchema)

	raw, err := json.Marshal(structured)
	if err != nil {
		t.Fatalf("%s: marshal structured content: %v", toolName, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("%s: structured content is not a JSON object: %v", toolName, err)
	}
	if err := schema.Validate(payload); err != nil {
		t.Fatalf("%s: structured content failed outputSchema validation: %v\npayload: %s", toolName, err, raw)
	}
}

func TestCheckpointResumeJSONRPCWireContract(t *testing.T) {
	s, _ := newTestServer(t)

	resume, ok := listToolDefs(t)["checkpoint_resume"]
	if !ok {
		t.Fatal("checkpoint_resume missing from tools/list")
	}
	schema := compileOutputSchema(t, resume.OutputSchema)

	// Found: every mode returns the ordinary checkpoint payload when a checkpoint
	// exists. on_missing affects absence only; the explicit modes must not select
	// the found:false branch.
	save := callToolJSONRPC(t, s, 1, "checkpoint_save", map[string]any{
		"agent_id": "rpc-agent",
		"state":    `{"step":3}`,
	})
	if save.Result.IsError {
		t.Fatalf("checkpoint_save failed: %+v", save.Result)
	}

	defaultSuccess := callToolJSONRPC(t, s, 2, "checkpoint_resume", map[string]any{"agent_id": "rpc-agent"})
	if defaultSuccess.Result.IsError {
		t.Fatalf("checkpoint_resume default success path returned an error: %+v", defaultSuccess.Result)
	}
	var baselinePayload map[string]any
	if err := json.Unmarshal(defaultSuccess.Result.StructuredContent, &baselinePayload); err != nil {
		t.Fatalf("decode default success structuredContent: %v", err)
	}
	if baselinePayload["id"] == nil || baselinePayload["created_at"] == nil {
		t.Fatalf("default success result missing required fields: %v", baselinePayload)
	}
	state, ok := baselinePayload["state"].(map[string]any)
	if !ok {
		t.Fatalf("default success result state = %T, want object: %v", baselinePayload["state"], baselinePayload)
	}
	if step, ok := state["step"].(float64); !ok || step != 3 {
		t.Fatalf("default success result state.step = %v, want 3", state["step"])
	}
	for _, absenceKey := range []string{"found", "agent_id"} {
		if _, present := baselinePayload[absenceKey]; present {
			t.Fatalf("default success result carried absence key %q: %v", absenceKey, baselinePayload)
		}
	}
	if err := schema.Validate(baselinePayload); err != nil {
		t.Fatalf("default success payload failed union schema (checkpoint branch): %v", err)
	}

	for _, tc := range []struct {
		name string
		id   int
		args map[string]any
	}{
		{"explicit error", 3, map[string]any{"agent_id": "rpc-agent", "on_missing": "error"}},
		{"explicit empty", 4, map[string]any{"agent_id": "rpc-agent", "on_missing": "empty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			success := callToolJSONRPC(t, s, tc.id, "checkpoint_resume", tc.args)
			if success.Result.IsError {
				t.Fatalf("checkpoint_resume success path returned an error: %+v", success.Result)
			}
			var successPayload map[string]any
			if err := json.Unmarshal(success.Result.StructuredContent, &successPayload); err != nil {
				t.Fatalf("decode success structuredContent: %v", err)
			}
			if successPayload["id"] == nil || successPayload["created_at"] == nil {
				t.Fatalf("success result missing required fields: %v", successPayload)
			}
			for _, absenceKey := range []string{"found", "agent_id"} {
				if _, present := successPayload[absenceKey]; present {
					t.Fatalf("success result carried absence key %q: %v", absenceKey, successPayload)
				}
			}
			if err := schema.Validate(successPayload); err != nil {
				t.Fatalf("success payload failed union schema (checkpoint branch): %v", err)
			}
			if !reflect.DeepEqual(successPayload, baselinePayload) {
				t.Fatalf("success payload = %v, want default payload %v", successPayload, baselinePayload)
			}
		})
	}

	// Default absence: text-only isError, no structuredContent key at all.
	missing := callToolJSONRPC(t, s, 5, "checkpoint_resume", map[string]any{"agent_id": "rpc-ghost"})
	if !missing.Result.IsError {
		t.Fatalf("missing checkpoint should set isError: %+v", missing.Result)
	}
	if len(missing.Result.StructuredContent) != 0 {
		t.Fatalf("default absence must omit structuredContent on the wire, got %s", missing.Result.StructuredContent)
	}

	// on_missing="empty": successful, machine-readable absence (branch 2).
	empty := callToolJSONRPC(t, s, 6, "checkpoint_resume", map[string]any{
		"agent_id":   "rpc-ghost",
		"on_missing": "empty",
	})
	if empty.Result.IsError {
		t.Fatalf("on_missing=empty absence returned an error: %+v", empty.Result)
	}
	var emptyPayload map[string]any
	if err := json.Unmarshal(empty.Result.StructuredContent, &emptyPayload); err != nil {
		t.Fatalf("decode absence structuredContent: %v", err)
	}
	found, present := emptyPayload["found"]
	if !present {
		t.Fatalf("absence payload dropped the found marker (omitempty regression): %s", empty.Result.StructuredContent)
	}
	if found != false {
		t.Fatalf("absence payload found = %v, want false", found)
	}
	if emptyPayload["agent_id"] != "rpc-ghost" {
		t.Fatalf("absence payload agent_id = %v, want rpc-ghost", emptyPayload["agent_id"])
	}
	if err := schema.Validate(emptyPayload); err != nil {
		t.Fatalf("absence payload failed union schema (branch 2): %v", err)
	}
	if raw := string(empty.Result.StructuredContent); !strings.Contains(raw, `"found":false`) {
		t.Fatalf("absence structuredContent = %s, want the false marker on the wire", raw)
	}

	// Invalid values are rejected with a text-only error result.
	bad := callToolJSONRPC(t, s, 7, "checkpoint_resume", map[string]any{
		"agent_id":   "rpc-ghost",
		"on_missing": "sometimes",
	})
	if !bad.Result.IsError {
		t.Fatalf("invalid on_missing should error: %+v", bad.Result)
	}
	if len(bad.Result.StructuredContent) != 0 {
		t.Fatalf("invalid on_missing carried structuredContent: %s", bad.Result.StructuredContent)
	}
}
