package mcp

import (
	"fmt"
	"strings"
	"testing"
)

func TestRP41SubjectAlternatives(t *testing.T) {
	for _, profile := range []string{ToolsCore, ToolsFull} {
		t.Run(profile, func(t *testing.T) {
			srv, mem := utilityContractServer(t)
			if profile == ToolsFull {
				srv = New(NewDirectBackend(mem, nil), "utility-test", WithFullCatalog())
			}
			const agent = "r41-subject"
			const topic = "Meridian proof_profile"
			const other = "Meridian sheet size"
			add := func(text, subject string, target ...string) utilityContractResult {
				t.Helper()
				args := map[string]any{"agent_id": agent, "text": text, "subject": subject}
				if len(target) != 0 {
					args["replaces"] = target
				}
				return utilityContractCall(t, srv, "memory_add", args)
			}
			const old = "Approved profile is Violet W50"
			const current = "Approved profile is Amber D65"
			first := add(old, topic)
			second := add(current, topic)
			unrelated := add("Sheet size is A4", other)
			if profile == ToolsFull {
				const want = "subject requires the core tools profile; restart mcp serve --tools core"
				for i, got := range []utilityContractResult{
					first, second, add("Same", topic, "01M70000"),
					add("Wrong", topic, "01M70001"), add("Recover", topic, "01M70002"),
				} {
					if !got.IsError || got.text() != want {
						t.Errorf("full case %d: %s", i, got.Raw)
					}
				}
				return
			}
			for _, got := range []utilityContractResult{first, second, unrelated} {
				if got.IsError {
					t.Fatal(got.text())
				}
			}
			if !strings.Contains(second.text(), "Replaced") {
				t.Fatal(second.text())
			}
			facts, err := mem.Advanced().List(agent)
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]string{}
			for _, f := range facts {
				ids[f.Text] = f.ID
			}
			for _, tt := range []struct {
				id, owner string
			}{
				{ids[current], topic},
				{ids["Sheet size is A4"], other},
			} {
				if tt.id == "" {
					t.Fatal("fixture lost its canonical ID")
				}
				got := add("Invalid revision", topic, tt.id)
				want := fmt.Sprintf("to replace %s, reuse its subject %q", tt.id, tt.owner)
				if !got.IsError || got.text() != want {
					t.Errorf("want %q, got %s", want, got.Raw)
				}
			}
			multiple := add("Invalid multi-ID revision", topic, ids[current], ids["Sheet size is A4"])
			for _, owner := range []string{topic, other} {
				if !multiple.IsError || !strings.Contains(multiple.text(), fmt.Sprintf("reuse its subject %q", owner)) {
					t.Errorf("multi-ID guidance omitted %s: %s", owner, multiple.Raw)
				}
			}
			mixed := utilityContractCall(t, srv, "memory_add", map[string]any{
				"agent_id": agent, "facts": []map[string]any{
					{"text": "Invalid first row", "subject": topic, "replaces": []string{ids[current]}},
					{"text": "Invalid second row", "subject": other, "replaces": []string{ids["Sheet size is A4"]}},
				},
			})
			if !mixed.IsError || !strings.Contains(mixed.text(), "if facts[0] is the conflict") ||
				!strings.Contains(mixed.text(), "if facts[1] is the conflict") {
				t.Errorf("batch guidance must be conditional: %s", mixed.Raw)
			}
			read := utilityContractCall(t, srv, "memory_search", map[string]any{
				"agent_id": agent, "query": "approved profile", "scope": "project",
			})
			if read.IsError || !strings.Contains(read.text(), current) ||
				strings.Contains(read.text(), old) || strings.Contains(read.text(), "Invalid revision") {
				t.Fatalf("unexpected live head: %s", read.Raw)
			}
		})
	}
}

func TestRP41UnresolvedRecovery(t *testing.T) {
	TestV1SubjectRecoveryAcceptsDisplayedID(t)
	TestV1SubjectRecoveryRetainsExactHistoricalAnchor(t)
}
