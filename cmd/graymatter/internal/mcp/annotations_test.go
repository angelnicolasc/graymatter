package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// TestToolAnnotations pins the hints every tool advertises in tools/list.
//
// This goes through HandleMessage rather than inspecting the Tool structs so it
// asserts on exactly the payload a client receives. It exists because mcp-go's
// NewTool defaults everything to destructive + non-idempotent + open-world: if
// a dependency bump ever drops our annotations, hosts start gating plain
// lookups behind an approval prompt and unattended agents quietly stop calling
// them. That failure is invisible from the outside, so it gets a test.
func TestToolAnnotations(t *testing.T) {
	s, _ := newTestServer(t)

	req := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), req))
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}

	var resp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Annotations struct {
					ReadOnlyHint    *bool `json:"readOnlyHint"`
					DestructiveHint *bool `json:"destructiveHint"`
					IdempotentHint  *bool `json:"idempotentHint"`
					OpenWorldHint   *bool `json:"openWorldHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode tools/list response: %v\n%s", err, raw)
	}

	type hints struct{ readOnly, destructive, idempotent bool }
	want := map[string]hints{
		"memory_search":     {readOnly: true, destructive: false, idempotent: true},
		"checkpoint_resume": {readOnly: true, destructive: false, idempotent: true},
		"memory_add":        {readOnly: false, destructive: false, idempotent: false},
		"checkpoint_save":   {readOnly: false, destructive: false, idempotent: false},
		"memory_reflect":    {readOnly: false, destructive: true, idempotent: false},
	}

	if len(resp.Result.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(resp.Result.Tools), len(want))
	}

	seen := make(map[string]bool, len(want))
	for _, tool := range resp.Result.Tools {
		exp, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q; add it to the annotation table", tool.Name)
			continue
		}
		seen[tool.Name] = true

		a := tool.Annotations
		for _, c := range []struct {
			field string
			got   *bool
			want  bool
		}{
			{"readOnlyHint", a.ReadOnlyHint, exp.readOnly},
			{"destructiveHint", a.DestructiveHint, exp.destructive},
			{"idempotentHint", a.IdempotentHint, exp.idempotent},
			// Every tool reads and writes the local store only. An open world
			// hint would push hosts toward treating them like network calls.
			{"openWorldHint", a.OpenWorldHint, false},
		} {
			if c.got == nil {
				t.Errorf("%s: %s missing from tools/list", tool.Name, c.field)
				continue
			}
			if *c.got != c.want {
				t.Errorf("%s: %s = %v, want %v", tool.Name, c.field, *c.got, c.want)
			}
		}
	}

	for name := range want {
		if !seen[name] {
			t.Errorf("tool %q missing from tools/list", name)
		}
	}
}
