package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/export"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// linkFactNotesToEntities appends an "## Entities" section with wikilinks to
// every exported fact note, closing the bidirectional fact<->entity layer:
// the Obsidian graph view then draws facts AND entities as one connected
// web. Idempotent — a note already carrying the section is left untouched.
//
// Note naming is delegated entirely to the export package (fact notes) and
// kg.SanitizeFilename (entity notes): this pass owns no naming logic of its
// own, so the writer and the linker can never drift apart again.
func linkFactNotesToEntities(outDir string, facts []memory.Fact) error {
	names := export.BuildFactNoteNames(facts)
	for _, f := range facts {
		path := filepath.Join(outDir, export.AgentDirName(f.AgentID), names[f.ID]+".md")
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
			section.WriteString("- [[" + kg.SanitizeFilename(t) + "]]\n")
		}
		trimmed := strings.TrimRight(content, "\n") + "\n"
		if err := os.WriteFile(path, []byte(trimmed+section.String()), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}
