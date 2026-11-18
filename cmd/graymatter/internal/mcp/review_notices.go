package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/mark3labs/mcp-go/mcp"
)

func reviewJournalSupported(backend Backend) bool {
	journal, ok := backend.(interface{ ReviewJournalSupported() bool })
	return ok && journal.ReviewJournalSupported()
}

func (b *DirectBackend) ReviewJournalSupported() bool {
	_, ok := b.mem.Advanced().(interface {
		ReviewNotices(context.Context, string, []string) ([]memory.ReviewNotice, error)
	})
	return ok
}

func writeReviewNotices(out *strings.Builder, notices []memory.ReviewNotice) {
	if prose := memory.ReviewNoticesText(notices); prose != "" {
		out.WriteString("\n" + prose + "\n")
	}
}

const reviewNoticesOutputSchema = `{
  "type": "array",
  "minItems": 1,
  "maxItems": 4,
  "items": {
    "type": "object",
    "additionalProperties": false,
    "required": ["conflict_id", "source_ids", "source_state"],
    "properties": {
      "conflict_id": {"type": "string", "maxLength": 64, "pattern": "^[A-Za-z0-9_-]+$"},
      "source_ids": {
        "type": "array",
        "minItems": 1,
        "maxItems": 12,
        "uniqueItems": true,
        "items": {"type": "string", "maxLength": 64, "pattern": "^[A-Za-z0-9_-]+$"}
      },
      "source_state": {"enum": ["current", "retired", "changed", "missing"]}
    }
  }
}`

// Keep the generated contract for existing fields and add the limits the
// Go slice and string types cannot express.
func reviewOutputSchemaOf[T any](expected int) mcp.ToolOption {
	schema := mcp.SchemaFor[T]()
	if got := constrainReviewNoticeFields(schema); got != expected {
		panic(fmt.Sprintf("graymatter/mcp: expected %d conflict notice schema fields, found %d", expected, got))
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("graymatter/mcp: marshal conflict notice schema: %v", err))
	}
	return mcp.WithRawOutputSchema(raw)
}

func constrainReviewNoticeFields(schema map[string]any) int {
	count := 0
	if props, ok := schema["properties"].(map[string]any); ok {
		if _, exists := props["conflict_notices"]; exists {
			props["conflict_notices"] = json.RawMessage(reviewNoticesOutputSchema)
			count++
		}
	}
	for _, value := range schema {
		if child, ok := value.(map[string]any); ok {
			count += constrainReviewNoticeFields(child)
		}
	}
	return count
}
