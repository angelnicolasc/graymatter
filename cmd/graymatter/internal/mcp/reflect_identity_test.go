package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Every persistence and optional confidence entry point records a call. Invalid
// identities must fail before even a read, including through confidenceGuard.
type reflectIdentityBackend struct {
	calls  []string
	agents []string
}

func (b *reflectIdentityBackend) record(method, agent string) {
	b.calls = append(b.calls, method)
	b.agents = append(b.agents, agent)
}
func (b *reflectIdentityBackend) Remember(_ context.Context, agent, _ string) error {
	b.record("Remember", agent)
	return nil
}
func (b *reflectIdentityBackend) Recall(_ context.Context, agent, _ string, _ int) ([]string, error) {
	b.record("Recall", agent)
	return nil, nil
}
func (b *reflectIdentityBackend) RecallDetailed(_ context.Context, agent, _ string, _ int) ([]string, string, error) {
	b.record("RecallDetailed", agent)
	return nil, "", nil
}
func (b *reflectIdentityBackend) RecallExplain(_ context.Context, agent, _ string, _ int) ([]memory.RecallReceipt, error) {
	b.record("RecallExplain", agent)
	return nil, nil
}
func (b *reflectIdentityBackend) PutAlias(_ context.Context, agent, _ string, _ []string) error {
	b.record("PutAlias", agent)
	return nil
}
func (b *reflectIdentityBackend) List(agent string) ([]memory.Fact, error) {
	b.record("List", agent)
	return []memory.Fact{{ID: "original", Text: "original fact", Weight: 1}}, nil
}
func (b *reflectIdentityBackend) UpdateFact(agent string, _ memory.Fact) error {
	b.record("UpdateFact", agent)
	return nil
}
func (b *reflectIdentityBackend) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	b.record("CheckpointSave", cp.AgentID)
	return cp, nil
}
func (b *reflectIdentityBackend) CheckpointResume(agent string) (*session.Checkpoint, error) {
	b.record("CheckpointResume", agent)
	return nil, nil
}
func (b *reflectIdentityBackend) CheckpointList(agent string) ([]session.Checkpoint, error) {
	b.record("CheckpointList", agent)
	return nil, nil
}
func (b *reflectIdentityBackend) AuditWrite(e audit.Entry) error {
	b.record("AuditWrite", e.Agent)
	return nil
}
func (b *reflectIdentityBackend) KGLink(_, _, _ string) error {
	b.record("KGLink", "")
	return nil
}
func (b *reflectIdentityBackend) PutReturningFact(_ context.Context, agent, text string) (memory.Fact, error) {
	b.record("PutReturningFact", agent)
	return memory.Fact{ID: "replacement", Text: text}, nil
}
func (b *reflectIdentityBackend) PutWithOptionsReturningFact(_ context.Context, agent, text string, _ memory.WriteOptions) (memory.Fact, error) {
	b.record("PutWithOptionsReturningFact", agent)
	return memory.Fact{ID: "replacement", Text: text}, nil
}
func (b *reflectIdentityBackend) ReviseFactsWithOptions(_ context.Context, agent, text string, _ memory.WriteOptions, _ ...memory.Fact) (memory.Fact, error) {
	b.record("ReviseFactsWithOptions", agent)
	return memory.Fact{ID: "replacement", Text: text}, nil
}
func (b *reflectIdentityBackend) Retire(agent string, _ ...memory.Fact) error {
	b.record("Retire", agent)
	return nil
}
func (b *reflectIdentityBackend) SetPinned(agent string, _ bool, _ ...memory.Fact) error {
	b.record("SetPinned", agent)
	return nil
}
func (b *reflectIdentityBackend) DefaultConfidenceWeight() float64 {
	b.record("DefaultConfidenceWeight", "")
	return 0
}
func (b *reflectIdentityBackend) Ready() error {
	b.record("Ready", "")
	return nil
}
func (b *reflectIdentityBackend) RecallWithOptions(_ context.Context, agent, _ string, _ int, _ memory.RecallOptions) (memory.RecallResult, error) {
	b.record("RecallWithOptions", agent)
	return memory.RecallResult{}, nil
}
func (b *reflectIdentityBackend) RecallExplainWithOptions(_ context.Context, agent, _ string, _ int, _ memory.RecallOptions) (memory.RecallExplainResult, error) {
	b.record("RecallExplainWithOptions", agent)
	return memory.RecallExplainResult{}, nil
}

func TestMemoryReflect_InvalidIdentityHasNoBackendCalls(t *testing.T) {
	invalid := []struct {
		name  string
		value any
	}{
		{"null", nil}, {"number", float64(7)}, {"boolean", true},
		{"object", map[string]any{"id": "project"}}, {"array", []any{"project"}},
		{"empty", ""}, {"whitespace", " \t\r\n"}, {"unicode whitespace", "\u2003\u00a0"},
	}
	identities := []struct {
		name string
		args map[string]any
	}{{"absent", nil}}
	for _, key := range []string{"agent_id", "agent"} {
		other := "agent_id"
		if key == other {
			other = "agent"
		}
		for _, bad := range invalid {
			identities = append(identities,
				struct {
					name string
					args map[string]any
				}{key + "/" + bad.name, map[string]any{key: bad.value}},
				struct {
					name string
					args map[string]any
				}{key + "/" + bad.name + "/other valid", map[string]any{key: bad.value, other: "project"}},
			)
		}
	}
	for _, transport := range []string{"handler", "jsonrpc"} {
		for _, action := range []string{"add", "update", "forget", "link", "pin", "unpin"} {
			for _, confidence := range []string{"omitted", "verified"} {
				for _, identity := range identities {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", transport, action, confidence, identity.name), func(t *testing.T) {
						backend := &reflectIdentityBackend{}
						s := New(backend, "test")
						args := map[string]any{"action": action, "text": "original fact", "target": "original fact"}
						for key, value := range identity.args {
							args[key] = value
						}
						if confidence != "omitted" {
							args["confidence"] = confidence
						}
						if transport == "handler" {
							result, err := s.handleMemoryReflect(context.Background(), reflectReq(args))
							if err != nil || result == nil || !result.IsError {
								t.Errorf("invalid identity accepted: result=%+v err=%v", result, err)
							} else if !strings.Contains(resultText(t, result), "agent") {
								t.Errorf("identity error must name the argument: %s", resultText(t, result))
							}
						} else if result := callToolJSONRPC(t, s, 1, "memory_reflect", args); !result.Result.IsError {
							t.Errorf("JSON-RPC accepted invalid identity: %+v", result.Result)
						}
						if len(backend.calls) != 0 {
							t.Errorf("invalid identity reached backend: %v (namespaces %q)", backend.calls, backend.agents)
						}
					})
				}
			}
		}
	}
}

func TestMemoryReflect_ValidIdentityPreservesNamespace(t *testing.T) {
	for _, agent := range []string{"__shared__", "proyecto-東京-🧠", " project ", "project"} {
		for _, key := range []string{"agent_id", "agent", "both"} {
			t.Run(agent+"/"+key, func(t *testing.T) {
				backend := &reflectIdentityBackend{}
				s := New(backend, "test")
				args := map[string]any{"action": "add", "text": "namespace probe"}
				if key == "both" {
					args["agent_id"], args["agent"] = agent, "other namespace"
				} else {
					args[key] = agent
				}
				result := callToolJSONRPC(t, s, 1, "memory_reflect", args)
				if result.Result.IsError {
					t.Fatalf("valid identity rejected: %+v", result.Result)
				}
				if len(backend.calls) == 0 {
					t.Fatal("valid add made no backend calls")
				}
				for i, actual := range backend.agents {
					if actual != agent {
						t.Errorf("%s changed namespace bytes: got %q want %q", backend.calls[i], actual, agent)
					}
				}
			})
		}
	}
}
