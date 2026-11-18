package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketLineageRecords = []byte("lineage_records_v1")
	bucketLineageHeads   = []byte("lineage_heads_v1")

	// ErrRevisionRequired protects content already referenced by lineage.
	ErrRevisionRequired = errors.New("content has retained lineage; revise it explicitly")
	// ErrEvidenceQuota means the complete evidence write exceeds its budget.
	ErrEvidenceQuota = errors.New("evidence quota exceeded")
	// ErrFactNotFound means the requested fact version is unavailable.
	ErrFactNotFound = errors.New("fact version not found")
)

const (
	LineageVerified         = "versioned"
	LineageContentChanged   = "content_changed"
	LineageLegacyIncomplete = "legacy_incomplete"
	LineageMissing          = "missing"
	LineageCycle            = "cycle"
	lineageOwnerPrefix      = "lineage:"
)

// LineageReference identifies an exact version in the owning namespace.
type LineageReference struct {
	FactID      string `json:"fact_id"`
	ContentHash string `json:"content_hash,omitempty"`
}

func (r LineageReference) key(namespace string) string {
	return evidenceIdentityKey(namespace, r.FactID, r.ContentHash)
}

// LineageProjection is regenerated from authoritative version records on read.
type LineageProjection struct {
	ContentHash    string             `json:"content_hash,omitempty"`
	HistoricalHash string             `json:"historical_hash,omitempty"`
	DerivedFrom    []LineageReference `json:"derived_from,omitempty"`
	Replaces       []LineageReference `json:"replaces,omitempty"`
	LegacySources  []LineageReference `json:"legacy_sources,omitempty"`
	LineageStatus  string             `json:"lineage_status,omitempty"`
}

// LineageInspection shows one retained version and the relations it recorded.
// A divergent canonical row can be inspected by requesting its historical hash.
type LineageInspection struct {
	Snapshot   *EvidenceSnapshot `json:"snapshot,omitempty"`
	CapturedAt *time.Time        `json:"captured_at,omitempty"`
	LineageProjection
}

type lineageRecord struct {
	Head     LineageReference   `json:"head"`
	Replaces []LineageReference `json:"replaces,omitempty"`
	Derived  []LineageReference `json:"derived_from,omitempty"`
	Legacy   []LineageReference `json:"legacy_sources,omitempty"`
	Captured *time.Time         `json:"captured_at,omitempty"`
}

func (*Store) SupportsAuthoritativeLineage() bool { return true }

func lineageRef(snapshot EvidenceSnapshot) LineageReference {
	return LineageReference{FactID: snapshot.FactID, ContentHash: snapshot.ContentHash}
}

func (r lineageRecord) sources() []LineageReference {
	out := make([]LineageReference, 0, len(r.Replaces)+len(r.Derived)+len(r.Legacy))
	out = append(out, r.Replaces...)
	out = append(out, r.Derived...)
	return append(out, r.Legacy...)
}

func (r lineageRecord) projection() LineageProjection {
	status := LineageVerified
	if r.Captured != nil {
		status = LineageLegacyIncomplete
	}
	return LineageProjection{
		ContentHash: r.Head.ContentHash, LineageStatus: status,
		Replaces: r.Replaces, DerivedFrom: r.Derived, LegacySources: r.Legacy,
	}
}

func lineageRecordAt(records *bolt.Bucket, namespace, key string) (lineageRecord, bool, error) {
	var record lineageRecord
	if records == nil || records.Get([]byte(key)) == nil {
		return record, false, nil
	}
	if err := json.Unmarshal(records.Get([]byte(key)), &record); err != nil {
		return record, false, fmt.Errorf("decode lineage record: %w", err)
	}
	if record.Head.FactID == "" || record.Head.ContentHash == "" || record.Head.key(namespace) != key {
		return record, false, errors.New("lineage record identity mismatch")
	}
	return record, true, nil
}

func lineageRowBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
	if parent := tx.Bucket(bucketFacts); parent != nil {
		return parent.Bucket([]byte(namespace))
	}
	return nil
}

func lineageHeadKey(tx *bolt.Tx, namespace, factID string) string {
	heads, _ := evidenceNamespace(tx, bucketLineageHeads, namespace, false)
	if heads == nil {
		return ""
	}
	return string(heads.Get([]byte(factID)))
}

func hydrateLineageTx(tx *bolt.Tx, f *Fact) error {
	// Treat persisted projections as caches, including projections written
	// by a caller that does not own the version record.
	f.LineageProjection = LineageProjection{}
	key := lineageHeadKey(tx, f.AgentID, f.ID)
	if key == "" {
		return nil
	}
	records, _ := evidenceNamespace(tx, bucketLineageRecords, f.AgentID, false)
	record, found, err := lineageRecordAt(records, f.AgentID, key)
	if err != nil {
		return err
	}
	current := NewEvidenceSnapshot(*f)
	if !found {
		f.LineageProjection = LineageProjection{ContentHash: current.ContentHash, LineageStatus: LineageMissing}
		return nil
	}
	if record.Head.ContentHash != current.ContentHash {
		f.LineageProjection = LineageProjection{
			ContentHash: current.ContentHash, HistoricalHash: record.Head.ContentHash,
			LineageStatus: LineageContentChanged,
		}
		return nil
	}
	f.LineageProjection = record.projection()
	return nil
}

func writeLineageTx(tx *bolt.Tx, head EvidenceSnapshot, sources []EvidenceSnapshot, record lineageRecord, added *int64) error {
	namespace := head.Namespace
	key := head.key()
	records, err := evidenceNamespace(tx, bucketLineageRecords, namespace, true)
	if err != nil {
		return err
	}
	heads, err := evidenceNamespace(tx, bucketLineageHeads, namespace, true)
	if err != nil {
		return err
	}
	if records.Get([]byte(key)) != nil {
		return errors.New("lineage version already exists")
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	*added += int64(len(key) + len(payload))
	if heads.Get([]byte(head.FactID)) == nil {
		*added += int64(len(head.FactID) + len(key))
	}
	owner := lineageOwnerPrefix + key
	for _, snapshot := range append([]EvidenceSnapshot{head}, sources...) {
		if snapshot.Namespace != namespace {
			return errors.New("lineage source belongs to a different namespace")
		}
		bytes, err := evidenceRootAddedBytesTx(tx, snapshot, owner)
		if err != nil {
			return err
		}
		*added += bytes
		if err := rootEvidenceTx(tx, snapshot, owner); err != nil {
			return err
		}
	}
	if err := records.Put([]byte(key), payload); err != nil {
		return err
	}
	return heads.Put([]byte(head.FactID), []byte(key))
}

func recordRevisionLineageTx(tx *bolt.Tx, replacement *Fact, sources []Fact, added *int64) error {
	head := NewEvidenceSnapshot(*replacement)
	record := lineageRecord{Head: lineageRef(head)}
	snapshots := make([]EvidenceSnapshot, 0, len(sources))
	for _, source := range sources {
		snapshot := NewEvidenceSnapshot(source)
		snapshots = append(snapshots, snapshot)
		record.Replaces = append(record.Replaces, lineageRef(snapshot))
	}
	if err := writeLineageTx(tx, head, snapshots, record, added); err != nil {
		return err
	}
	replacement.LineageProjection = record.projection()
	return nil
}

// walkLineageTx follows exact content versions. Separate active and visited
// states distinguish a real cycle from a shared ancestor in a fork.
func walkLineageTx(ctx context.Context, tx *bolt.Tx, namespace string, roots []string) (map[string]bool, []string, string, error) {
	return walkLineageWithLegacyTx(ctx, tx, namespace, roots, nil)
}

func walkLineageWithLegacyTx(ctx context.Context, tx *bolt.Tx, namespace string, roots []string, legacy map[string][]string) (map[string]bool, []string, string, error) {
	records, _ := evidenceNamespace(tx, bucketLineageRecords, namespace, false)
	snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	type step struct {
		key  string
		exit bool
	}
	visited := make(map[string]bool)
	active := make(map[string]bool)
	seenIDs := make(map[string]bool)
	var ids []string
	var status string
	stack := make([]step, 0, len(roots))
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, step{key: roots[i]})
	}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := item.key
		if item.exit {
			delete(active, key)
			continue
		}
		if active[key] {
			status = LineageCycle
			continue
		}
		if visited[key] {
			continue
		}
		visited[key], active[key] = true, true
		stack = append(stack, step{key: key, exit: true})
		if snapshots == nil || snapshots.Get([]byte(key)) == nil {
			if status == "" {
				status = LineageMissing
			}
			continue
		}
		entry, err := evidenceEntry(snapshots, key)
		if err != nil {
			return nil, nil, "", err
		}
		if entry.Snapshot.Namespace != namespace {
			return nil, nil, "", errors.New("lineage snapshot namespace mismatch")
		}
		id := entry.Snapshot.FactID
		if !seenIDs[id] {
			seenIDs[id] = true
			ids = append(ids, id)
		}
		record, _, err := lineageRecordAt(records, namespace, key)
		if err != nil {
			return nil, nil, "", err
		}
		for _, predecessor := range legacy[key] {
			stack = append(stack, step{key: predecessor})
		}
		refs := record.sources()
		for i := len(refs) - 1; i >= 0; i-- {
			if refs[i].ContentHash != "" {
				stack = append(stack, step{key: refs[i].key(namespace)})
			}
		}
	}
	return visited, ids, status, nil
}

// lineageEvidenceRootsTx is called in the same transaction as owner release
// and orphan sweeping. Journal roots retain their complete version chains.
func lineageEvidenceRootsTx(tx *bolt.Tx, namespace string, stagedRoots ...string) (map[string]bool, error) {
	heads, _ := evidenceNamespace(tx, bucketLineageHeads, namespace, false)
	if heads == nil {
		return nil, nil
	}
	roots := append([]string(nil), stagedRoots...)
	legacy := make(map[string][]string)
	destinations := make(map[string]legacyEvidenceDestination)
	if rows := lineageRowBucket(tx, namespace); rows != nil {
		if err := heads.ForEach(func(id, key []byte) error {
			raw := rows.Get(id)
			if raw == nil {
				return nil
			}
			f, err := unmarshalFactLite(raw)
			if err != nil {
				return err
			}
			if f.ID != string(id) || f.AgentID != namespace {
				return errors.New("lineage head fact identity mismatch")
			}
			if f.IsSuperseded() {
				// Follow observable old-writer transitions without turning
				// them into claims about exact historical relationships.
				destination, err := legacyEvidenceDestinationTx(tx, f, destinations)
				if err != nil {
					return err
				}
				if !destination.live {
					if version := destination.version; version != "" {
						legacy[version] = append(legacy[version], string(key))
					}
					return nil
				}
			}
			roots = append(roots, string(key))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	owners, _ := evidenceNamespace(tx, bucketEvidenceOwners, namespace, false)
	if owners != nil {
		if err := owners.ForEach(func(key, _ []byte) error {
			b := owners.Bucket(key)
			if b == nil {
				return errors.New("invalid evidence owners index")
			}
			if err := b.ForEach(func(owner, _ []byte) error {
				if !strings.HasPrefix(string(owner), lineageOwnerPrefix) {
					roots = append(roots, string(key))
				}
				return nil
			}); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.Strings(roots)
	reachable, _, _, err := walkLineageWithLegacyTx(context.Background(), tx, namespace, roots, legacy)
	return reachable, err
}

func collectLineageTx(tx *bolt.Tx, namespace string, now time.Time, stagedRoots ...string) error {
	_, err := collectLineageChangesTx(tx, namespace, now, stagedRoots...)
	return err
}

func collectLineageChangesTx(tx *bolt.Tx, namespace string, now time.Time, stagedRoots ...string) (bool, error) {
	changed := false
	records, _ := evidenceNamespace(tx, bucketLineageRecords, namespace, false)
	reachable, err := lineageEvidenceRootsTx(tx, namespace, stagedRoots...)
	if err != nil {
		return false, err
	}
	if records != nil {
		owners, _ := evidenceNamespace(tx, bucketEvidenceOwners, namespace, false)
		var releases [][2]string
		if owners != nil {
			if err := owners.ForEach(func(key, _ []byte) error {
				b := owners.Bucket(key)
				if b == nil {
					return errors.New("invalid evidence owners index")
				}
				return b.ForEach(func(raw, _ []byte) error {
					owner := string(raw)
					if root, ok := strings.CutPrefix(owner, lineageOwnerPrefix); ok && !reachable[root] {
						releases = append(releases, [2]string{string(key), owner})
					}
					return nil
				})
			}); err != nil {
				return false, err
			}
		}
		for _, item := range releases {
			if err := releaseEvidenceTx(tx, namespace, item[0], item[1], now); err != nil {
				return false, err
			}
		}
		changed = len(releases) != 0
	}
	pruned, err := pruneEvidenceOrphansTx(tx, namespace, now, reachable)
	if err != nil {
		return false, err
	}
	changed = changed || pruned
	if records == nil {
		return changed, nil
	}
	snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	heads, _ := evidenceNamespace(tx, bucketLineageHeads, namespace, false)
	var expired []string
	if err := records.ForEach(func(key, _ []byte) error {
		if snapshots == nil || snapshots.Get(key) == nil {
			expired = append(expired, string(key))
		}
		return nil
	}); err != nil {
		return false, err
	}
	for _, key := range expired {
		record, _, err := lineageRecordAt(records, namespace, key)
		if err != nil {
			return false, err
		}
		id := []byte(record.Head.FactID)
		if heads != nil && string(heads.Get(id)) == key {
			if err := heads.Delete(id); err != nil {
				return false, err
			}
		}
		if err := records.Delete([]byte(key)); err != nil {
			return false, err
		}
	}
	return changed || len(expired) != 0, nil
}

func hasLineageEvidenceTx(tx *bolt.Tx, f Fact) (bool, error) {
	if lineageHeadKey(tx, f.AgentID, f.ID) != "" {
		return true, nil
	}
	key := NewEvidenceSnapshot(f).key()
	snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, f.AgentID, false)
	records, _ := evidenceNamespace(tx, bucketLineageRecords, f.AgentID, false)
	if snapshots == nil || records == nil || snapshots.Get([]byte(key)) == nil {
		return false, nil
	}
	owners, _ := evidenceNamespace(tx, bucketEvidenceOwners, f.AgentID, false)
	if owners != nil {
		if b := owners.Bucket([]byte(key)); b != nil {
			owner, _ := b.Cursor().Seek([]byte(lineageOwnerPrefix))
			if strings.HasPrefix(string(owner), lineageOwnerPrefix) {
				return true, nil
			}
		}
	}
	// A journal can re-root a chain after its physical owner was released.
	// An exact record reference still protects that evidence.
	cursor := records.Cursor()
	for recordKey, _ := cursor.First(); recordKey != nil; recordKey, _ = cursor.Next() {
		record, _, err := lineageRecordAt(records, f.AgentID, string(recordKey))
		if err != nil {
			return false, err
		}
		for _, ref := range record.sources() {
			if ref.FactID == f.ID && ref.key(f.AgentID) == key {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Store) sweepLineage(namespace string, now time.Time) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		changed, err := collectLineageChangesTx(tx, namespace, now)
		if err != nil {
			return err
		}
		if !changed {
			return errFactUnchanged
		}
		return nil
	})
	if err == errFactUnchanged {
		return nil
	}
	return err
}
