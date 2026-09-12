package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TD-002 guard. mcp-go's WithOutputSchema swallows generation failures to
// stderr, which would publish a tool without its declared contract. Our
// wrapper fails fast instead; its panic path is effectively unreachable with
// reflectable Go types (jsonschema reflection handles even chan/func/complex
// fields — verified empirically), so this test pins the reachable half: every
// result type in this package generates a valid object schema, and the helper
// is a drop-in ToolOption.
func TestOutputSchemaOfGeneratesForEveryResultType(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  mcp.ToolOption
	}{
		{"memory_search", outputSchemaOf[searchResult]()},
		{"memory_add", outputSchemaOf[addResult]()},
		{"checkpoint_save", outputSchemaOf[checkpointSaveResult]()},
		{"checkpoint_resume", checkpointResumeOutputSchema()},
		{"memory_reflect", outputSchemaOf[reflectResult]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := mcp.NewTool(tc.name, tc.opt)
			raw, err := json.Marshal(tool)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var wire struct {
				OutputSchema json.RawMessage `json:"outputSchema"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(wire.OutputSchema) == 0 {
				t.Fatalf("%s: generated output schema never reached the wire", tc.name)
			}
		})
	}
}

// TestCheckpointResumeOutputSchemaIsUnion pins the union builder's shape: two
// object branches, the generated success payload first and the exact absence
// marker second, with no root-level properties/required/additionalProperties
// (a root constraint applies to every branch and breaks validation).
func TestCheckpointResumeOutputSchemaIsUnion(t *testing.T) {
	tool := mcp.NewTool("checkpoint_resume", checkpointResumeOutputSchema())
	raw, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(wire.OutputSchema, &root); err != nil {
		t.Fatalf("decode outputSchema: %v", err)
	}
	for _, forbidden := range []string{"properties", "required", "additionalProperties"} {
		if _, present := root[forbidden]; present {
			t.Errorf("union root must not declare %s; it would constrain every branch", forbidden)
		}
	}
	var rootType string
	if err := json.Unmarshal(root["type"], &rootType); err != nil || rootType != "object" {
		t.Fatalf("union root type = %s (%v), want object", root["type"], err)
	}

	var branches []json.RawMessage
	if err := json.Unmarshal(root["oneOf"], &branches); err != nil {
		t.Fatalf("decode oneOf: %v", err)
	}
	if len(branches) != 2 {
		t.Fatalf("oneOf has %d branches, want 2", len(branches))
	}

	// Branch 1 is generated from the Go result type, never hand-written, so
	// it cannot drift from checkpointResumeResult.
	generated, err := mcp.SchemaForRaw[checkpointResumeResult]()
	if err != nil {
		t.Fatalf("generate success schema: %v", err)
	}
	assertJSONEquivalent(t, "success branch", branches[0], generated)

	// The branches must stay disjoint for oneOf matching: a future field
	// addition that brings found or agent_id into checkpointResumeResult would
	// make some payloads match both branches.
	var successBranch struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(branches[0], &successBranch); err != nil {
		t.Fatalf("decode success branch properties: %v", err)
	}
	for _, key := range []string{"found", "agent_id"} {
		if _, present := successBranch.Properties[key]; present {
			t.Errorf("success branch must not declare absence key %q", key)
		}
	}

	// Branch 2 is the exact absence contract (ADR-015).
	assertJSONEquivalent(t, "absence branch", branches[1],
		json.RawMessage(`{"type":"object","additionalProperties":false,"required":["found","agent_id"],"properties":{"found":{"type":"boolean","enum":[false]},"agent_id":{"type":"string"}}}`))
}

func assertJSONEquivalent(t *testing.T, name string, got, want json.RawMessage) {
	t.Helper()
	var gotAny, wantAny any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatalf("%s: decode got: %v", name, err)
	}
	if err := json.Unmarshal(want, &wantAny); err != nil {
		t.Fatalf("%s: decode want: %v", name, err)
	}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}
