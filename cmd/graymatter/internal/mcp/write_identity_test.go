package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestR4IdentityMCPConfirmedWrites(t *testing.T) {
	for _, mode := range []string{"memory_add", "add", "update"} {
		for _, confidence := range []string{"", "verified"} {
			t.Run(mode+"/"+confidence, func(t *testing.T) {
				s, mem := confidenceFullTestServer(t)
				ctx := context.Background()
				const namespace = "confirmed-project"
				const text = "confirmed deployment policy"
				tool := "memory_reflect"
				args := map[string]any{"agent_id": namespace, "text": text}
				if confidence != "" {
					args["confidence"] = confidence
				}
				switch mode {
				case "memory_add":
					tool = mode
				case "add":
					args["action"] = mode
				case "update":
					args["action"], args["target"] = mode, "previous deployment policy"
					if err := mem.Remember(ctx, namespace, args["target"].(string)); err != nil {
						t.Fatal(err)
					}
				}
				result := confidenceToolCall(t, s, tool, args)
				payload, ok := result["structuredContent"].(map[string]any)
				if result["isError"] == true || !ok {
					t.Fatalf("write failed: %+v", result)
				}
				id, ok := payload["fact_id"].(string)
				if !ok || id == "" || !strings.Contains(writeIdentityText(t, result), "fact_id "+id) {
					t.Fatalf("missing committed identity in schema or text: %+v", result)
				}
				wantConfidence := confidence
				if mode == "update" && wantConfidence == "" {
					wantConfidence = "inferred"
				}
				if got, _ := payload["confidence"].(string); got != wantConfidence {
					t.Fatalf("confidence projection=%q, want %q", got, wantConfidence)
				}
				facts, err := mem.Advanced().List(namespace)
				if err != nil {
					t.Fatal(err)
				}
				var live, retired int
				for _, f := range facts {
					if f.IsSuperseded() {
						if mode != "update" || f.Text != args["target"] || f.SupersededBy != id {
							t.Fatalf("unrelated retirement: %+v", f)
						}
						retired++
						continue
					}
					if f.ID != id || f.AgentID != namespace || f.Text != text || f.Kind != memory.KindFact || f.Confidence != wantConfidence {
						t.Fatalf("receipt disagrees with committed row: %+v, receipt %+v", f, payload)
					}
					live++
				}
				if live != 1 || (mode == "update" && retired != 1) || (mode != "update" && retired != 0) {
					t.Fatalf("unexpected durable effects: %+v", facts)
				}
			})
		}
	}
}

type identityTestGraph struct{}

func (identityTestGraph) LinkNodes(string, string, string) error  { return nil }
func (identityTestGraph) UpsertNode(string, string, string) error { return nil }

func TestR4IdentityMCPCompatibilityAndLifecycleReceipts(t *testing.T) {
	_, mem := confidenceFullTestServer(t)
	s := New(NewDirectBackend(mem, identityTestGraph{}), "test", WithFullCatalog())
	const namespace = "legacy-project"
	const text = "fact with legacy backend"
	// Embedding only Backend exposes the safe local write without the
	// optional fact-returning methods.
	legacy := New(struct{ Backend }{NewDirectBackend(mem, nil)}, "test", WithFullCatalog())
	added := confidenceToolCall(t, legacy, "memory_add", map[string]any{"agent_id": namespace, "text": text})
	if added["isError"] == true || added["structuredContent"].(map[string]any)["fact_id"] != nil {
		t.Fatalf("legacy receipt invented identity: %+v", added)
	}
	facts, err := mem.Advanced().List(namespace)
	if err != nil || len(facts) != 1 || facts[0].Text != text {
		t.Fatalf("legacy write=%+v / %v", facts, err)
	}
	for _, action := range []string{"pin", "unpin", "link", "forget"} {
		out := confidenceToolCall(t, s, "memory_reflect", map[string]any{
			"agent_id": namespace, "action": action, "text": text, "target": text,
		})
		if out["isError"] == true || out["structuredContent"].(map[string]any)["fact_id"] != nil ||
			strings.Contains(writeIdentityText(t, out), "fact_id") {
			t.Fatalf("action %q returned an identity or failed: %+v", action, out)
		}
	}
	for _, tool := range []string{"memory_add", "memory_reflect"} {
		schema := outputSchemas(t)[tool]
		if schema.Properties["fact_id"].Type != "string" {
			t.Fatalf("%s omits the fact_id output schema", tool)
		}
		for _, key := range schema.Required {
			if key == "fact_id" {
				t.Fatalf("%s requires an identity even on legacy/lifecycle responses", tool)
			}
		}
	}
}

func writeIdentityText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("missing text content: %+v", result)
	}
	return content[0].(map[string]any)["text"].(string)
}
