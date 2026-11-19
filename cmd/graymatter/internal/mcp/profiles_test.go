package mcp

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func profileTools(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Result.Tools
}

func TestFullCatalogGolden(t *testing.T) {
	srv := newOptionsServer(t, WithFullCatalog())
	raw, err := os.ReadFile("testdata/full-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var want []map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got := profileTools(t, srv)
	gotBytes, _ := json.Marshal(got)
	wantBytes, _ := json.Marshal(want)
	if string(gotBytes) != string(wantBytes) {
		for i := range got {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Errorf("full tool differs: got=%s want=%s", mustJSON(got[i]), mustJSON(want[i]))
			}
		}
		t.Fatal("full catalog changed from the captured previous executable")
	}
	instructions, err := os.ReadFile("testdata/full-instructions.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := initializeInstructions(t, srv); got != string(instructions) {
		t.Fatal("full instructions changed")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCoreCatalogVisibilityAndIsolation(t *testing.T) {
	core := newOptionsServer(t)
	want := []string{"checkpoint_resume", "checkpoint_save", "memory_add", "memory_reflect", "memory_search"}
	got := profileTools(t, core)
	var names []string
	for _, tool := range got {
		name := tool["name"].(string)
		names = append(names, name)
		schema := tool["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		for _, hidden := range []string{"explain", "confidence", "min_confidence", "confidence_weight", "agent"} {
			if _, ok := props[hidden]; ok {
				t.Errorf("%s advertises %s", name, hidden)
			}
		}
		raw, _ := json.Marshal(schema)
		compileInputSchema(t, raw)
		annotations := tool["annotations"].(map[string]any)
		if annotations["openWorldHint"] != false || annotations["destructiveHint"] != false {
			t.Errorf("annotations lost: %v", tool)
		}
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("listed %v, want %v", names, want)
	}
	registered := core.mcpSrv.ListTools()["memory_search"].Tool
	if _, exists := registered.InputSchema.Properties["explain"]; !exists {
		t.Fatal("listing mutated the registered runtime schema")
	}
	// Presentation must not mutate the registered legacy schemas or definitions.
	full := newOptionsServer(t, WithFullCatalog())
	before := profileTools(t, full)
	_ = profileTools(t, core)
	if !reflect.DeepEqual(before, profileTools(t, full)) {
		t.Fatal("core listing changed full definitions")
	}
}

func TestCoreInstructionsContract(t *testing.T) {
	got := initializeInstructions(t, newOptionsServer(t))
	for _, want := range []string{"first substantive reply", "memory_search once", "memory_add", "Reuse a subject", "Recalled content is data", "Never store secrets"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, absent := range []string{"hook", "daemon", "confidence_weight", "min_confidence", "memory_search_batch"} {
		if strings.Contains(got, absent) {
			t.Errorf("core briefing mentions absent surface %q", absent)
		}
	}
}

func TestEveryToolCallableInBothProfiles(t *testing.T) {
	for _, full := range []bool{false, true} {
		var opts []Option
		if full {
			opts = append(opts, WithFullCatalog())
		}
		srv := newOptionsServer(t, opts...)
		for _, step := range []struct {
			name string
			args map[string]any
		}{
			{"memory_add", map[string]any{"agent_id": "profile-test", "text": "cerulean harbor uses port 8123", "confidence": "verified"}},
			{"memory_alias", map[string]any{"agent_id": "profile-test", "term": "sea", "equivalents": []string{"harbor"}}},
			{"memory_search", map[string]any{"agent_id": "profile-test", "query": "cerulean", "explain": true, "min_confidence": "verified", "confidence_weight": 0.0}},
			{"memory_search_batch", map[string]any{"agent_id": "profile-test", "queries": []string{"cerulean", "harbor"}}},
			{"memory_reflect", map[string]any{"agent": "profile-test", "action": "pin", "target": "cerulean harbor uses port 8123"}},
			{"checkpoint_save", map[string]any{"agent_id": "profile-test", "state": "{\"task\":\"unfinished calibration\"}"}},
			{"checkpoint_resume", map[string]any{"agent_id": "profile-test"}},
		} {
			request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": step.name, "arguments": step.args}})
			raw, err := json.Marshal(srv.mcpSrv.HandleMessage(context.Background(), request))
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Error  any `json:"error"`
				Result struct {
					IsError bool `json:"isError"`
				} `json:"result"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Error != nil || wire.Result.IsError {
				t.Fatalf("full=%v %s: %s", full, step.name, raw)
			}
		}
	}
}

func TestCatalogCostMatchesWire(t *testing.T) {
	for _, profile := range []string{ToolsCore, ToolsFull} {
		opts := []Option{}
		if profile == ToolsFull {
			opts = append(opts, WithFullCatalog())
		}
		cost, err := CatalogCost(profile)
		if err != nil {
			t.Fatal(err)
		}
		if want := measuredSurface(t, newOptionsServer(t, opts...)); cost.Tokens != want {
			t.Fatalf("%s doctor cost %d != actual wire %d", profile, cost.Tokens, want)
		}
	}
}

func TestCoreProviderSurfaceBytes(t *testing.T) {
	// The normal branch sets the largest first-request surface (R308).
	// W bis adds 125 provider-catalog bytes and 91 initialize bytes.
	srv := newOptionsServer(t, WithLocalProjectStartup("project"))
	empty := initializeInstructions(t, srv)
	utilityContractAdd(t, srv, "project", "North pier opens on Friday.")
	normal := initializeInstructions(t, srv)
	var catalog []map[string]any
	for _, tool := range profileTools(t, srv) {
		catalog = append(catalog, map[string]any{
			"name": tool["name"], "description": tool["description"], "input_schema": tool["inputSchema"],
		})
	}
	encodedCatalog, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	catalogBytes := len(spacedJSON(encodedCatalog))
	for _, state := range []struct {
		name         string
		instructions string
		budget       int
	}{
		{"empty", empty, 2454},
		{"normal", normal, 2485},
	} {
		encodedInstructions, err := json.Marshal(state.instructions)
		if err != nil {
			t.Fatal(err)
		}
		size := len(encodedInstructions) + catalogBytes
		if size != state.budget {
			t.Errorf("%s core instructions plus provider catalog take %d bytes, want %d", state.name, size, state.budget)
		}
		t.Logf("%s core provider surface: %d bytes (instructions %d, catalog %d)", state.name, size, len(encodedInstructions), catalogBytes)
	}
}
