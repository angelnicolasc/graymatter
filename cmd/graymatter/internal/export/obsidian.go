package export

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// ObsidianExporter writes one .md file per fact with YAML frontmatter,
// plus a _index.md that links to all facts. The output is a valid Obsidian vault.
type ObsidianExporter struct{}

func (e *ObsidianExporter) Export(facts []memory.Fact, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Write one note per fact.
	for _, f := range facts {
		if err := writeObsidianNote(outDir, f); err != nil {
			return err
		}
	}

	// Write _index.md with backlinks grouped by agent.
	return writeObsidianIndex(outDir, facts)
}

func writeObsidianNote(outDir string, f memory.Fact) error {
	agentDir := filepath.Join(outDir, sanitiseFilename(f.AgentID))
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return err
	}

	var sb strings.Builder
	// YAML frontmatter.
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("id: %s\n", f.ID))
	sb.WriteString(fmt.Sprintf("agent: %s\n", f.AgentID))
	sb.WriteString(fmt.Sprintf("created: %s\n", f.CreatedAt.Format("2006-01-02T15:04:05Z")))
	sb.WriteString(fmt.Sprintf("accessed: %s\n", f.AccessedAt.Format("2006-01-02T15:04:05Z")))
	sb.WriteString(fmt.Sprintf("access_count: %d\n", f.AccessCount))
	sb.WriteString(fmt.Sprintf("weight: %.4f\n", f.Weight))
	if f.Confidence != "" {
		sb.WriteString(fmt.Sprintf("confidence: %s\n", f.Confidence))
	}
	sb.WriteString(fmt.Sprintf("tags:\n  - graymatter\n  - %s\n", sanitiseFilename(f.AgentID)))
	sb.WriteString("---\n\n")
	sb.WriteString(f.Text + "\n")

	filename := fmt.Sprintf("%s.md", f.ID)
	return os.WriteFile(filepath.Join(agentDir, filename), []byte(sb.String()), 0o644)
}

func writeObsidianIndex(outDir string, facts []memory.Fact) error {
	byAgent := make(map[string][]memory.Fact)
	for _, f := range facts {
		byAgent[f.AgentID] = append(byAgent[f.AgentID], f)
	}

	var sb strings.Builder
	sb.WriteString("---\ntags: [graymatter, index]\n---\n\n")
	sb.WriteString("# GrayMatter Memory Index\n\n")
	sb.WriteString(fmt.Sprintf("_%d total facts across %d agents_\n\n", len(facts), len(byAgent)))

	for agentID, agentFacts := range byAgent {
		sb.WriteString(fmt.Sprintf("## %s (%d facts)\n\n", agentID, len(agentFacts)))
		for _, f := range agentFacts {
			preview := f.Text
			if len(preview) > 80 {
				preview = preview[:77] + "..."
			}
			sb.WriteString(fmt.Sprintf("- [[%s/%s|%s]]\n", sanitiseFilename(agentID), f.ID, preview))
		}
		sb.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outDir, "_index.md"), []byte(sb.String()), 0o644)
}
