package mcp

import (
	"strings"
	"testing"
)

func TestV1SubjectRecoveryAcceptsDisplayedID(t *testing.T) {
	for _, tool := range []string{"memory_add", "memory_reflect"} {
		t.Run(tool, func(t *testing.T) {
			srv, mem := utilityContractServer(t)
			const ns = "unresolved-short-recovery"
			const subject = "dispatch window"
			for _, args := range []map[string]any{
				{"agent_id": ns, "text": "Dispatch window was Tuesday", "subject": subject},
				{"agent_id": ns, "text": "Dispatch window is Thursday"},
			} {
				if result := utilityContractCall(t, srv, "memory_add", args); result.IsError {
					t.Fatalf("seed: %s", result.Raw)
				}
			}
			store := mem.Advanced()
			facts, err := store.List(ns)
			if err != nil || len(facts) != 2 {
				t.Fatalf("seed facts: %+v %v", facts, err)
			}
			head, successor := facts[0], facts[1]
			if len(head.Subjects) == 0 {
				head, successor = successor, head
			}
			head.SupersededBy = successor.ID
			if err := store.UpdateFact(ns, head); err != nil {
				t.Fatal(err)
			}
			search := utilityContractCall(t, srv, "memory_search", map[string]any{
				"agent_id": ns, "query": "dispatch window",
			})
			marker, _, ok := strings.Cut(search.text(), " ")
			if search.IsError || !ok || marker == successor.ID || !strings.HasPrefix(successor.ID, marker) ||
				!strings.Contains(search.text(), "Unresolved subjects:") {
				t.Fatalf("missing short live successor or unresolved warning: %s", search.Raw)
			}
			args := map[string]any{
				"agent_id": ns, "subject": subject, "text": "Dispatch window is Friday",
				"replaces": []string{marker[:1]},
			}
			if tool == "memory_reflect" {
				args["action"] = "add"
			}
			ambiguous := utilityContractCall(t, srv, tool, args)
			args["replaces"] = []string{marker}
			recovered := utilityContractCall(t, srv, tool, args)
			if recovered.IsError {
				t.Fatalf("displayed ID could not recover subject: %s", recovered.Raw)
			}
			if !ambiguous.IsError || !strings.Contains(ambiguous.text(), "ambiguous") {
				t.Fatalf("ambiguous recovery was not rejected visibly: %s", ambiguous.Raw)
			}
			facts, err = store.List(ns)
			if err != nil {
				t.Fatal(err)
			}
			live := 0
			for _, fact := range facts {
				if fact.IsSuperseded() {
					continue
				}
				live++
				if fact.Text != args["text"] || len(fact.Subjects) != 1 || fact.Subjects[0].Name != subject {
					t.Fatalf("incorrect live recovery: %+v", fact)
				}
			}
			if live != 1 {
				t.Fatalf("recovery left %d live facts", live)
			}
		})
	}
}

func TestV1SubjectRecoveryRetainsExactHistoricalAnchor(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const ns = "unresolved-historical-recovery"
	const subject = "dispatch window"
	add := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": ns, "text": "Dispatch window was Tuesday", "subject": subject,
	})
	if add.IsError {
		t.Fatalf("seed: %s", add.Raw)
	}
	store := mem.Advanced()
	facts, err := store.List(ns)
	if err != nil || len(facts) != 1 {
		t.Fatalf("seed facts: %+v %v", facts, err)
	}
	head := facts[0]
	head.SupersededBy = "legacy-target-no-longer-present"
	if err := store.UpdateFact(ns, head); err != nil {
		t.Fatal(err)
	}
	recovered := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": ns, "text": "Dispatch window is Friday", "subject": subject, "replaces": []string{head.ID},
	})
	if recovered.IsError {
		t.Fatalf("exact historical anchor was not accepted: %s", recovered.Raw)
	}
}
