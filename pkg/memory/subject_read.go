package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// The subject fence covers the complete retrieval, including multi-query
// selection callbacks. Comparing exact heads prevents two different IDs from
// representing successive versions of one subject in a single result.
func (s *Store) subjectReadFence(ctx context.Context, namespaces []string) (map[string][sha256.Size]byte, error) {
	out := make(map[string][sha256.Size]byte, len(namespaces))
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, ns := range namespaces {
			if err := ctx.Err(); err != nil {
				return err
			}
			view := s.subjectReadView(tx, ns)
			if view.indexErr != nil {
				return view.indexErr
			}
			out[ns] = view.fingerprint
		}
		return nil
	})
	return out, err
}

func (s *Store) checkSubjectReadFenceTx(tx *bolt.Tx, fence map[string][sha256.Size]byte) (map[string]subjectReadView, error) {
	out := make(map[string]subjectReadView, len(fence))
	for ns, before := range fence {
		view := s.subjectReadView(tx, ns)
		if view.indexErr != nil {
			return nil, view.indexErr
		}
		if view.fingerprint != before {
			return nil, fmt.Errorf("subject head changed during retrieval; retry the query")
		}
		out[ns] = view
	}
	return out, nil
}

// Retained evidence participates in prefix uniqueness even after its
// canonical row has been pruned. A displayed prefix never selects a different
// fact solely because the original fact has retired.
func namespaceFactIDsTx(tx *bolt.Tx, namespace string) ([]string, error) {
	facts, err := canonicalFactsTx(tx, namespace)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(facts))
	for _, f := range facts {
		ids[f.ID] = true
	}
	b, err := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	if err != nil {
		return nil, err
	}
	if b != nil {
		if err := b.ForEach(func(key, _ []byte) error {
			entry, err := evidenceEntry(b, string(key))
			if err != nil {
				if ref, damaged := damagedSubjectSnapshotTx(tx, namespace, string(key)); damaged {
					ids[ref.FactID] = true
					return nil
				}
				return err
			}
			if entry.Snapshot.Namespace != namespace || entry.Snapshot.FactID == "" {
				return fmt.Errorf("target evidence namespace or identity mismatch")
			}
			ids[entry.Snapshot.FactID] = true
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func shortFactIDs(ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	for i, id := range ids {
		n := 8
		for _, j := range []int{i - 1, i + 1} {
			if j < 0 || j >= len(ids) {
				continue
			}
			k := 0
			for k < len(id) && k < len(ids[j]) && id[k] == ids[j][k] {
				k++
			}
			if k+1 > n {
				n = k + 1
			}
		}
		if n > len(id) {
			n = len(id)
		}
		// IDs may be opaque UTF-8 strings. Keep a complete rune boundary.
		for n < len(id) && id[n]&0xc0 == 0x80 {
			n++
		}
		out[id] = id[:n]
	}
	return out
}

func (s *Store) projectSubjectSearch(ctx context.Context, fence map[string][sha256.Size]byte, namespaces []string, result *ScopedSearchResult, selected []Fact) error {
	return s.db.View(func(tx *bolt.Tx) error {
		views, err := s.checkSubjectReadFenceTx(tx, fence)
		if err != nil {
			return err
		}
		var issues SubjectIssues
		for _, ns := range namespaces {
			view := views[ns]
			issues.merge(view.issues)
			if view.prefixErr != nil {
				return view.prefixErr
			}
		}
		result.SubjectIssues = issues.nonempty()
		facts := result.Facts
		for i := range facts {
			if err := ctx.Err(); err != nil {
				return err
			}
			f := selected[i]
			facts[i].Subjects = append([]SubjectProjection(nil), views[f.AgentID].byVersion[NewEvidenceSnapshot(f).key()]...)
			facts[i].ShortID = views[f.AgentID].shortID(tx, f.AgentID, f.ID)
			if facts[i].ShortID == "" {
				return fmt.Errorf("selected fact ID disappeared during retrieval")
			}
			facts[i].AgeDays = s.now().Sub(f.CreatedAt).Hours() / 24
			if facts[i].AgeDays < 0 {
				facts[i].AgeDays = 0
			}
		}
		return nil
	})
}

func subjectHookSuffix(subjects []SubjectProjection) string {
	if len(subjects) == 0 {
		return ""
	}
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].Name < subjects[j].Name })
	data, _ := json.Marshal(subjects)
	return " [subjects=" + string(data) + "]"
}
