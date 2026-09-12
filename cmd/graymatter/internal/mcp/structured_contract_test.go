package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

// The issue-#76 acceptance requires that handler outputs validate against the
// declared outputSchema. These tests enforce that contract deterministically
// for flat object schemas:
//
//  1. every tool declares an outputSchema (type object) in tools/list;
//  2. every success result carries structuredContent whose key set is a
//     subset of the schema properties and which contains every required
//     schema property (omitempty fields may be absent, nothing else);
//  3. every present key has the JSON type the schema declares;
//  4. resume errors set isError and omit structuredContent on the wire.
//
// Point 2/3's hand-rolled checks cannot evaluate a oneOf union, so tools whose
// output schema is a union (checkpoint_resume, ADR-015) are validated with a
// real JSON Schema engine instead — see validateAgainstSchema.

type schemaShape struct {
	Type       string `json:"type"`
	Properties map[string]struct {
		// Type can be a single string ("string") or a union ("null" plus a
		// primitive) for nullable fields such as the search facts slice.
		Type any `json:"type"`
	} `json:"properties"`
	Required []string `json:"required"`
	// OneOf is set on output schemas that declare more than one successful
	// result shape. Each branch is a flat object shape; the root carries no
	// properties/required of its own.
	OneOf []schemaShape `json:"oneOf"`
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
		// A union root declares its shapes in oneOf branches and must not
		// constrain the branches with properties/required of its own
		// (a root additionalProperties:false would reject both).
		if len(schema.OneOf) > 0 {
			for i, branch := range schema.OneOf {
				if branch.Type != "object" {
					t.Errorf("%s: oneOf[%d].type = %q, want object", name, i, branch.Type)
				}
				if len(branch.Properties) == 0 {
					t.Errorf("%s: oneOf[%d] has no properties", name, i)
				}
			}
			continue
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

func TestCheckpointResumeNotFoundIsTextError(t *testing.T) {
	s, _ := newTestServer(t)
	res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{"agent_id": "sc-ghost"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("not-found must set isError")
	}

	if res.StructuredContent != nil {
		validateAgainstSchema(t, "checkpoint_resume", outputSchemas(t)["checkpoint_resume"], res.StructuredContent)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, present := wire["structuredContent"]; present {
		t.Error("resume error must omit structuredContent on the wire")
	}
	if got, want := resultText(t, res), `no checkpoint found for agent "sc-ghost": no checkpoints for agent "sc-ghost"`; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

type resumeErrorBackend struct {
	Backend
	err error
}

func (b resumeErrorBackend) CheckpointResume(string) (*session.Checkpoint, error) {
	return nil, b.err
}

func TestCheckpointResumeBackendFailure(t *testing.T) {
	for _, cause := range []error{errors.New("memory store not initialised"), errors.New("daemon connection lost"), errors.New("no checkpoints")} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := New(resumeErrorBackend{err: cause}, "test")
			res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{"agent_id": "sc-a"}))
			if err != nil || !res.IsError || res.StructuredContent != nil {
				t.Fatalf("result = %+v, error = %v; want text-only tool error", res, err)
			}
			if got, want := resultText(t, res), "checkpoint resume error: "+cause.Error(); got != want {
				t.Errorf("text = %q, want %q", got, want)
			}
		})
	}
}

func TestCheckpointResumeWrappedAbsence(t *testing.T) {
	cause := fmt.Errorf("backend: %w", session.ErrNoCheckpoint)
	s := New(resumeErrorBackend{err: cause}, "test")
	res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{"agent_id": "sc-a"}))
	if err != nil || !res.IsError || res.StructuredContent != nil {
		t.Fatalf("result = %+v, error = %v; want text-only tool error", res, err)
	}
	if got, want := resultText(t, res), `no checkpoint found for agent "sc-a": `+cause.Error(); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// validateAgainstSchema checks the payload against the declared schema shape:
// key subset of properties, required keys present, primitive types matching.
// Union schemas need branch selection and cannot be evaluated by those
// hand-rolled checks, so they go through a real JSON Schema engine.
func validateAgainstSchema(t *testing.T, toolName string, schema schemaShape, structured any) {
	t.Helper()
	if len(schema.OneOf) > 0 {
		validateStructuredAgainstToolSchema(t, toolName, structured)
		return
	}
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
