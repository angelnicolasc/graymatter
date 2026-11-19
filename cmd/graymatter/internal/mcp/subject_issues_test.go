package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestA1SubjectIssuesMCPProfiles(t *testing.T) {
	core, mem := utilityContractServer(t)
	store := mem.Advanced()
	for i := range 4 {
		name := fmt.Sprintf("deployment %d", i)
		result := utilityContractCall(t, core, "memory_add", map[string]any{
			"agent_id": "project", "subject": name, "text": fmt.Sprintf("Deployment was retired on Tuesday %d", i),
		})
		if result.IsError {
			t.Fatalf("seed subject: %s", result.Raw)
		}
	}
	utilityContractAdd(t, core, "project", "Deployment is on Thursday")
	facts, err := store.List("project")
	if err != nil || len(facts) != 5 {
		t.Fatalf("seed facts: %+v %v", facts, err)
	}
	var currentID string
	for _, fact := range facts {
		if fact.Text == "Deployment is on Thursday" {
			currentID = fact.ID
		}
	}
	if currentID == "" {
		t.Fatal("missing live replacement")
	}
	// Like the published writer, write retirement without a lineage edge.
	// UpdateFact keeps the retrieval index consistent with each retired row.
	for _, fact := range facts {
		if len(fact.Subjects) == 0 {
			continue
		}
		fact.SupersededBy = currentID
		if err := store.UpdateFact("project", fact); err != nil {
			t.Fatal(err)
		}
	}
	full := New(NewDirectBackend(mem, nil), "utility-test", WithFullCatalog())
	for _, tc := range []struct {
		name, tool string
		server     *Server
		args       map[string]any
	}{
		{"core search", "memory_search", core, map[string]any{"agent_id": "project", "query": "deployment"}},
		{"core batch", "memory_search", core, map[string]any{"agent_id": "project", "queries": []string{"deployment", "Thursday"}}},
		{"full search", "memory_search", full, map[string]any{"agent_id": "project", "query": "deployment"}},
		{"full explain", "memory_search", full, map[string]any{"agent_id": "project", "query": "deployment", "explain": true}},
		{"full batch", "memory_search_batch", full, map[string]any{"agent_id": "project", "queries": []string{"deployment", "Thursday"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := utilityContractCall(t, tc.server, tc.tool, tc.args)
			text := result.text()
			if result.IsError || !strings.Contains(text, "Deployment is on Thursday") || strings.Contains(text, "was retired") ||
				strings.Count(text, "Unresolved subjects:") != 1 || !strings.Contains(text, "(4 total)") {
				t.Fatalf("unexpected result: %s", result.Raw)
			}
			if tc.server == full && tc.tool == "memory_search" {
				feedback, _ := result.Structured["feedback"].(string)
				if strings.Count(feedback, "Unresolved subjects:") != 1 || !strings.Contains(feedback, "(4 total)") ||
					strings.Contains(feedback, "deployment 3") {
					t.Fatalf("unbounded or missing subject feedback: %q", feedback)
				}
				return
			}
			if tc.server == full && tc.tool == "memory_search_batch" {
				schema := compileOutputSchema(t, fullGoldenBatchOutputSchema(t))
				if err := schema.Validate(result.Structured); err != nil {
					t.Fatalf("full batch result violates the r4 output schema: %v", err)
				}
				return
			}
			if tc.server == core {
				if result.Structured != nil || strings.Contains(text, "deployment 3") {
					t.Fatalf("unbounded or structured core subject warning: %s", result.Raw)
				}
				return
			}
			raw, err := json.Marshal(result.Structured["subject_issues"])
			var issues memory.SubjectIssues
			if err != nil || json.Unmarshal(raw, &issues) != nil || issues.Total != 4 || len(issues.Subjects) != 3 {
				t.Fatalf("unbounded or missing subject diagnostics: %s", raw)
			}
		})
	}
	// Both profiles continue to omit the field and warning on healthy reads.
	for _, server := range []*Server{core, full} {
		result := utilityContractCall(t, server, "memory_search", map[string]any{"agent_id": "clean", "query": "deployment"})
		if _, present := result.Structured["subject_issues"]; result.IsError || present || strings.Contains(result.text(), "Unresolved subjects:") {
			t.Fatalf("healthy response changed: %s", result.Raw)
		}
	}
}

// Use the r4 golden schema rather than the candidate's live catalog.
func fullGoldenBatchOutputSchema(t *testing.T) json.RawMessage {
	t.Helper()

	golden, err := os.ReadFile("testdata/full-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var tools []struct {
		Name         string          `json:"name"`
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if err := json.Unmarshal(golden, &tools); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name == "memory_search_batch" && len(tool.OutputSchema) > 0 {
			return tool.OutputSchema
		}
	}
	t.Fatal("missing r4 batch output schema")
	return nil
}
