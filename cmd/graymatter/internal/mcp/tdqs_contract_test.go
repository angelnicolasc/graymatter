package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The tools/list payload is the only part of an MCP server an agent (and any
// registry-quality scorer such as Glama's TDQS) ever reads when deciding
// whether and how to call a tool. These tests pin that payload's structure so
// description edits cannot silently regress the qualities TDQS scores: a
// meaningful title, a front-loaded purpose, explicit usage guidance with named
// sibling alternatives, behavioral disclosure (returns and empty/error cases),
// and full per-parameter schema documentation. See ADR-012 for the rubric
// mapping. TestToolAnnotations pins the annotation half of the same payload.

type toolDef struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

func listToolDefs(t *testing.T) map[string]toolDef {
	t.Helper()
	s, _ := newTestServer(t)

	req := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), req))
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}

	var resp struct {
		Result struct {
			Tools []toolDef `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode tools/list response: %v\n%s", err, raw)
	}

	byName := make(map[string]toolDef, len(resp.Result.Tools))
	for _, tool := range resp.Result.Tools {
		if _, dup := byName[tool.Name]; dup {
			t.Fatalf("duplicate tool name %q in tools/list", tool.Name)
		}
		byName[tool.Name] = tool
	}
	return byName
}

// TestToolDefinitionContract pins the client-visible definition of every tool:
// name set, title quality, and the structural properties of the description
// that TDQS rewards. Content stays free to evolve; the shape may not drift.
func TestToolDefinitionContract(t *testing.T) {
	byName := listToolDefs(t)

	wantNames := []string{"memory_search", "memory_add", "checkpoint_save", "checkpoint_resume", "memory_reflect"}
	if len(byName) != len(wantNames) {
		t.Fatalf("got %d tools, want %d", len(byName), len(wantNames))
	}
	for _, name := range wantNames {
		if _, ok := byName[name]; !ok {
			t.Errorf("tool %q missing from tools/list", name)
		}
	}
	if t.Failed() {
		t.Fatal("tool set drifted; fix the set before updating this contract")
	}

	// startVerb: TDQS Purpose Clarity 5/5 requires a specific verb front-loaded
	// in the description. Each tool owns exactly one opening verb.
	startVerb := map[string]string{
		"memory_search":     "Search",
		"memory_add":        "Store",
		"checkpoint_save":   "Persist",
		"checkpoint_resume": "Read",
		"memory_reflect":    "Curate",
	}
	// sibling: TDQS Usage Guidelines 5/5 requires naming the alternative tool
	// for the cases this tool excludes. Each entry must appear verbatim.
	sibling := map[string]string{
		"memory_search":     "memory_add",
		"memory_add":        "memory_reflect",
		"checkpoint_save":   "checkpoint_resume",
		"checkpoint_resume": "checkpoint_save",
		"memory_reflect":    "memory_add",
	}
	// sharedToken: load-bearing vocabulary a description must not lose.
	sharedToken := map[string]string{
		"memory_search":     "__shared__",
		"checkpoint_resume": "checkpoint_save",
	}

	for _, name := range wantNames {
		tool := byName[name]

		// Title: TDQS context signal titleIsMeaningful = title exists,
		// differs from the name, and is longer than the name.
		if tool.Title == "" {
			t.Errorf("%s: empty title", name)
		} else if tool.Title == name || len(tool.Title) <= len(name) {
			t.Errorf("%s: title %q must differ from the name and be longer", name, tool.Title)
		}

		// Description: present, bounded, front-loaded verb, terminal period.
		desc := tool.Description
		if strings.TrimSpace(desc) == "" {
			t.Errorf("%s: empty description", name)
			continue
		}
		if utf8.RuneCountInString(desc) > 800 {
			t.Errorf("%s: description is %d chars, want <= 800 (conciseness budget)", name, utf8.RuneCountInString(desc))
		}
		if !strings.HasPrefix(desc, startVerb[name]) {
			t.Errorf("%s: description must start with %q, got %q", name, startVerb[name], firstWord(desc))
		}
		if !strings.HasSuffix(desc, ".") {
			t.Errorf("%s: description must end with a period", name)
		}

		// Anti-tautology: TDQS caps Purpose at 2 when the description merely
		// restates the name or title.
		norm := strings.ToLower(strings.TrimSpace(desc))
		if norm == strings.ToLower(name) || norm == strings.ToLower(tool.Title) {
			t.Errorf("%s: description is tautological", name)
		}

		// Usage guidance: an explicit when-to-use cue, and the sibling tool
		// named as the boundary.
		if !strings.Contains(strings.ToLower(desc), "use") {
			t.Errorf("%s: description carries no when-to-use guidance", name)
		}
		if !strings.Contains(desc, sibling[name]) {
			t.Errorf("%s: description must name sibling %q as the alternative", name, sibling[name])
		}
		if tok, ok := sharedToken[name]; ok && !strings.Contains(desc, tok) {
			t.Errorf("%s: description must mention %q", name, tok)
		}

		// Behavioral transparency: with annotations already carrying the safety
		// hints, the description must add what they cannot express — what comes
		// back, and what happens on the empty path.
		if !strings.Contains(desc, "Returns") && !strings.Contains(desc, "Errors") {
			t.Errorf("%s: description must disclose its return or error behavior", name)
		}
	}
}

// TestToolSchemaContract pins the input schemas: exact parameter sets, exact
// required lists, 100% per-parameter description coverage (TDQS schema
// coverage signal), the memory_reflect action enum, and the top_k default.
func TestToolSchemaContract(t *testing.T) {
	byName := listToolDefs(t)

	type paramSpec struct {
		props    []string
		required []string
	}
	want := map[string]paramSpec{
		"memory_search":     {props: []string{"agent_id", "query", "top_k"}, required: []string{"agent_id", "query"}},
		"memory_add":        {props: []string{"agent_id", "text"}, required: []string{"agent_id", "text"}},
		"checkpoint_save":   {props: []string{"agent_id", "state"}, required: []string{"agent_id"}},
		"checkpoint_resume": {props: []string{"agent_id"}, required: []string{"agent_id"}},
		"memory_reflect":    {props: []string{"action", "agent", "agent_id", "text", "target"}, required: []string{"action"}},
	}

	for name, spec := range want {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("tool %q missing", name)
		}

		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
				Enum        []any  `json:"enum"`
				Default     any    `json:"default"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s: decode inputSchema: %v\n%s", name, err, tool.InputSchema)
		}

		if len(schema.Properties) != len(spec.props) {
			t.Errorf("%s: got %d properties, want %d", name, len(schema.Properties), len(spec.props))
		}
		for _, p := range spec.props {
			prop, ok := schema.Properties[p]
			if !ok {
				t.Errorf("%s: property %q missing", name, p)
				continue
			}
			if strings.TrimSpace(prop.Description) == "" {
				t.Errorf("%s.%s: schema description is empty (coverage must stay 100%%)", name, p)
			}
		}

		if len(schema.Required) != len(spec.required) {
			t.Errorf("%s: required = %v, want %v", name, schema.Required, spec.required)
		}
		gotReq := map[string]bool{}
		for _, r := range schema.Required {
			gotReq[r] = true
		}
		for _, r := range spec.required {
			if !gotReq[r] {
				t.Errorf("%s: required parameter %q missing", name, r)
			}
		}

		t.Run(name, func(t *testing.T) {
			switch name {
			case "memory_search":
				if d, ok := schema.Properties["top_k"].Default.(float64); !ok || d != 8 {
					t.Errorf("memory_search.top_k default = %v, want 8", schema.Properties["top_k"].Default)
				}
			case "memory_reflect":
				wantEnum := []string{"add", "update", "forget", "link", "pin", "unpin"}
				got := schema.Properties["action"].Enum
				if len(got) != len(wantEnum) {
					t.Fatalf("memory_reflect.action enum = %v, want %v", got, wantEnum)
				}
				for i, v := range wantEnum {
					if got[i] != v {
						t.Errorf("memory_reflect.action enum[%d] = %v, want %q", i, got[i], v)
					}
				}
			}
		})
	}
}

func firstWord(s string) string {
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i]
	}
	return s
}
