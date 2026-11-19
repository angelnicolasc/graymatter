package mcp

import (
	"fmt"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestRP43TargetErrorsPointToTheOwnerOrLiveVersion(t *testing.T) {
	for _, arm := range []string{"memory_add", "memory_reflect"} {
		t.Run(arm, func(t *testing.T) {
			for _, tt := range []struct {
				name, owner string
				retire      bool
				expectHint  bool
			}{
				{"shared_owner", memory.SharedAgentID, false, true},
				{"other_project", "r-p43-other", false, true},
				{"successor", "r-p43-project", true, true},
				{"retired_without_successor", "r-p43-project", true, false},
			} {
				t.Run(tt.name, func(t *testing.T) {
					srv, mem := utilityContractServer(t)
					const project = "r-p43-project"
					const claim = "Deployment opens Thursday."
					args := map[string]any{"agent_id": tt.owner, "text": claim}
					if tt.name == "successor" {
						args["subject"] = "deployment"
					}
					old := utilityContractCall(t, srv, "memory_add", args)
					if old.IsError {
						t.Fatalf("seed old row: %s", old.Raw)
					}
					id := compactTestWriteIDs(t, old)[0]
					var localID string
					if tt.name == "successor" || arm == "memory_add" && tt.owner != project {
						rows, err := mem.Advanced().List(tt.owner)
						if err != nil || len(rows) != 1 {
							t.Fatalf("cannot identify canonical old row: %+v / %v", rows, err)
						}
						id = rows[0].ID
					}
					if arm == "memory_add" && tt.owner != project {
						utilityContractAdd(t, srv, project, "Locally scoped operation.")
						rows, err := mem.Advanced().List(project)
						if err != nil || len(rows) != 1 {
							t.Fatalf("cannot identify canonical local row: %+v / %v", rows, err)
						}
						localID = rows[0].ID
					}
					var live string
					if tt.name == "successor" {
						for _, text := range []string{"Deployment opens Friday.", "Deployment opens Saturday."} {
							next := utilityContractCall(t, srv, "memory_add", map[string]any{
								"agent_id": project, "text": text, "subject": "deployment",
							})
							if next.IsError {
								t.Fatalf("seed successor: %s", next.Raw)
							}
							live = compactTestWriteIDs(t, next)[0]
						}
					} else if tt.retire {
						retired := utilityContractCall(t, srv, "memory_reflect", map[string]any{
							"agent_id": project, "action": "forget", "target": id, "target_type": "id",
						})
						if retired.IsError {
							t.Fatalf("seed retirement: %s", retired.Raw)
						}
					}
					before, err := mem.Advanced().List(tt.owner)
					if err != nil {
						t.Fatal(err)
					}
					call := map[string]any{"agent_id": project, "text": "Incorrectly applied revision."}
					if arm == "memory_add" {
						call["replaces"] = []string{id}
						if localID != "" {
							call["replaces"] = []string{localID, id}
						}
					} else {
						call["action"], call["target"] = "update", id
					}
					got := utilityContractCall(t, srv, arm, call)
					var want string
					switch {
					case tt.owner != project:
						want = fmt.Sprintf("target ID %q belongs to scope %q; pass agent_id %q", id, tt.owner, tt.owner)
					case tt.expectHint:
						want = fmt.Sprintf("target ID %q was already replaced; use current ID %q", id, live)
					default:
						want = fmt.Sprintf("target ID %q was already retired and has no current replacement", id)
					}
					if arm == "memory_reflect" {
						want = "resolve target: " + want
					} else {
						want = "put facts: " + want
					}
					if !got.IsError || got.text() != want {
						t.Errorf("want error %q, got %s", want, got.Raw)
					}
					after, err := mem.Advanced().List(tt.owner)
					if err != nil || len(after) != len(before) {
						t.Fatalf("diagnostic mutated owner rows: %+v / %+v / %v", before, after, err)
					}
					for i := range before {
						if before[i].ID != after[i].ID || before[i].Text != after[i].Text || before[i].SupersededBy != after[i].SupersededBy {
							t.Fatalf("diagnostic changed an owner row: %+v / %+v", before[i], after[i])
						}
					}
					if tt.owner != project {
						rows, err := mem.Advanced().List(project)
						wantCount := 0
						if localID != "" {
							wantCount = 1
						}
						if err != nil || len(rows) != wantCount ||
							wantCount == 1 && (rows[0].IsSuperseded() || rows[0].Text != "Locally scoped operation.") {
							t.Fatalf("diagnostic redirected or wrote in request namespace: %+v / %v", rows, err)
						}
					}
				})
			}
		})
	}
}

func TestRP43RetainedErrorsAndFullParity(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const namespace = "r-p43-project"
	const target = "01M70000"
	for _, tt := range []struct {
		name     string
		server   *Server
		target   string
		selector string
		want     string
	}{
		{"unknown_id", srv, target, "id", `resolve target: target ID "01M70000" has no eligible canonical row`},
		{"explicit_text", srv, target, "text", "resolve target: target text not found"},
		{"unknown_text", srv, "an ordinary missing sentence", "", `resolve target: target ID "an ordinary missing sentence" has no eligible canonical row`},
		{"full", New(NewDirectBackend(mem, nil), "full", WithFullCatalog()), target, "", `target fact not found: "01M70000"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]any{"agent_id": namespace, "action": "update", "target": tt.target, "text": "new"}
			if tt.selector != "" {
				args["target_type"] = tt.selector
			}
			got := utilityContractCall(t, tt.server, "memory_reflect", args)
			if !got.IsError || got.text() != tt.want {
				t.Fatalf("want error %q, got %s", tt.want, got.Raw)
			}
		})
	}
}

func TestRP43MixedSubjectAndSharedReplaces(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const project = "r-p43-project"
	const subject = "deployment"
	write := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": project, "text": "Deployment opens Monday.", "subject": subject,
	})
	if write.IsError {
		t.Fatal(write.text())
	}
	utilityContractAdd(t, srv, memory.SharedAgentID, "Shared change window is Thursday.")
	ids := make([]string, 0, 2)
	for _, scope := range []string{project, memory.SharedAgentID} {
		rows, err := mem.Advanced().List(scope)
		if err != nil || len(rows) != 1 {
			t.Fatalf("scope %s fixture: %+v / %v", scope, rows, err)
		}
		ids = append(ids, rows[0].ID)
	}
	got := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": project, "text": "Deployment opens Tuesday.", "subject": subject, "replaces": ids,
	})
	want := fmt.Sprintf("target ID %q belongs to scope %q; pass agent_id %q",
		ids[1], memory.SharedAgentID, memory.SharedAgentID)
	if !got.IsError || got.text() != want {
		t.Fatalf("want %q, got %s", want, got.Raw)
	}
	rows, err := mem.Advanced().List(project)
	if err != nil || len(rows) != 1 || rows[0].ID != ids[0] || rows[0].IsSuperseded() {
		t.Fatalf("mixed-scope write changed the project head: %+v / %v", rows, err)
	}
}
