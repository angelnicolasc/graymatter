package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// ResolveFactTargets preserves exact text's historical selection of all copies.
// ID uniqueness is checked before excluding retired rows, so a prefix never
// silently changes meaning when a matching row becomes retired.
func (s *Store) ResolveFactTargets(ctx context.Context, namespace, target, targetType string, includeRetired bool) ([]Fact, error) {
	if strings.TrimSpace(namespace) == "" || target == "" {
		return nil, fmt.Errorf("namespace and target are required")
	}
	if targetType != "" && targetType != "id" && targetType != "text" {
		return nil, fmt.Errorf("target_type must be id or text")
	}
	var out []Fact
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		facts, err := canonicalFactsTx(tx, namespace)
		if err != nil {
			return err
		}
		if targetType != "id" {
			for _, f := range facts {
				if f.Text == target && (includeRetired || !f.IsSuperseded()) {
					current, err := factInTx(tx, namespace, f.ID)
					if err != nil {
						return err
					}
					out = append(out, current)
				}
			}
			if len(out) > 0 {
				return nil
			}
			if targetType == "text" {
				return fmt.Errorf("target text not found")
			}
		}
		f, err := resolveFactIDTx(tx, namespace, target, includeRetired)
		if err != nil {
			return err
		}
		out = []Fact{f}
		return nil
	})
	return out, err
}

func resolveFactIDTx(tx *bolt.Tx, namespace, target string, includeRetired bool) (Fact, error) {
	facts, err := canonicalFactsTx(tx, namespace)
	if err != nil {
		return Fact{}, err
	}
	byID := map[string]Fact{}
	ids := map[string]bool{}
	for _, f := range facts {
		byID[f.ID] = f
		ids[f.ID] = true
	}
	if b, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false); b != nil {
		if err := b.ForEach(func(key, _ []byte) error {
			entry, err := evidenceEntry(b, string(key))
			if err != nil {
				if ref, damaged := damagedSubjectSnapshotTx(tx, namespace, string(key)); damaged {
					ids[ref.FactID] = true
					return nil
				}
				return err
			}
			if entry.Snapshot.Namespace != namespace {
				return fmt.Errorf("target evidence namespace mismatch")
			}
			ids[entry.Snapshot.FactID] = true
			return nil
		}); err != nil {
			return Fact{}, err
		}
	}
	chosen := ""
	if ids[target] {
		chosen = target
	} else {
		var candidates []string
		for id := range ids {
			if strings.HasPrefix(id, target) {
				candidates = append(candidates, id)
			}
		}
		sort.Strings(candidates)
		if len(candidates) > 1 {
			return Fact{}, fmt.Errorf("ambiguous fact ID prefix %q", target)
		}
		if len(candidates) == 1 {
			chosen = candidates[0]
		}
	}
	f, exists := byID[chosen]
	if !exists || !includeRetired && f.IsSuperseded() {
		return Fact{}, fmt.Errorf("target ID %q has no eligible canonical row", target)
	}
	// ID resolution is also a mutation-selection API. Preserve every field
	// checked by sourceVersionMatches; a ranking-only snapshot is insufficient.
	return factInTx(tx, namespace, f.ID)
}
