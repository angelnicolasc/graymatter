package export

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// MarkdownExporter writes one .md file per agent with all their facts listed.
type MarkdownExporter struct{}

func (e *MarkdownExporter) Export(facts []memory.Fact, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Group by agent.
	byAgent := make(map[string][]memory.Fact)
	for _, f := range facts {
		byAgent[f.AgentID] = append(byAgent[f.AgentID], f)
	}

	for agentID, agentFacts := range byAgent {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("# %s\n\n", agentID))
		sb.WriteString(fmt.Sprintf("_%d facts_\n\n", len(agentFacts)))
		sb.WriteString("_Lineage scope · fact projections. Retained evidence is available in the complete JSON audit export._\n\n")
		sb.WriteString("---\n\n")
		for _, f := range agentFacts {
			sb.WriteString(fmt.Sprintf("## %s\n\n", f.CreatedAt.Format("2006-01-02 15:04:05")))
			sb.WriteString(f.Text + "\n\n")
			sb.WriteString(fmt.Sprintf(
				"_weight: %.3f · accessed: %d times_\n\n",
				f.Weight, f.AccessCount,
			))
			writeFactLineage(&sb, "", f.LineageProjection)
			sb.WriteString("\n---\n\n")
		}
		name := sanitiseFilename(agentID) + ".md"
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(sb.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func sanitiseFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}
