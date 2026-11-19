package mcp

import (
	"strings"
	"testing"
)

func TestRP5ReflectUpdateID(t *testing.T) {
	for _, row := range []struct {
		name    string
		options []Option
	}{
		{name: "core"},
		{name: "full", options: []Option{WithFullCatalog()}},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv, mem := utilityContractServer(t, row.options...)
			const agent = "contingency-reflect"
			const original = "Original sampling interval is eight seconds"
			const corrected = "Current sampling interval is four seconds"
			utilityContractAdd(t, srv, agent, original)
			result := utilityContractCall(t, srv, "memory_reflect", map[string]any{
				"agent_id": agent, "action": "update", "target": original, "text": corrected,
			})
			if result.IsError || len(result.Content) != 1 {
				t.Fatalf("unexpected update: %s", result.Raw)
			}
			facts, err := mem.Advanced().List(agent)
			if err != nil {
				t.Fatal(err)
			}
			var id string
			for _, fact := range facts {
				if fact.Text == corrected && !fact.IsSuperseded() {
					id = fact.ID
				}
			}
			if id == "" {
				t.Fatal("update did not commit the corrected fact")
			}
			if row.name == "full" {
				if result.Structured["fact_id"] != id || !strings.Contains(result.text(), id) {
					t.Fatalf("legacy result lost full identity: %s", result.Raw)
				}
				return
			}
			short, ok := strings.CutPrefix(strings.TrimSuffix(result.text(), "."), "Stored ")
			if !ok || len(short) < 8 || len(short) >= len(id) || !strings.HasPrefix(id, short) ||
				result.Structured != nil || strings.Contains(result.text(), "\n") {
				t.Fatalf("expected one line with only the short identity: %s", result.Raw)
			}
		})
	}
}
