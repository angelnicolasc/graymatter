package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func TestConfidenceLegacyLifecycleRealBinaryAndMCP(t *testing.T) {
	// Build before the fixture isolates HOME; Go's read-only module cache must
	// stay outside t.TempDir so Unix cleanup can remove the fixture.
	previous := confidencePreviousBinary(t)
	f := newIssue81Fixture(t)
	dir := t.TempDir()
	oldProcess := confidenceStartDaemon(t, previous, dir)
	initial, err := daemon.ConnectNoSpawn(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopens := 0
	store := newReconnectingStoreAt(daemonStore{Client: initial}, func() (cliStore, error) {
		reopens++
		client, err := daemon.ConnectNoSpawn(dir)
		if err != nil {
			return nil, err
		}
		return daemonStore{Client: client}, nil
	})
	t.Cleanup(func() { _ = store.Close() })
	for _, text := range []string{"cli fact", "mcp fact", "upgrade fact"} {
		// Seed through the legacy protocol before testing what the upgraded client can do.
		if err := initial.CallService(rpc.ServiceName, "Put", &rpc.PutRequest{AgentID: "lifecycle", Text: text}, &rpc.PutResponse{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := initial.List("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		fact.Confidence, fact.Weight, fact.AccessCount = "verified", 0.75, 42
		if err := initial.UpdateFact("lifecycle", fact); !errors.Is(err, rpc.ErrLineageUnsupported) {
			t.Fatalf("full-snapshot write against an old daemon: %v", err)
		}
		// Seed through the old wire contract. The production client's
		// full-snapshot writer requires authoritative lineage negotiation.
		request := rpc.UpdateFactRequest{AgentID: "lifecycle", Fact: fact}
		if err := initial.CallService(rpc.ServiceName, "UpdateFact", &request, &rpc.UpdateFactResponse{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	read := func(text string) memory.Fact {
		t.Helper()
		facts, err := store.List("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range facts {
			if fact.Text == text {
				if fact.Confidence != "verified" || fact.Weight != 0.75 || fact.AccessCount != 42 {
					t.Fatalf("lifecycle overwrote unrelated metadata: %+v", fact)
				}
				return fact
			}
		}
		t.Fatalf("missing fact %q", text)
		return memory.Fact{}
	}
	for _, action := range []string{"pin", "unpin", "forget"} {
		out := f.run(dir, "", "--dir", dir, action, "lifecycle", "cli fact")
		if out.code != 0 {
			t.Fatalf("legacy CLI %s failed: %s %s", action, out.stdout, out.stderr)
		}
		fact := read("cli fact")
		if fact.Pinned != (action == "pin") || fact.IsSuperseded() != (action == "forget") {
			t.Fatalf("legacy CLI %s did not persist: %+v", action, fact)
		}
	}
	// A revision on the older daemon could schedule unsafe maintenance.
	out := f.run(dir, "", "--dir", dir, "revise", "lifecycle", "upgrade fact", "must not be written")
	if out.code == 0 || !strings.Contains(out.stderr, daemon.ErrMaintenancePolicyUnsupported.Error()) {
		t.Fatalf("legacy daemon accepted a maintenance-triggering revision: %+v", out)
	}
	if read("upgrade fact").IsSuperseded() {
		t.Fatal("rejected revision retired its target")
	}
	mcp := f.startMCP(dir, f.bin, []string{"--dir", dir, "mcp", "serve"}, nil)
	for i, action := range []string{"pin", "unpin", "forget"} {
		result := mcp.request(3+i, "tools/call", map[string]any{
			"name": "memory_reflect", "arguments": map[string]any{"agent_id": "lifecycle", "action": action, "target": "mcp fact"},
		})
		payload := confidenceStructured(t, result)
		if payload["ok"] != true {
			t.Fatalf("legacy MCP %s failed: %s", action, result)
		}
		fact := read("mcp fact")
		if fact.Pinned != (action == "pin") || fact.IsSuperseded() != (action == "forget") {
			t.Fatalf("legacy MCP %s did not persist: %+v", action, fact)
		}
	}
	for i, args := range []map[string]any{
		{"agent_id": "lifecycle", "text": "must not land", "confidence": "verified"},
		{"agent_id": "lifecycle", "action": "update", "target": "upgrade fact", "text": "must not land"},
	} {
		tool := "memory_add"
		if i == 1 {
			tool = "memory_reflect"
		}
		result := mcp.requestAllowToolError(6+i, "tools/call", map[string]any{"name": tool, "arguments": args})
		var response map[string]any
		if err := json.Unmarshal(result, &response); err != nil || response["isError"] != true || !bytes.Contains(result, []byte(daemon.ErrMaintenancePolicyUnsupported.Error())) {
			t.Fatalf("legacy MCP daemon accepted a maintenance-triggering write: %s / %v", result, err)
		}
	}
	mcp.close()
	all, err := store.List("lifecycle")
	if err != nil || len(all) != 3 {
		t.Fatalf("rejected requests wrote facts: %+v / %v", all, err)
	}
	if reopens != 0 {
		t.Fatal("semantic rejection restarted the old daemon")
	}
	oldProcess.stop()
	newProcess := confidenceStartDaemon(t, f.bin, dir)
	defer newProcess.stop()
	var upgrade memory.Fact
	for _, fact := range facts {
		if fact.Text == "upgrade fact" {
			upgrade = fact // intentionally predates the verified metadata update
		}
	}
	if err := store.SetPinned("lifecycle", true, upgrade); err != nil {
		t.Fatal(err)
	}
	if reopens != 1 || !read("upgrade fact").Pinned {
		t.Fatalf("lifecycle did not renegotiate after daemon upgrade: reopens=%d", reopens)
	}
}
