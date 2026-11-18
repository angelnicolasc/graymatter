package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

const LineageArchiveVersion = 1

// LineageArchive is a versioned audit export of canonical rows and retained
// evidence. Use Backup for a complete database image including journal owners.
type LineageArchive struct {
	Version       int                     `json:"version"`
	Namespace     string                  `json:"namespace,omitempty"`
	Facts         []Fact                  `json:"facts"`
	Evidence      []LineageInspection     `json:"evidence"`
	SubjectHeads  []SubjectHeadInspection `json:"subject_heads,omitempty"`
	SubjectIssues *SubjectIssues          `json:"subject_issues,omitempty"`
}

// ExportLineage reads a namespace, or all namespaces if namespace is empty,
// within one transaction. It exports metadata and source text without vectors.
func (s *Store) ExportLineage(ctx context.Context, namespace string) (LineageArchive, error) {
	out := LineageArchive{
		Version: LineageArchiveVersion, Namespace: namespace,
		Facts: []Fact{}, Evidence: []LineageInspection{},
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := walkLineageNamespaces(tx.Bucket(bucketFacts), namespace, func(ns string, b *bolt.Bucket) error {
			idx, err := resolveSubjectIndexTx(tx, ns)
			if err != nil {
				return err
			}
			return b.ForEach(func(id, raw []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				f, err := unmarshalInspectionFact(raw)
				if err != nil {
					return err
				}
				if f.ID != string(id) || f.AgentID != ns {
					return errors.New("export fact identity mismatch")
				}
				if err := hydrateLineageTx(tx, &f); err != nil {
					return err
				}
				if err := verifyLineageProjectionTx(ctx, tx, &f); err != nil {
					return err
				}
				f.Subjects = append([]SubjectProjection(nil), idx.ByVersion[NewEvidenceSnapshot(f).key()]...)
				out.Facts = append(out.Facts, f)
				return nil
			})
		}); err != nil {
			return err
		}
		if err := walkLineageNamespaces(tx.Bucket(bucketEvidenceSnapshots), namespace, func(ns string, b *bolt.Bucket) error {
			records, _ := evidenceNamespace(tx, bucketLineageRecords, ns, false)
			return b.ForEach(func(key, _ []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				entry, err := evidenceEntry(b, string(key))
				if err != nil {
					if _, damaged := damagedSubjectSnapshotTx(tx, ns, string(key)); damaged {
						return nil
					}
					return err
				}
				if entry.Snapshot.Namespace != ns {
					return errors.New("export snapshot namespace mismatch")
				}
				item := LineageInspection{
					Snapshot:          &entry.Snapshot,
					LineageProjection: LineageProjection{ContentHash: entry.Snapshot.ContentHash},
				}
				item.Evidence, err = citationAtTx(tx, ns, string(key))
				if err != nil {
					return err
				}
				record, found, err := lineageRecordAt(records, ns, string(key))
				if err != nil {
					return err
				}
				if found {
					item.LineageProjection = record.projection()
					item.CapturedAt = record.Captured
				}
				out.Evidence = append(out.Evidence, item)
				return nil
			})
		}); err != nil {
			return err
		}
		var issues SubjectIssues
		err := walkLineageNamespaces(tx.Bucket(bucketSubjectHeads), namespace, func(ns string, _ *bolt.Bucket) error {
			idx, err := resolveSubjectIndexTx(tx, ns)
			if err != nil {
				return err
			}
			issues.add(ns, idx)
			keys := make([]string, 0, len(idx.Heads))
			for key := range idx.Heads {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := ctx.Err(); err != nil {
					return err
				}
				h := idx.Heads[key].Head
				out.SubjectHeads = append(out.SubjectHeads, SubjectHeadInspection{Namespace: ns, Name: h.Name, Reference: h.Reference, Revisions: h.Revisions, State: h.State})
			}
			return nil
		})
		out.SubjectIssues = issues.nonempty()
		return err
	})
	if err != nil {
		return LineageArchive{}, err
	}
	return out, nil
}

func walkLineageNamespaces(root *bolt.Bucket, namespace string, visit func(string, *bolt.Bucket) error) error {
	if root == nil {
		return nil
	}
	if namespace != "" {
		if b := root.Bucket([]byte(namespace)); b != nil {
			return visit(namespace, b)
		}
		return nil
	}
	return root.ForEach(func(key, _ []byte) error {
		b := root.Bucket(key)
		if b == nil {
			return errors.New("expected namespaced bucket")
		}
		return visit(string(key), b)
	})
}

type backupWriter struct {
	ctx context.Context
	dst io.Writer
}

func (w backupWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.dst.Write(data)
}

// Backup writes a complete bbolt database image from one read transaction.
func (s *Store) Backup(ctx context.Context, writer io.Writer) error {
	return s.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(backupWriter{ctx: ctx, dst: writer})
		return err
	})
}

// RestoreLineageBackup validates an image before installing it at a new
// destination. It never opens an existing destination for writing.
func RestoreLineageBackup(ctx context.Context, reader io.Reader, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".graymatter-restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, copyErr := io.Copy(backupWriter{ctx: ctx, dst: f}, reader)
	syncErr := f.Sync()
	if err := errors.Join(copyErr, syncErr, f.Close()); err != nil {
		return err
	}
	db, err := bolt.Open(f.Name(), 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	validateErr := db.View(func(tx *bolt.Tx) error {
		var checkErr error
		for err := range tx.Check() {
			if checkErr == nil {
				checkErr = err
			}
		}
		if checkErr != nil {
			return checkErr
		}
		if tx.Bucket(bucketFacts) == nil || tx.Bucket(bucketAgents) == nil {
			return errors.New("backup is missing the GrayMatter store buckets")
		}
		return validateLineageBackup(ctx, tx)
	})
	if err := errors.Join(validateErr, db.Close(), ctx.Err()); err != nil {
		return err
	}
	return os.Link(f.Name(), destination)
}

func validateLineageBackup(ctx context.Context, tx *bolt.Tx) error {
	if err := validateSubjectBackupTx(ctx, tx); err != nil {
		return err
	}
	if err := validateCitationBackupTx(ctx, tx); err != nil {
		return err
	}
	if err := walkLineageNamespaces(tx.Bucket(bucketEvidenceSnapshots), "", func(ns string, b *bolt.Bucket) error {
		return b.ForEach(func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry, err := evidenceEntry(b, string(key))
			if err != nil {
				return err
			}
			if entry.Snapshot.Namespace != ns {
				return errors.New("backup snapshot namespace mismatch")
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if err := walkLineageNamespaces(tx.Bucket(bucketLineageRecords), "", func(ns string, b *bolt.Bucket) error {
		snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, ns, false)
		heads, _ := evidenceNamespace(tx, bucketLineageHeads, ns, false)
		rows := lineageRowBucket(tx, ns)
		graph := make(map[string][]string)
		err := b.ForEach(func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, found, err := lineageRecordAt(b, ns, string(key))
			if err != nil {
				return err
			}
			if !found {
				return errors.New("backup contains a malformed lineage record")
			}
			if snapshots == nil || snapshots.Get(key) == nil {
				return fmt.Errorf("backup missing lineage head snapshot %s", key)
			}
			if err := validateBackupHead(rows, heads, ns, record, key); err != nil {
				return err
			}
			graph[string(key)] = nil
			for i, ref := range record.sources() {
				legacy := i >= len(record.Replaces)+len(record.Derived)
				if ref.FactID == "" || ref.ContentHash == "" && !legacy {
					return errors.New("backup contains a malformed lineage reference")
				}
				if ref.ContentHash == "" {
					continue
				}
				hash, err := hex.DecodeString(ref.ContentHash)
				if err != nil || len(hash) != 32 {
					return errors.New("backup contains a malformed lineage hash")
				}
				source := ref.key(ns)
				if snapshots.Get([]byte(source)) == nil {
					return fmt.Errorf("backup missing lineage source %s", ref.FactID)
				}
				graph[string(key)] = append(graph[string(key)], source)
			}
			return nil
		})
		if err != nil {
			return err
		}
		return validateAcyclicLineage(graph)
	}); err != nil {
		return err
	}
	if err := walkLineageNamespaces(tx.Bucket(bucketLineageHeads), "", func(ns string, b *bolt.Bucket) error {
		records, _ := evidenceNamespace(tx, bucketLineageRecords, ns, false)
		return b.ForEach(func(id, key []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, found, err := lineageRecordAt(records, ns, string(key))
			if err != nil {
				return err
			}
			if !found || record.Head.FactID != string(id) {
				return errors.New("backup lineage head index mismatch")
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if err := walkLineageNamespaces(tx.Bucket(bucketEvidenceOwners), "", func(ns string, b *bolt.Bucket) error {
		snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, ns, false)
		records, _ := evidenceNamespace(tx, bucketLineageRecords, ns, false)
		return b.ForEach(func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if snapshots == nil || snapshots.Get(key) == nil || b.Bucket(key) == nil {
				return errors.New("backup contains a dangling evidence owner")
			}
			return b.Bucket(key).ForEach(func(owner, value []byte) error {
				if len(value) != 1 || value[0] != 1 {
					return errors.New("backup contains a malformed evidence owner")
				}
				root, lineage := strings.CutPrefix(string(owner), lineageOwnerPrefix)
				if !lineage {
					return nil // journal claims are validated against retained records below
				}
				record, found, err := lineageRecordAt(records, ns, root)
				if err != nil {
					return err
				}
				if !found || root != string(key) && !slices.ContainsFunc(record.sources(), func(ref LineageReference) bool {
					return ref.ContentHash != "" && ref.key(ns) == string(key)
				}) {
					return errors.New("backup contains an unclaimed lineage owner")
				}
				return nil
			})
		})
	}); err != nil {
		return err
	}
	return validateReviewEvidenceBackupTx(ctx, tx)
}

// Record creation and collection change the head index in the same transaction.
// Retired legacy bridges and pruned history still need their last known index.
func validateBackupHead(rows, heads *bolt.Bucket, namespace string, record lineageRecord, key []byte) error {
	id := []byte(record.Head.FactID)
	if heads == nil || string(heads.Get(id)) != string(key) {
		return fmt.Errorf("backup lineage head index mismatch for fact %s in namespace %q", id, namespace)
	}
	if rows == nil {
		return nil
	}
	raw := rows.Get(id)
	if raw == nil {
		return nil
	}
	f, err := unmarshalFactLite(raw)
	if err != nil {
		return err
	}
	if f.ID != record.Head.FactID || f.AgentID != namespace {
		return errors.New("backup lineage head fact identity mismatch")
	}
	return nil
}

// A complete restore rejects cycles in durable version records. Observable
// legacy fact pointers remain separate and are not reclassified by restore.
func validateAcyclicLineage(graph map[string][]string) error {
	degree := make(map[string]int, len(graph))
	for _, sources := range graph {
		for _, source := range sources {
			if _, exists := graph[source]; exists {
				degree[source]++
			}
		}
	}
	var queue []string
	for key := range graph {
		if degree[key] == 0 {
			queue = append(queue, key)
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, source := range graph[queue[i]] {
			if _, exists := graph[source]; !exists {
				continue
			}
			degree[source]--
			if degree[source] == 0 {
				queue = append(queue, source)
			}
		}
	}
	if len(queue) != len(graph) {
		return errors.New("backup contains a cycle in versioned lineage")
	}
	return nil
}
