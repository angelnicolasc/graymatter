package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var bucketSubjectHeads = []byte("subject_heads_v1")

const subjectOwnerPrefix = "subject:"

type subjectHead struct {
	Name      string           `json:"name"`
	Reference LineageReference `json:"reference"`
	Revisions uint64           `json:"revisions"`
	State     string           `json:"state"`
}

type resolvedSubject struct {
	Key                   string
	Head                  subjectHead
	Fact                  Fact
	Revisions             uint64
	Closed                bool
	Unresolved            error
	UnresolvedFingerprint string
	DamagedSnapshot       *LineageReference
}

type subjectIndex struct {
	Heads     map[string]resolvedSubject
	ByVersion map[string][]SubjectProjection
	ByID      map[string][]string
}

func subjectHeadsTx(tx *bolt.Tx, namespace string) (map[string]subjectHead, error) {
	out := map[string]subjectHead{}
	b, err := evidenceNamespace(tx, bucketSubjectHeads, namespace, false)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return out, nil
	}
	err = b.ForEach(func(key, raw []byte) error {
		var h subjectHead
		if raw == nil {
			return fmt.Errorf("unexpected bucket in subject heads")
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return err
		}
		normalized, err := NormalizeSubject(h.Name)
		hash, hashErr := hex.DecodeString(h.Reference.ContentHash)
		if err != nil || normalized != string(key) || h.Reference.FactID == "" || hashErr != nil || len(hash) != 32 || h.State != "active" && h.State != "closed" {
			return fmt.Errorf("invalid subject head %q", key)
		}
		out[string(key)] = h
		return nil
	})
	return out, err
}

func exactRevisionSuccessorsTx(tx *bolt.Tx, namespace string) (map[string][]LineageReference, error) {
	out := map[string][]LineageReference{}
	b, _ := evidenceNamespace(tx, bucketLineageRecords, namespace, false)
	if b == nil {
		return out, nil
	}
	err := b.ForEach(func(key, _ []byte) error {
		r, found, err := lineageRecordAt(b, namespace, string(key))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("missing lineage record")
		}
		for _, source := range r.Replaces {
			if source.FactID == "" || source.ContentHash == "" {
				return fmt.Errorf("invalid exact revision reference")
			}
			sourceKey := source.key(namespace)
			duplicate := false
			for _, prior := range out[sourceKey] {
				if prior == r.Head {
					duplicate = true
				}
			}
			if !duplicate {
				out[sourceKey] = append(out[sourceKey], r.Head)
			}
		}
		return nil
	})
	return out, err
}

func resolveSubjectHeadTx(tx *bolt.Tx, namespace, key string, head subjectHead, next map[string][]LineageReference) (out resolvedSubject, err error) {
	out = resolvedSubject{Key: key, Head: head, Revisions: head.Revisions, Closed: head.State == "closed"}
	if out.Closed {
		return out, nil
	}
	ref := head.Reference
	seen := map[string]LineageReference{}
	rows := lineageRowBucket(tx, namespace)
	snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	defer func() {
		if errors.Is(err, ErrSubjectHeadUnavailable) || errors.Is(err, ErrSubjectAmbiguous) {
			out.Unresolved = err
			out.UnresolvedFingerprint = unresolvedSubjectFingerprint(seen, next, rows, snapshots)
		}
	}()
	for {
		version := ref.key(namespace)
		if _, exists := seen[version]; exists {
			return out, fmt.Errorf("%w: subject %q has a revision cycle", ErrSubjectHeadUnavailable, head.Name)
		}
		seen[version] = ref
		entry, err := evidenceEntry(snapshots, version)
		if err != nil || entry.Snapshot.FactID != ref.FactID || entry.Snapshot.ContentHash != ref.ContentHash || entry.Snapshot.Namespace != namespace {
			out.DamagedSnapshot = &ref
			return out, fmt.Errorf("%w: subject %q exact snapshot missing or corrupt", ErrSubjectHeadUnavailable, head.Name)
		}
		var current Fact
		exists := false
		if rows != nil && rows.Get([]byte(ref.FactID)) != nil {
			current, err = unmarshalFactLite(rows.Get([]byte(ref.FactID)))
			if err != nil {
				return out, err
			}
			if current.AgentID != namespace || current.ID != ref.FactID || NewEvidenceSnapshot(current).ContentHash != ref.ContentHash {
				return out, fmt.Errorf("%w: subject %q canonical version changed", ErrSubjectHeadUnavailable, head.Name)
			}
			exists = true
		}
		children := next[version]
		if len(children) > 1 {
			return out, fmt.Errorf("%w: subject %q has multiple exact successors", ErrSubjectAmbiguous, head.Name)
		}
		if exists && !current.IsSuperseded() {
			if len(children) > 0 {
				return out, fmt.Errorf("%w: live predecessor contradicts revision for %q", ErrSubjectAmbiguous, head.Name)
			}
			out.Fact = current
			return out, nil
		}
		if exists && current.SupersededBy == SupersededByAgent {
			if len(children) > 0 {
				return out, fmt.Errorf("%w: retirement contradicts revision for %q", ErrSubjectAmbiguous, head.Name)
			}
			out.Closed = true
			// Keep the proven terminal row for a later new-writer delete or
			// prune to close the stored head and release its subject owner.
			out.Fact = current
			return out, nil
		}
		if len(children) != 1 {
			return out, fmt.Errorf("%w: subject %q has no provable live successor", ErrSubjectHeadUnavailable, head.Name)
		}
		if exists && current.SupersededBy != children[0].FactID {
			return out, fmt.Errorf("%w: subject %q successor disagrees with canonical retirement", ErrSubjectHeadUnavailable, head.Name)
		}
		if out.Revisions == ^uint64(0) {
			return out, fmt.Errorf("subject revision counter overflow")
		}
		out.Revisions++
		ref = children[0]
	}
}

// Include the observed exact path in a degraded fence. Ordinary access
// counters and decay weights do not change the authoritative version.
func unresolvedSubjectFingerprint(seen map[string]LineageReference, next map[string][]LineageReference, rows, snapshots *bolt.Bucket) string {
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		observation := struct {
			Reference     LineageReference
			CanonicalHash string
			RetiredBy     string
			SnapshotState string
			Successors    []LineageReference
		}{Reference: seen[key], Successors: next[key]}
		if rows != nil {
			if raw := rows.Get([]byte(observation.Reference.FactID)); raw != nil {
				fact, err := unmarshalFactLite(raw)
				if err == nil {
					observation.CanonicalHash = NewEvidenceSnapshot(fact).ContentHash
					observation.RetiredBy = fact.SupersededBy
				} else {
					observation.CanonicalHash = fmt.Sprintf("invalid:%x", sha256.Sum256(raw))
				}
			}
		}
		if snapshots != nil {
			if raw := snapshots.Get([]byte(key)); raw != nil {
				if _, err := evidenceEntry(snapshots, key); err == nil {
					observation.SnapshotState = "retained"
				} else {
					observation.SnapshotState = fmt.Sprintf("invalid:%x", sha256.Sum256(raw))
				}
			}
		}
		raw, _ := json.Marshal(observation)
		hash.Write(raw)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func resolveSubjectIndexTx(tx *bolt.Tx, namespace string) (subjectIndex, error) {
	out := subjectIndex{Heads: map[string]resolvedSubject{}, ByVersion: map[string][]SubjectProjection{}, ByID: map[string][]string{}}
	heads, err := subjectHeadsTx(tx, namespace)
	if err != nil {
		return out, err
	}
	if len(heads) == 0 {
		return out, nil
	}
	next, err := exactRevisionSuccessorsTx(tx, namespace)
	if err != nil {
		return out, err
	}
	var keys []string
	for key := range heads {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r, err := resolveSubjectHeadTx(tx, namespace, key, heads[key], next)
		if err != nil && r.Unresolved == nil {
			return out, err
		}
		out.Heads[key] = r
		if r.Unresolved != nil {
			continue
		}
		if !r.Closed {
			version := NewEvidenceSnapshot(r.Fact).key()
			out.ByVersion[version] = append(out.ByVersion[version], SubjectProjection{Name: r.Head.Name, Revisions: r.Revisions})
		}
		if r.Fact.ID != "" {
			out.ByID[r.Fact.ID] = append(out.ByID[r.Fact.ID], key)
		}
	}
	return out, nil
}

func (s *Store) hydrateSubjectFactsTx(tx *bolt.Tx, namespace string, facts []Fact) error {
	view := s.subjectReadView(tx, namespace)
	if view.indexErr != nil {
		return view.indexErr
	}
	projectSubjectFacts(view.byVersion, facts)
	return nil
}

func projectSubjectFacts(byVersion map[string][]SubjectProjection, facts []Fact) {
	for i := range facts {
		facts[i].Subjects = append([]SubjectProjection(nil), byVersion[NewEvidenceSnapshot(facts[i]).key()]...)
	}
}

func writeSubjectHeadTx(tx *bolt.Tx, namespace, key string, head subjectHead, added *int64) error {
	b, err := evidenceNamespace(tx, bucketSubjectHeads, namespace, true)
	if err != nil {
		return err
	}
	if b.Bucket([]byte(key)) != nil {
		return fmt.Errorf("unexpected bucket in subject heads")
	}
	data, err := json.Marshal(head)
	if err != nil {
		return err
	}
	old := b.Get([]byte(key))
	delta := len(data) - len(old)
	if old == nil {
		delta += len(key)
	}
	if delta > 0 {
		*added += int64(delta)
	}
	return b.Put([]byte(key), data)
}

func (s *Store) advanceSubjectTx(tx *bolt.Tx, namespace, key, name string, prior *resolvedSubject, fact *Fact, added *int64) error {
	snapshot := NewEvidenceSnapshot(*fact)
	records, _ := evidenceNamespace(tx, bucketLineageRecords, namespace, false)
	_, found, err := lineageRecordAt(records, namespace, snapshot.key())
	if err != nil {
		return err
	}
	if !found {
		if err := writeLineageTx(tx, snapshot, nil, lineageRecord{Head: lineageRef(snapshot)}, added); err != nil {
			return err
		}
	}
	owner := subjectOwnerPrefix + key
	if prior != nil && prior.Head.State == "active" {
		if err := releaseEvidenceTx(tx, namespace, prior.Head.Reference.key(namespace), owner, s.now().UTC()); err != nil {
			return err
		}
	}
	bytes, err := evidenceRootAddedBytesTx(tx, snapshot, owner)
	if err != nil {
		return err
	}
	*added += bytes
	if err := rootEvidenceTx(tx, snapshot, owner); err != nil {
		return err
	}
	head := subjectHead{Name: name, Reference: lineageRef(snapshot), State: "active"}
	if prior != nil {
		if prior.Revisions == ^uint64(0) {
			return fmt.Errorf("subject revision counter overflow")
		}
		head.Revisions = prior.Revisions + 1
	}
	return writeSubjectHeadTx(tx, namespace, key, head, added)
}

// closeSubjectsForFactTx records retirement before deleting a live canonical
// head. It releases only this metadata owner's root; PR-D owns the history.
func (s *Store) closeSubjectsForFactTx(tx *bolt.Tx, namespace, id string) error {
	idx, err := resolveSubjectIndexTx(tx, namespace)
	if err != nil {
		return err
	}
	var added int64
	for _, key := range idx.ByID[id] {
		r := idx.Heads[key]
		h := r.Head
		if err := releaseEvidenceTx(tx, namespace, h.Reference.key(namespace), subjectOwnerPrefix+key, s.now().UTC()); err != nil {
			return err
		}
		h.State = "closed"
		h.Revisions = r.Revisions
		if err := writeSubjectHeadTx(tx, namespace, key, h, &added); err != nil {
			return err
		}
	}
	if added > 0 {
		return checkEvidenceQuotaTx(tx, namespace, added)
	}
	return nil
}

func subjectIndexFingerprint(idx subjectIndex) string {
	var keys []string
	for key := range idx.Heads {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var pieces []string
	for _, key := range keys {
		r := idx.Heads[key]
		ref := r.Head.Reference
		if !r.Closed && r.Unresolved == nil {
			ref = lineageRef(NewEvidenceSnapshot(r.Fact))
		}
		raw, _ := json.Marshal(struct {
			Key, Name  string
			Ref        LineageReference
			Revisions  uint64
			Closed     bool
			Unresolved string
			Observed   string
		}{key, r.Head.Name, ref, r.Revisions, r.Closed, subjectIssueReason(r), r.UnresolvedFingerprint})
		pieces = append(pieces, string(raw))
	}
	return strings.Join(pieces, "\n")
}

func subjectIssueReason(subject resolvedSubject) string {
	if subject.Unresolved == nil {
		return ""
	}
	return subject.Unresolved.Error()
}

// The exact head path identifies a damaged version without trusting the
// invalid snapshot's bytes. Call this only after a snapshot read failed.
func damagedSubjectSnapshotTx(tx *bolt.Tx, namespace, version string) (LineageReference, bool) {
	idx, err := resolveSubjectIndexTx(tx, namespace)
	if err != nil {
		return LineageReference{}, false
	}
	for _, head := range idx.Heads {
		if ref := head.DamagedSnapshot; ref != nil && ref.key(namespace) == version {
			return *ref, true
		}
	}
	return LineageReference{}, false
}
