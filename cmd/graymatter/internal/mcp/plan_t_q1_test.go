package mcp

import "testing"

func TestRPPlanTQ1CoreMemoryAddDescription(t *testing.T) {
	const want = "Store text or facts objects. For evidence give source and quote: the whole source text or paragraph, the same on every fact from that source. A subject names one current fact. In facts, set it inside each object; facts in one call never share a subject."
	if len(want) != 253 {
		t.Fatalf("description fixture has %d bytes, want 253", len(want))
	}

	for _, tool := range profileTools(t, newOptionsServer(t)) {
		if tool["name"] == "memory_add" {
			if got := tool["description"]; got != want {
				t.Errorf("memory_add description = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatal("memory_add missing from the core catalog")
}
