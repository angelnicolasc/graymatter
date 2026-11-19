package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestReviewC111MCPWireMatchesStrictSchemaAndProse(t *testing.T) {
	server, mem := newTestServer(t)
	store := mem.Advanced().(*memory.Store)
	ctx := context.Background()
	const namespace = "review-fixture"
	const providerNote = "UNTRUSTED_PROVIDER_NOTE"
	verified, unverified := "verified", "unverified"
	first, err := store.PutWithOptionsReturningFact(ctx, namespace, "Keep the quartz gateway open.", memory.WriteOptions{Confidence: &verified})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PutWithOptionsReturningFact(ctx, namespace, "Close the network gateway.", memory.WriteOptions{Confidence: &unverified})
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ReviewCommit(ctx, memory.ReviewSubmission{
		Namespace: namespace, Provider: "ollama", Model: "fixture", PromptVersion: "review-v1",
		Snapshots: []memory.EvidenceSnapshot{memory.NewEvidenceSnapshot(first), memory.NewEvidenceSnapshot(second)},
		Findings:  []memory.ReviewFinding{{Kind: "conflict", SourceIDs: []string{first.ID, second.ID}, Note: providerNote}},
	})
	if err != nil || len(records) != 1 {
		t.Fatalf("commit conflict = %+v, %v", records, err)
	}
	if err := store.Delete(namespace, second.ID); err != nil {
		t.Fatal(err)
	}

	for i, tc := range []struct {
		name      string
		tool      string
		arguments map[string]any
		explain   bool
	}{
		{"plain", "memory_search", map[string]any{"query": "quartz", "min_confidence": verified}, false},
		{"explain", "memory_search", map[string]any{"query": "quartz", "min_confidence": verified, "explain": true}, true},
		{"batch with default ranking", "memory_search_batch", map[string]any{"queries": []string{"quartz", "gateway"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.arguments["agent_id"] = namespace
			resp := callToolJSONRPC(t, server, i+1, tc.tool, tc.arguments)
			if resp.Result.IsError {
				t.Fatalf("tool error = %+v", resp.Result)
			}
			raw := resp.Result.StructuredContent
			validateStructuredAgainstToolSchema(t, tc.tool, raw)
			var wire struct {
				Notices   []memory.ReviewNotice     `json:"conflict_notices"`
				Explained []memory.RecallReceipt    `json:"explained"`
				Retrieval *memory.RetrievalMetadata `json:"retrieval"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			notices := wire.Notices
			if tc.explain {
				if len(wire.Explained) != 1 {
					t.Fatalf("expected one receipt, got %s", raw)
				}
				notices = wire.Explained[0].ReviewNotices
			}
			if len(notices) != 1 || notices[0].ID != records[0].ID || notices[0].SourceState != "missing" ||
				!slices.Equal(notices[0].SourceIDs, []string{first.ID}) {
				t.Fatalf("unexpected notices in %s", raw)
			}
			if tc.tool == "memory_search_batch" && wire.Retrieval != nil {
				t.Fatalf("default batch added confidence metadata: %s", raw)
			}
			if len(resp.Result.Content) != 1 {
				t.Fatalf("unexpected prose blocks: %+v", resp.Result.Content)
			}
			var prose struct{ Text string }
			if err := json.Unmarshal(resp.Result.Content[0], &prose); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prose.Text, memory.ReviewNoticesText(notices)) ||
				strings.Contains(prose.Text, providerNote) || strings.Contains(string(raw), providerNote) ||
				strings.Contains(prose.Text, second.ID) || strings.Contains(string(raw), second.ID) {
				t.Fatalf("mismatched or unsafe prose %q for structured result %s", prose.Text, raw)
			}
		})
	}
}
