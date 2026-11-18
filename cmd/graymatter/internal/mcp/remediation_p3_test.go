package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRP3CoreCheckpointResume(t *testing.T) {
	core, _ := utilityContractServer(t)
	const agent = "contingency-checkpoint"
	resume := func() utilityContractResult {
		return utilityContractCall(t, core, "checkpoint_resume", map[string]any{"agent_id": agent})
	}
	assertAbsent := func() {
		t.Helper()
		got := resume()
		if got.IsError || got.Structured != nil || len(got.Content) != 1 ||
			strings.Contains(got.text(), "\n") || len(got.Raw) > 120 {
			t.Fatalf("expected bounded one-line absence: %s", got.Raw)
		}
	}
	assertAbsent()

	state := map[string]any{"step": "unique-marker", "progress": []any{float64(1), true}, "quoted": strings.Repeat(`"\`, 120)}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	saved := utilityContractCall(t, core, "checkpoint_save", map[string]any{
		"agent_id": agent, "state": string(raw),
	})
	if saved.IsError {
		t.Fatal(saved.text())
	}
	got := resume()
	id := saved.Structured["checkpoint_id"].(string)
	restored, err := json.Marshal(got.Structured["state"])
	if err != nil || got.IsError || got.Structured["id"] != id || len(got.Content) != 1 ||
		got.text() != "Resumed." || string(restored) != string(raw) ||
		strings.Count(string(got.Raw), "unique-marker") != 1 ||
		len(got.Raw) > len(raw)+120 {
		t.Fatalf("expected one compact copy of state and no creation line: %s", got.Raw)
	}

	completed := utilityContractCall(t, core, "checkpoint_save", map[string]any{
		"agent_id": agent, "state": "{\"status\":\"done\"}",
	})
	if completed.IsError {
		t.Fatal(completed.text())
	}
	assertAbsent()
}

func TestRP3FullCheckpointContract(t *testing.T) {
	full, _ := utilityContractServer(t, WithFullCatalog())
	args := map[string]any{"agent_id": "full-contingency-checkpoint"}
	missing := utilityContractCall(t, full, "checkpoint_resume", args)
	if !missing.IsError || !strings.Contains(missing.text(), "no checkpoint found") {
		t.Fatalf("expected legacy absence error: %s", missing.Raw)
	}
	saved := utilityContractCall(t, full, "checkpoint_save", map[string]any{
		"agent_id": args["agent_id"], "state": "{\"step\":1}",
	})
	if saved.IsError {
		t.Fatal(saved.text())
	}
	got := utilityContractCall(t, full, "checkpoint_resume", args)
	if got.IsError || got.Structured["id"] != saved.Structured["checkpoint_id"] ||
		!strings.Contains(got.text(), "Created:") || !strings.Contains(got.text(), "State:") {
		t.Fatalf("expected legacy text and structured checkpoint: %s", got.Raw)
	}
}
