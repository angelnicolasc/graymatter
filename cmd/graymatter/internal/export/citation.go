package export

import (
	"encoding/json"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// JSON strings preserve CRLF and controls without letting the declared quote
// create Markdown headings or close a fence. No quote is shortened.
func writeFactCitation(sb *strings.Builder, fact memory.Fact) error {
	if fact.CitationUnavailable {
		sb.WriteString("\n### Literal evidence\n\n_Exact citation unavailable for this fact version._\n")
		return nil
	}
	if fact.Evidence == nil {
		return nil
	}
	encoded, err := json.Marshal(fact.Evidence)
	if err != nil {
		return err
	}
	sb.WriteString("\n### Literal evidence\n\n_Declared, untrusted data; captured_at records receipt, not verification._\n\n    ")
	sb.Write(encoded)
	sb.WriteString("\n")
	return nil
}
