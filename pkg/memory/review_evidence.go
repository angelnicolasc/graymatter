package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

const EvidenceSnapshotVersion = 1

const (
	evidenceOrphanGrace       = 30 * 24 * time.Hour
	reviewEvidenceOwnerPrefix = "review:"

	evidenceTransactionBytes = 1 << 20
	evidenceNamespaceBytes   = 64 << 20
	evidenceStoreBytes       = 256 << 20
)

var (
	bucketEvidenceSnapshots = []byte("evidence_snapshots_v1")
	bucketEvidenceOwners    = []byte("evidence_owners_v1")
)

// EvidenceSnapshot captures the immutable content of one canonical revision.
// It contains no vector or mutable retrieval metadata.
type EvidenceSnapshot struct {
	Version     int       `json:"version"`
	Namespace   string    `json:"namespace"`
	FactID      string    `json:"fact_id"`
	Text        string    `json:"text"`
	Kind        string    `json:"kind"`
	Confidence  string    `json:"confidence"`
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
}

// NewEvidenceSnapshot hashes the exact text, kind and declared confidence.
// Version 1 hashes a domain string followed by the three values in that
// order, each prefixed with its eight-byte big-endian byte length.
func NewEvidenceSnapshot(f Fact) EvidenceSnapshot {
	return EvidenceSnapshot{
		Version: EvidenceSnapshotVersion, Namespace: f.AgentID, FactID: f.ID,
		Text: f.Text, Kind: f.Kind, Confidence: f.Confidence, CreatedAt: f.CreatedAt.UTC(),
		ContentHash: evidenceHash("graymatter-evidence-content-v1", f.Text, f.Kind, f.Confidence),
	}
}

func evidenceHash(domain string, fields ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain + "\x00"))
	var length [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s EvidenceSnapshot) key() string {
	return evidenceIdentityKey(s.Namespace, s.FactID, s.ContentHash)
}

func evidenceIdentityKey(namespace, factID, contentHash string) string {
	return evidenceHash("graymatter-evidence-identity-v1", namespace, factID, contentHash)
}

type storedEvidence struct {
	Snapshot    EvidenceSnapshot `json:"snapshot"`
	OrphanSince *time.Time       `json:"orphan_since,omitempty"`
	// LineageOnly scopes ownerless rows to the shared evidence budget. Older
	// stores only had review owners, so their grace rows use the zero value.
	LineageOnly bool `json:"lineage_only,omitempty"`
}

func evidenceNamespace(tx *bolt.Tx, name []byte, namespace string, create bool) (*bolt.Bucket, error) {
	if create {
		root, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return nil, err
		}
		return root.CreateBucketIfNotExists([]byte(namespace))
	}
	if root := tx.Bucket(name); root != nil {
		return root.Bucket([]byte(namespace)), nil
	}
	return nil, nil
}

func evidenceEntry(bucket *bolt.Bucket, key string) (storedEvidence, error) {
	var entry storedEvidence
	if bucket == nil {
		return entry, fmt.Errorf("evidence snapshot %s is missing", key)
	}
	data := bucket.Get([]byte(key))
	if data == nil {
		return entry, fmt.Errorf("evidence snapshot %s is missing", key)
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return entry, fmt.Errorf("decode evidence snapshot: %w", err)
	}
	snapshot := entry.Snapshot
	if snapshot.Version != EvidenceSnapshotVersion || snapshot.key() != key ||
		snapshot.ContentHash != evidenceHash("graymatter-evidence-content-v1", snapshot.Text, snapshot.Kind, snapshot.Confidence) {
		return entry, errors.New("evidence snapshot identity or content hash mismatch")
	}
	return entry, nil
}

func putEvidenceEntry(bucket *bolt.Bucket, key string, entry storedEvidence) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(key), raw)
}

// rootEvidenceTx makes an exact snapshot durable for a named owner and
// preserves the first captured timestamp when another owner shares its identity.
func rootEvidenceTx(tx *bolt.Tx, snapshot EvidenceSnapshot, owner string) error {
	if snapshot.Version != EvidenceSnapshotVersion || snapshot.Namespace == "" || snapshot.FactID == "" ||
		!utf8.ValidString(snapshot.Namespace) || !utf8.ValidString(snapshot.FactID) ||
		!utf8.ValidString(snapshot.Text) || !utf8.ValidString(snapshot.Kind) || !utf8.ValidString(snapshot.Confidence) ||
		snapshot.ContentHash != evidenceHash("graymatter-evidence-content-v1", snapshot.Text, snapshot.Kind, snapshot.Confidence) {
		return errors.New("cannot root an invalid evidence snapshot")
	}
	snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, snapshot.Namespace, true)
	if err != nil {
		return err
	}
	key := snapshot.key()
	entry := storedEvidence{Snapshot: snapshot}
	missing := snapshots.Get([]byte(key)) == nil
	if !missing {
		entry, err = evidenceEntry(snapshots, key)
		if err != nil {
			return err
		}
	}
	if missing || entry.OrphanSince != nil || entry.LineageOnly {
		entry.OrphanSince = nil
		entry.LineageOnly = false
		if err := putEvidenceEntry(snapshots, key, entry); err != nil {
			return err
		}
	}
	owners, err := evidenceNamespace(tx, bucketEvidenceOwners, snapshot.Namespace, true)
	if err != nil {
		return err
	}
	rooted, err := owners.CreateBucketIfNotExists([]byte(key))
	if err != nil {
		return err
	}
	return rooted.Put([]byte(owner), []byte{1})
}

func releaseEvidenceTx(tx *bolt.Tx, namespace, key, owner string, now time.Time) error {
	owners, err := evidenceNamespace(tx, bucketEvidenceOwners, namespace, false)
	if err != nil {
		return err
	}
	if owners == nil {
		return errors.New("evidence owners bucket is missing")
	}
	rooted := owners.Bucket([]byte(key))
	if rooted == nil || rooted.Get([]byte(owner)) == nil {
		return errors.New("evidence owner is missing")
	}
	if err := rooted.Delete([]byte(owner)); err != nil {
		return err
	}
	if first, _ := rooted.Cursor().First(); first != nil {
		return nil
	}
	if err := owners.DeleteBucket([]byte(key)); err != nil {
		return err
	}
	snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	if err != nil {
		return err
	}
	entry, err := evidenceEntry(snapshots, key)
	if err != nil {
		return err
	}
	if entry.OrphanSince == nil {
		now = now.UTC()
		entry.OrphanSince = &now
	}
	entry.LineageOnly = strings.HasPrefix(owner, lineageOwnerPrefix)
	return putEvidenceEntry(snapshots, key, entry)
}

// hasReviewEvidenceOwner checks a single snapshot's owner index.
func hasReviewEvidenceOwner(owners *bolt.Bucket) bool {
	if owners == nil {
		return false
	}
	prefix := []byte(reviewEvidenceOwnerPrefix)
	owner, _ := owners.Cursor().Seek(prefix)
	return bytes.HasPrefix(owner, prefix)
}

// evidenceRootAddedBytesTx measures the new payload that rootEvidenceTx will
// add. Existing snapshots and identical owner edges are already accounted for.
func evidenceRootAddedBytesTx(tx *bolt.Tx, snapshot EvidenceSnapshot, owner string) (int64, error) {
	snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, snapshot.Namespace, false)
	if err != nil {
		return 0, err
	}
	key := []byte(snapshot.key())
	var added int64
	if snapshots == nil || snapshots.Get(key) == nil {
		raw, err := json.Marshal(storedEvidence{Snapshot: snapshot})
		if err != nil {
			return 0, err
		}
		added += int64(len(key) + len(raw))
	}
	owners, err := evidenceNamespace(tx, bucketEvidenceOwners, snapshot.Namespace, false)
	if err != nil {
		return 0, err
	}
	if owners != nil {
		if rooted := owners.Bucket(key); rooted != nil && rooted.Get([]byte(owner)) != nil {
			return added, nil
		}
	}
	return added + int64(len(key)+len(owner)+1), nil
}

// checkEvidenceQuotaTx counts retained logical bytes after the staged write.
// The added count includes only new snapshots, owners and lineage records.
func checkEvidenceQuotaTx(tx *bolt.Tx, namespace string, added int64) error {
	if added > evidenceTransactionBytes {
		return fmt.Errorf("%w: %d new logical bytes exceed %d per transaction", ErrEvidenceQuota, added, evidenceTransactionBytes)
	}
	nsBytes, storeBytes, err := evidencePayloadBytesTx(tx, namespace)
	if err != nil {
		return err
	}
	if nsBytes > evidenceNamespaceBytes {
		return fmt.Errorf("%w: %d retained logical bytes exceed %d for namespace %q", ErrEvidenceQuota, nsBytes, evidenceNamespaceBytes, namespace)
	}
	if storeBytes > evidenceStoreBytes {
		return fmt.Errorf("%w: %d retained logical bytes exceed %d for the store", ErrEvidenceQuota, storeBytes, evidenceStoreBytes)
	}
	return nil
}

func evidencePayloadBytesTx(tx *bolt.Tx, namespace string) (namespaceBytes, storeBytes int64, err error) {
	for _, name := range [][]byte{bucketEvidenceSnapshots, bucketEvidenceOwners, bucketLineageRecords, bucketLineageHeads} {
		bucket := tx.Bucket(name)
		if bucket == nil {
			continue
		}
		isOwnerIndex := bytes.Equal(name, bucketEvidenceOwners)
		if err := bucket.ForEachBucket(func(ns []byte) error {
			b := bucket.Bucket(ns)
			var size int64
			if err := b.ForEach(func(key, raw []byte) error {
				if isOwnerIndex {
					rooted := b.Bucket(key)
					if rooted == nil {
						return errors.New("unexpected value in evidence owner index")
					}
					return rooted.ForEach(func(owner, value []byte) error {
						if value == nil {
							return errors.New("unexpected bucket in evidence owner index")
						}
						size += int64(len(key) + len(owner) + len(value))
						return nil
					})
				}
				if raw == nil {
					return errors.New("unexpected bucket in evidence payload")
				}
				size += int64(len(key) + len(raw))
				return nil
			}); err != nil {
				return err
			}
			storeBytes += size
			if string(ns) == namespace {
				namespaceBytes += size
			}
			return nil
		}); err != nil {
			return 0, 0, err
		}
	}
	return namespaceBytes, storeBytes, nil
}

// pruneEvidenceOrphansTx collects only snapshots that passed the grace period
// and remain unowned and unreachable in the write transaction. The caller
// computes roots in this transaction before removing obsolete lineage edges.
func pruneEvidenceOrphansTx(tx *bolt.Tx, namespace string, now time.Time, roots map[string]bool) (bool, error) {
	snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	if err != nil || snapshots == nil {
		return false, err
	}
	owners, err := evidenceNamespace(tx, bucketEvidenceOwners, namespace, false)
	if err != nil {
		return false, err
	}
	var expired, reachable, newlyOrphaned []string
	now = now.UTC()
	err = snapshots.ForEach(func(key, raw []byte) error {
		if owners != nil && owners.Bucket(key) != nil {
			return nil
		}
		var metadata struct {
			OrphanSince *time.Time `json:"orphan_since"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return err
		}
		if roots[string(key)] {
			if metadata.OrphanSince != nil {
				reachable = append(reachable, string(key))
			}
		} else if metadata.OrphanSince == nil {
			newlyOrphaned = append(newlyOrphaned, string(key))
		} else if !now.Before(metadata.OrphanSince.Add(evidenceOrphanGrace)) {
			expired = append(expired, string(key))
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, key := range reachable {
		entry, err := evidenceEntry(snapshots, key)
		if err != nil {
			return false, err
		}
		entry.OrphanSince = nil
		entry.LineageOnly = true
		if err := putEvidenceEntry(snapshots, key, entry); err != nil {
			return false, err
		}
	}
	for _, key := range newlyOrphaned {
		entry, err := evidenceEntry(snapshots, key)
		if err != nil {
			return false, err
		}
		entry.OrphanSince = &now
		if err := putEvidenceEntry(snapshots, key, entry); err != nil {
			return false, err
		}
	}
	for _, key := range expired {
		if err := snapshots.Delete([]byte(key)); err != nil {
			return false, err
		}
	}
	return len(expired)+len(reachable)+len(newlyOrphaned) != 0, nil
}
