package kg

import "strings"

// EntityWikilinkTargets returns the sanitized note filenames (without .md)
// of every entity extracted from text, so a fact note can link back to its
// entity notes with [[target]] — the bidirectional fact<->entity layer that
// makes the Obsidian graph view connect both sides.
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
		target := sanitizeFilename(n.Label)
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
