package export

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	names := BuildFactNoteNames(facts)

	// Write one note per fact.
	for _, f := range facts {
		if err := writeObsidianNote(outDir, f, names[f.ID]); err != nil {
			return err
		}
	}

	// Write _index.md with backlinks grouped by agent.
	return writeObsidianIndex(outDir, facts, names)
}

// AgentDirName is the single authority for the per-agent subdirectory name
// inside an Obsidian vault. Every writer and every link to a fact note must
// go through it.
func AgentDirName(agentID string) string {
	return sanitiseFilename(agentID)
}

// BuildFactNoteNames derives the note filename (without .md) for every fact.
// Names come from the fact text so the Obsidian graph view and quick
// switcher show knowledge instead of opaque ULIDs. The mapping is
// deterministic for a given fact slice and collision-free: identical or
// slug-less texts get a short-ID suffix, and any residual clash is resolved
// with a counter suffix.
func BuildFactNoteNames(facts []memory.Fact) map[string]string {
	used := make(map[string]bool, len(facts))
	names := make(map[string]string, len(facts))
	for _, f := range facts {
		base := slugifyFactText(f.Text)
		name := base
		if base == "" {
			// Blank slugs always carry the short ID: a bare "fact.md" says
			// nothing and invites pointless collisions between agents.
			name = "fact-" + shortID(f.ID)
		} else if used[name] {
			name = base + "-" + shortID(f.ID)
		}
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%s-%d", base, shortID(f.ID), n)
		}
		used[name] = true
		names[f.ID] = name
	}
	return names
}

// slugifyFactText turns fact text into a readable, filesystem- and
// Obsidian-safe slug: only alphanumerics, '-', and '_' survive; everything
// else collapses to '-'. The result is clamped to 64 runes without splitting
// multi-byte characters.
func slugifyFactText(text string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r == '\'' || r == '’':
			return -1 // drop apostrophes: "didn't" -> "didnt"
		case ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9'):
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(text))
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	r := []rune(s)
	if len(r) > 64 {
		r = r[:64]
	}
	return strings.Trim(string(r), "-._")
}

func shortID(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return strings.ToLower(id)
}

func writeObsidianNote(outDir string, f memory.Fact, noteName string) error {
	agentDir := filepath.Join(outDir, AgentDirName(f.AgentID))
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

	filename := fmt.Sprintf("%s.md", noteName)
	return os.WriteFile(filepath.Join(agentDir, filename), []byte(sb.String()), 0o644)
}

func writeObsidianIndex(outDir string, facts []memory.Fact, names map[string]string) error {
	byAgent := make(map[string][]memory.Fact)
	for _, f := range facts {
		byAgent[f.AgentID] = append(byAgent[f.AgentID], f)
	}

	// Sorted agent sections keep the index byte-identical across runs on the
	// same store — map iteration would scramble it.
	agents := make([]string, 0, len(byAgent))
	for agentID := range byAgent {
		agents = append(agents, agentID)
	}
	sort.Strings(agents)

	var sb strings.Builder
	sb.WriteString("---\ntags: [graymatter, index]\n---\n\n")
	sb.WriteString("# GrayMatter Memory Index\n\n")
	sb.WriteString(fmt.Sprintf("_%d total facts across %d agents_\n\n", len(facts), len(agents)))

	for _, agentID := range agents {
		agentFacts := byAgent[agentID]
		sb.WriteString(fmt.Sprintf("## %s (%d facts)\n\n", agentID, len(agentFacts)))
		for _, f := range agentFacts {
			sb.WriteString(fmt.Sprintf("- [[%s/%s|%s]]\n",
				AgentDirName(agentID), names[f.ID], truncatePreview(f.Text, 80)))
		}
		sb.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outDir, "_index.md"), []byte(sb.String()), 0o644)
}

// truncatePreview shortens s to at most max runes, rune-safe so multi-byte
// text never splits mid-character.
func truncatePreview(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}
