package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// entityNoteFilename mirrors kg.sanitizeFilename for entity note filenames.
func entityNoteFilename(label string) string {
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "-", "\"", "-", "<", "-", ">", "-", "|", "-", " ", "_").Replace(label)
}

// linkFactNotesToEntities appends an "## Entities" section with wikilinks to
// every exported fact note, closing the bidirectional fact<->entity layer:
// the Obsidian graph view then draws facts AND entities as one connected
// web. Idempotent — a note already carrying the section is left untouched.
func linkFactNotesToEntities(outDir string, facts []memory.Fact) error {
	for _, f := range facts {
		path := filepath.Join(outDir, entityNoteFilename(f.AgentID), f.ID+".md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue // fact was superseded away or filtered: nothing to link
		}
		content := string(data)
		if strings.Contains(content, "## Entities") {
			continue
		}
		targets := kg.EntityWikilinkTargets(f.Text)
		if len(targets) == 0 {
			continue
		}
		var section strings.Builder
		section.WriteString("\n## Entities\n")
		for _, t := range targets {
			section.WriteString("- [[" + entityNoteFilename(t) + "]]\n")
		}
		trimmed := strings.TrimRight(content, "\n") + "\n"
		if err := os.WriteFile(path, []byte(trimmed+section.String()), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}
