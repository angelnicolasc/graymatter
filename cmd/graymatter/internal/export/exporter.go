// Package export writes GrayMatter memories to human-readable files.
// Three formats are supported: plain markdown, Obsidian vault, and JSON.
package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/lineageview"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Format names the output format for an export run.
type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatObsidian Format = "obsidian"
	FormatJSON     Format = "json"
)

// Exporter writes a slice of facts to an output directory.
type Exporter interface {
	Export(facts []memory.Fact, outDir string) error
}

// New returns an Exporter for the given format.
func New(f Format) (Exporter, error) {
	switch f {
	case FormatMarkdown:
		return &MarkdownExporter{}, nil
	case FormatObsidian:
		return &ObsidianExporter{}, nil
	case FormatJSON:
		return &JSONExporter{}, nil
	default:
		return nil, fmt.Errorf("unknown export format: %q (valid: markdown, obsidian, json)", f)
	}
}

// JSONExporter writes all facts as a single pretty-printed JSON array.
type JSONExporter struct{}

func (e *JSONExporter) Export(facts []memory.Fact, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "memories.json"), data, 0o644)
}

// WriteLineageArchive places the audit archive after the complete file has been
// encoded. The temporary file keeps the same restrictive permissions on rename.
func WriteLineageArchive(archive memory.LineageArchive, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(outDir, ".memory-lineage-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	writeErr := enc.Encode(archive)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(outDir, "memory-lineage.json"))
}

func writeFactLineage(sb *strings.Builder, heading string, p memory.LineageProjection) {
	lines := lineageview.Lines(p)
	if len(lines) == 0 {
		return
	}
	sb.WriteString(heading)
	for _, line := range lines {
		fmt.Fprintf(sb, "- %s\n", line)
	}
}
