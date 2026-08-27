package mcp

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TD-002 guard. mcp-go's WithOutputSchema swallows generation failures to
// stderr, which would publish a tool without its declared contract. Our
// wrapper fails fast instead; its panic path is effectively unreachable with
// reflectable Go types (jsonschema reflection handles even chan/func/complex
// fields — verified empirically), so this test pins the reachable half: every
// result type in this package generates a valid object schema, and the helper
// is a drop-in ToolOption.
func TestOutputSchemaOfGeneratesForEveryResultType(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  mcp.ToolOption
	}{
		{"memory_search", outputSchemaOf[searchResult]()},
		{"memory_add", outputSchemaOf[addResult]()},
		{"checkpoint_save", outputSchemaOf[checkpointSaveResult]()},
		{"checkpoint_resume", outputSchemaOf[checkpointResumeResult]()},
		{"memory_reflect", outputSchemaOf[reflectResult]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := mcp.NewTool(tc.name, tc.opt)
			raw, err := json.Marshal(tool)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var wire struct {
				OutputSchema json.RawMessage `json:"outputSchema"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(wire.OutputSchema) == 0 {
				t.Fatalf("%s: generated output schema never reached the wire", tc.name)
			}
		})
	}
}
