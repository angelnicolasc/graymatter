// Package lineageview formats version references for human-readable output.
package lineageview

import (
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Lines renders a projected version and its recorded relationships.
// Ambiguous legacy pointers have a separate label.
func Lines(p memory.LineageProjection) []string {
	var lines []string
	if p.LineageStatus != "" {
		lines = append(lines, "Lineage status · "+p.LineageStatus)
	}
	if p.ContentHash != "" {
		lines = append(lines, "Content version · "+p.ContentHash)
	}
	if p.HistoricalHash != "" {
		lines = append(lines, "Historical content version · "+p.HistoricalHash)
	}
	for _, ref := range p.DerivedFrom {
		lines = append(lines, Reference("Derived from", ref))
	}
	for _, ref := range p.Replaces {
		lines = append(lines, Reference("Replaces", ref))
	}
	for _, ref := range p.LegacySources {
		lines = append(lines, Reference("Legacy source (relationship ambiguous)", ref))
	}
	return lines
}

// Reference identifies a version within the caller's namespace.
func Reference(relationship string, ref memory.LineageReference) string {
	if ref.ContentHash == "" {
		return fmt.Sprintf("%s · %s", relationship, ref.FactID)
	}
	return fmt.Sprintf("%s · %s @ %s", relationship, ref.FactID, ref.ContentHash)
}
