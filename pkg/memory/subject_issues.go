package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// SubjectIssue identifies an unresolved subject's recorded anchor. The anchor
// ID is diagnostic, not a claim that the canonical fact still exists.
type SubjectIssue struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	HeadFactID string `json:"head_fact_id"`
}

// SubjectIssues gives the total and at most three names in a read snapshot.
type SubjectIssues struct {
	Subjects []SubjectIssue `json:"subjects"`
	Total    int            `json:"total"`
}

func (issues *SubjectIssues) Warning() string {
	if issues == nil || issues.Total == 0 {
		return ""
	}
	names := make([]string, 0, len(issues.Subjects))
	for _, issue := range issues.Subjects {
		names = append(names, issue.Name)
	}
	encoded, _ := json.Marshal(names)
	return fmt.Sprintf("Unresolved subjects: %s (%d total). Recover with memory_add using subject and replaces: [fact ID].", encoded, issues.Total)
}

func (issues *SubjectIssues) add(namespace string, idx subjectIndex) {
	var keys []string
	for key, head := range idx.Heads {
		if head.Unresolved != nil {
			keys = append(keys, key)
		}
	}
	issues.Total += len(keys)
	sort.Strings(keys)
	for _, key := range keys {
		if len(issues.Subjects) == 3 {
			break
		}
		head := idx.Heads[key].Head
		issues.Subjects = append(issues.Subjects, SubjectIssue{
			Namespace: namespace, Name: head.Name, HeadFactID: head.Reference.FactID,
		})
	}
}

func (issues *SubjectIssues) merge(other SubjectIssues) {
	issues.Total += other.Total
	n := min(len(other.Subjects), 3-len(issues.Subjects))
	issues.Subjects = append(issues.Subjects, other.Subjects[:n]...)
}

func (issues *SubjectIssues) nonempty() *SubjectIssues {
	if issues.Total == 0 {
		return nil
	}
	return issues
}

// SubjectIssues inspects only the requested namespaces without changing facts,
// metadata, or access counters. Repeated namespaces are counted once.
func (s *Store) SubjectIssues(ctx context.Context, namespaces []string) (SubjectIssues, error) {
	var out SubjectIssues
	err := s.db.View(func(tx *bolt.Tx) error {
		seen := make(map[string]bool, len(namespaces))
		for _, namespace := range namespaces {
			if err := ctx.Err(); err != nil {
				return err
			}
			if seen[namespace] {
				continue
			}
			seen[namespace] = true
			view := s.subjectReadView(tx, namespace)
			if view.indexErr != nil {
				return view.indexErr
			}
			out.merge(view.issues)
		}
		return ctx.Err()
	})
	return out, err
}

// SubjectIssuesByNamespace includes namespaces whose last canonical fact was
// pruned by an earlier writer, using the subject buckets as the inventory.
func (s *Store) SubjectIssuesByNamespace(ctx context.Context) (map[string]SubjectIssues, error) {
	out := make(map[string]SubjectIssues)
	err := s.db.View(func(tx *bolt.Tx) error {
		return walkLineageNamespaces(tx.Bucket(bucketSubjectHeads), "", func(namespace string, _ *bolt.Bucket) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			view := s.subjectReadView(tx, namespace)
			if view.indexErr != nil {
				return view.indexErr
			}
			if view.issues.Total != 0 {
				out[namespace] = SubjectIssues{
					Subjects: append([]SubjectIssue(nil), view.issues.Subjects...),
					Total:    view.issues.Total,
				}
			}
			return nil
		})
	})
	return out, err
}
