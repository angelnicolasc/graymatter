package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

// Issue #123: a fresh agent having no checkpoint is an ordinary session-start
// state. It stays an isError result by default - flipping that would change the
// contract for every existing caller - and becomes a successful structured
// result only for a client that asks for it with on_missing=empty. Everything
// else, including real daemon/storage/decode failures, is unaffected.

// The absent case, in both modes, on a backend that has nothing saved.
func TestCheckpointResume_AbsenceIsAnErrorByDefaultAndAResultOnRequest(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"on_missing omitted", map[string]any{"agent_id": "ghost"}},
		{"on_missing error", map[string]any{"agent_id": "ghost", "on_missing": "error"}},
		{"on_missing empty string", map[string]any{"agent_id": "ghost", "on_missing": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleCheckpointResume(ctx, reflectReq(tc.args))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("the default must stay the historical isError result, got: %s", resultText(t, res))
			}
			if !strings.Contains(resultText(t, res), "no checkpoint found") {
				t.Errorf("error text changed: %s", resultText(t, res))
			}
			if res.StructuredContent != nil {
				t.Errorf("the default result must stay text-only, got structured: %#v", res.StructuredContent)
			}
		})
	}

	res, err := s.handleCheckpointResume(ctx, reflectReq(map[string]any{
		"agent_id": "ghost", "on_missing": "empty",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("on_missing=empty must report absence as a success, got: %s", resultText(t, res))
	}
	assertAbsent(t, res, "ghost")
	// The human-readable half is still there, as it is for every other success.
	if !strings.Contains(resultText(t, res), "ghost") {
		t.Errorf("absence result carries no prose naming the agent: %s", resultText(t, res))
	}
}

// A present checkpoint is the ordinary payload in every mode: on_missing must
// not touch the success path.
func TestCheckpointResume_APresentCheckpointIsUnchangedInEveryMode(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	if res, _ := s.handleCheckpointSave(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "state": `{"step":3}`,
	})); res.IsError {
		t.Fatalf("checkpoint_save failed: %s", resultText(t, res))
	}

	for _, args := range []map[string]any{
		{"agent_id": "a1"},
		{"agent_id": "a1", "on_missing": "error"},
		{"agent_id": "a1", "on_missing": "empty"},
	} {
		res, err := s.handleCheckpointResume(ctx, reflectReq(args))
		if err != nil || res.IsError {
			t.Fatalf("%v: resume failed: %v / %s", args, err, resultText(t, res))
		}
		payload := structuredMap(t, res)
		if _, found := payload["found"]; found {
			t.Errorf("%v: the resume payload must not gain a found field: %#v", args, payload)
		}
		if payload["id"] == "" || payload["id"] == nil {
			t.Errorf("%v: resume payload lost its id: %#v", args, payload)
		}
		if payload["created_at"] == nil {
			t.Errorf("%v: resume payload lost its created_at: %#v", args, payload)
		}
		if !strings.Contains(resultText(t, res), "restored") {
			t.Errorf("%v: prose changed: %s", args, resultText(t, res))
		}
	}
}

// An unrecognized value is refused rather than quietly defaulted: a caller that
// misspells the mode it wanted must not silently receive the mode it avoided.
func TestCheckpointResume_AnUnknownOnMissingValueIsRefused(t *testing.T) {
	s, _ := newTestServer(t)
	for _, value := range []string{"Empty", "EMPTY", "none", "false", "ok"} {
		res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{
			"agent_id": "ghost", "on_missing": value,
		}))
		if err != nil {
			t.Fatalf("%q: handler error: %v", value, err)
		}
		if !res.IsError || !strings.Contains(resultText(t, res), "on_missing") {
			t.Errorf("%q: expected a validation error naming on_missing, got: %s", value, resultText(t, res))
		}
	}
}

// on_missing converts exactly one error. A storage/decode failure is not an
// absent checkpoint and must stay an error even under empty.
func TestCheckpointResume_ARealFailureStaysAnErrorUnderEmpty(t *testing.T) {
	s, _ := newTestServer(t)
	s.backend = &resumeFailureBackend{
		DirectBackend: s.backend.(*DirectBackend),
		err:           errors.New("bolt: database not open"),
	}

	for _, args := range []map[string]any{
		{"agent_id": "a1"},
		{"agent_id": "a1", "on_missing": "empty"},
	} {
		res, err := s.handleCheckpointResume(context.Background(), reflectReq(args))
		if err != nil {
			t.Fatalf("%v: handler error: %v", args, err)
		}
		if !res.IsError {
			t.Fatalf("%v: a storage failure must stay an error, got: %s", args, resultText(t, res))
		}
		if !strings.Contains(resultText(t, res), "checkpoint resume error") {
			t.Errorf("%v: unexpected error text: %s", args, resultText(t, res))
		}
	}
}

// The advertised schema has to describe both successful shapes, and the two
// must not both validate one payload - otherwise a client cannot tell them
// apart from the schema alone.
func TestCheckpointResumeOutputSchema_DescribesBothShapesDisjointly(t *testing.T) {
	tool := mcp.NewTool("checkpoint_resume", checkpointResumeOutputSchema())
	raw, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		OutputSchema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
			OneOf      []struct {
				Required             []string                   `json:"required"`
				Properties           map[string]json.RawMessage `json:"properties"`
				AdditionalProperties *bool                      `json:"additionalProperties"`
			} `json:"oneOf"`
		} `json:"outputSchema"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if wire.OutputSchema.Type != "object" {
		t.Errorf("output schema is not an object schema: %q", wire.OutputSchema.Type)
	}
	if len(wire.OutputSchema.OneOf) != 2 {
		t.Fatalf("expected both shapes in the union, got %d: %s", len(wire.OutputSchema.OneOf), raw)
	}

	// This package's output-schema contract reads the TOP-LEVEL properties
	// (structured_contract_test.go), so the union of both halves' fields has to
	// be declared there or a legitimate payload reads as undeclared.
	for _, key := range []string{"id", "created_at", "state", "message_count", "found", "agent_id"} {
		if _, declared := wire.OutputSchema.Properties[key]; !declared {
			t.Errorf("top-level properties omit %q, so a valid payload key is undeclared", key)
		}
	}
	// And no top-level required list: the two shapes disagree about which keys
	// are mandatory, which is what the oneOf says instead.
	if len(wire.OutputSchema.Required) != 0 {
		t.Errorf("top-level required = %v; it cannot hold for both shapes", wire.OutputSchema.Required)
	}

	var payload, absent int
	for _, alt := range wire.OutputSchema.OneOf {
		required := strings.Join(alt.Required, ",")
		switch {
		case strings.Contains(required, "id") && strings.Contains(required, "created_at"):
			payload++
			if _, has := alt.Properties["found"]; has {
				t.Error("the resume payload must not declare found")
			}
		case strings.Contains(required, "found") && strings.Contains(required, "agent_id"):
			absent++
			if _, has := alt.Properties["id"]; has {
				t.Error("the absence result must not declare id")
			}
		default:
			t.Errorf("unrecognized alternative, required=%q", required)
		}
		// What makes the union unambiguous: neither half tolerates the other's
		// fields, so no payload can satisfy both.
		if alt.AdditionalProperties == nil || *alt.AdditionalProperties {
			t.Errorf("alternative required=%q allows additional properties, so the union is ambiguous", required)
		}
	}
	if payload != 1 || absent != 1 {
		t.Errorf("expected exactly one of each shape, got payload=%d absent=%d", payload, absent)
	}
}

func assertAbsent(t *testing.T, res *mcp.CallToolResult, agentID string) {
	t.Helper()
	payload := structuredMap(t, res)
	if found, ok := payload["found"].(bool); !ok || found {
		t.Errorf("expected found=false, got %#v", payload["found"])
	}
	if payload["agent_id"] != agentID {
		t.Errorf("expected agent_id=%q, got %#v", agentID, payload["agent_id"])
	}
	if _, has := payload["id"]; has {
		t.Errorf("no checkpoint id may be invented for an absent checkpoint: %#v", payload)
	}
}

// structuredMap re-encodes the structured content so the assertions run against
// what a client receives on the wire, not against the Go value.
func structuredMap(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("result carries no structured content: %s", resultText(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	return out
}

// resumeFailureBackend turns CheckpointResume into a non-absence failure while
// leaving every other call to the real backend, the same embedding the
// reflect-write failure fake uses.
type resumeFailureBackend struct {
	*DirectBackend
	err error
}

func (b *resumeFailureBackend) CheckpointResume(string) (*session.Checkpoint, error) {
	return nil, b.err
}
