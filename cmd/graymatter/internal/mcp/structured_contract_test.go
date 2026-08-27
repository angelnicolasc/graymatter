package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// The issue-#76 acceptance requires that handler outputs validate against the
// declared outputSchema. These tests enforce that contract deterministically,
// without pulling a schema validator dependency:
//
//  1. every tool declares an outputSchema (type object) in tools/list;
//  2. every success result carries structuredContent whose key set is a
//     subset of the schema properties and which contains every required
//     schema property (omitempty fields may be absent, nothing else);
//  3. every present key has the JSON type the schema declares;
//  4. the not-found resume error is typed, isError, and its payload matches
//     its own declared shape.

type schemaShape struct {
	Type       string `json:"type"`
	Properties map[string]struct {
		// Type can be a single string ("string") or a union ("null" plus a
		// primitive) for nullable fields such as the search facts slice.
		Type any `json:"type"`
	} `json:"properties"`
	Required []string `json:"required"`
}

func outputSchemas(t *testing.T) map[string]schemaShape {
	t.Helper()
	byName := listToolDefs(t)
	out := make(map[string]schemaShape, len(byName))
	for name, tool := range byName {
		if len(tool.OutputSchema) == 0 {
			t.Fatalf("%s: no outputSchema declared in tools/list", name)
		}
		var schema schemaShape
		if err := json.Unmarshal(tool.OutputSchema, &schema); err != nil {
			t.Fatalf("%s: decode outputSchema: %v", name, err)
		}
		out[name] = schema
	}
	return out
}

func TestToolsDeclareObjectOutputSchemas(t *testing.T) {
	for name, schema := range outputSchemas(t) {
		if schema.Type != "object" {
			t.Errorf("%s: outputSchema.type = %q, want object", name, schema.Type)
		}
		if len(schema.Properties) == 0 {
			t.Errorf("%s: outputSchema has no properties", name)
		}
	}
}

func TestStructuredContentMatchesOutputSchema(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	schemas := outputSchemas(t)

	// Drive every handler down its success path, including each reflect
	// action, so no structured payload escapes validation.
	seeds := []struct {
		name string
		call func() (structured any, isError bool, err error)
	}{
		{"memory_search", func() (any, bool, error) {
			if _, err := s.handleMemoryAdd(ctx, reflectReq(map[string]any{"agent_id": "sc-a", "text": "structured content probe fact"})); err != nil {
				return nil, false, err
			}
			res, err := s.handleMemorySearch(ctx, reflectReq(map[string]any{"agent_id": "sc-a", "query": "structured content probe"}))
			return res.StructuredContent, res.IsError, err
		}},
		{"memory_search empty", func() (any, bool, error) {
			res, err := s.handleMemorySearch(ctx, reflectReq(map[string]any{"agent_id": "sc-empty", "query": "nothing matches this"}))
			return res.StructuredContent, res.IsError, err
		}},
		{"memory_add", func() (any, bool, error) {
			res, err := s.handleMemoryAdd(ctx, reflectReq(map[string]any{"agent_id": "sc-a", "text": "another fact"}))
			return res.StructuredContent, res.IsError, err
		}},
		{"checkpoint_save", func() (any, bool, error) {
			res, err := s.handleCheckpointSave(ctx, reflectReq(map[string]any{"agent_id": "sc-a", "state": `{"step":1}`}))
			return res.StructuredContent, res.IsError, err
		}},
		{"checkpoint_resume", func() (any, bool, error) {
			res, err := s.handleCheckpointResume(ctx, reflectReq(map[string]any{"agent_id": "sc-a"}))
			return res.StructuredContent, res.IsError, err
		}},
		{"memory_reflect add", func() (any, bool, error) {
			res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{"action": "add", "agent": "sc-a", "text": "reflect add probe"}))
			return res.StructuredContent, res.IsError, err
		}},
		{"memory_reflect update", func() (any, bool, error) {
			res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{"action": "update", "agent": "sc-a", "text": "reflect add probe v2", "target": "reflect add probe"}))
			return res.StructuredContent, res.IsError, err
		}},
		{"memory_reflect pin", func() (any, bool, error) {
			res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{"action": "pin", "agent": "sc-a", "text": "reflect add probe v2"}))
			return res.StructuredContent, res.IsError, err
		}},
	}

	for _, tc := range seeds {
		t.Run(tc.name, func(t *testing.T) {
			structured, isError, err := tc.call()
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if isError {
				t.Fatalf("unexpected tool error on success path")
			}
			if structured == nil {
				t.Fatal("success result carries no structuredContent")
			}

			toolName := tc.name
			if i := indexByte(toolName, ' '); i >= 0 {
				toolName = toolName[:i]
			}
			validateAgainstSchema(t, toolName, schemas[toolName], structured)
		})
	}
}

func TestCheckpointResumeNotFoundIsTypedError(t *testing.T) {
	s, _ := newTestServer(t)
	res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{"agent_id": "sc-ghost"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("not-found must set isError")
	}

	payload, ok := res.StructuredContent.(checkpointResumeNotFound)
	if !ok {
		t.Fatalf("structured content is %T, want checkpointResumeNotFound", res.StructuredContent)
	}
	if payload.Error != "not_found" {
		t.Errorf("error code = %q, want not_found", payload.Error)
	}
	if payload.AgentID != "sc-ghost" {
		t.Errorf("agent_id = %q, want sc-ghost", payload.AgentID)
	}
}

// validateAgainstSchema checks the payload against the declared schema shape:
// key subset of properties, required keys present, primitive types matching.
func validateAgainstSchema(t *testing.T, toolName string, schema schemaShape, structured any) {
	t.Helper()
	raw, err := json.Marshal(structured)
	if err != nil {
		t.Fatalf("%s: marshal structured content: %v", toolName, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("%s: structured content is not a JSON object: %v", toolName, err)
	}

	required := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		required[r] = true
	}
	for key := range payload {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("%s: structured key %q not declared in outputSchema", toolName, key)
		}
	}
	for key := range required {
		if _, ok := payload[key]; !ok {
			t.Errorf("%s: required output key %q missing from structured content", toolName, key)
		}
	}
	for key, val := range payload {
		prop, ok := schema.Properties[key]
		if !ok {
			continue
		}
		if !typeMatches(prop.Type, val) {
			t.Errorf("%s.%s: got %T (%v), schema says %v", toolName, key, val, val, prop.Type)
		}
	}
}

// typeMatches reports whether val satisfies the schema type, which may be a
// single primitive string or a union array (["null", "array"]).
func typeMatches(schemaType any, val any) bool {
	switch t := schemaType.(type) {
	case string:
		return primitiveMatches(t, val)
	case []any:
		for _, member := range t {
			name, ok := member.(string)
			if !ok {
				continue
			}
			if name == "null" {
				if val == nil {
					return true
				}
				continue
			}
			if primitiveMatches(name, val) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func primitiveMatches(name string, val any) bool {
	switch name {
	case "string":
		_, ok := val.(string)
		return ok
	case "number", "integer":
		_, ok := val.(float64)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "object":
		_, ok := val.(map[string]any)
		return ok
	default:
		return false
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
