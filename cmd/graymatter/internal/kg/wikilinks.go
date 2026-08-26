package kg

import "strings"

// EntityWikilinkTargets returns the sanitized note filenames (without .md)
// of every entity extracted from text, so a fact note can link back to its
// entity notes with [[target]] — the bidirectional fact<->entity layer that
// makes the Obsidian graph view connect both sides.
//
// Known limitation: when two entities' labels sanitize to the same name,
// ExportObsidian disambiguates the second one ("A_B-2"); links produced here
// still use the unsuffixed form, so that rare case yields an unresolved
// link in Obsidian rather than a wrong-note merge. Resolving it needs the
// full node list at link time, which this text-only surface deliberately
// does not have.
func EntityWikilinkTargets(text string) []string {
	ex := NewExtractor(ExtractorConfig{})
	nodes, _, err := ex.Extract(text)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		if n.ID == "" {
			continue
		}
		target := SanitizeFilename(n.Label)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

// HasWikilinkTarget reports whether s already contains a [[target]] link.
func HasWikilinkTarget(s, target string) bool {
	return strings.Contains(s, "[["+target+"]]")
}
