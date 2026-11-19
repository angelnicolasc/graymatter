package memory

import (
	"crypto/sha256"
	"slices"

	bolt "go.etcd.io/bbolt"
)

// A read view is immutable once it is published. It contains only data used by
// retrieval, rather than the canonical facts needed by the write resolver.
type subjectReadView struct {
	byVersion   map[string][]SubjectProjection
	facts       map[string]subjectFactReadView
	fingerprint [sha256.Size]byte
	issues      SubjectIssues
	shortIDs    map[string]string
	indexErr    error
	prefixErr   error
}

// Ranking reads the lite row for every candidate. These immutable fields can
// be reused for a verified subject version without reading its lineage record
// again. The bbolt transaction stamp also covers its citation and lineage data.
type subjectFactReadView struct {
	lineage             LineageProjection
	evidence            *Citation
	citationUnavailable bool
	subjects            []SubjectProjection
	err                 error
}

func (view subjectReadView) hydrateRankedFact(tx *bolt.Tx, namespace string, key []byte, fact *Fact) error {
	exactRow := view.prefixErr == nil && fact.AgentID == namespace && fact.ID == string(key)
	if exactRow {
		if saved, ok := view.facts[fact.ID]; ok {
			if saved.err != nil {
				return saved.err
			}
			fact.LineageProjection = saved.lineage
			fact.DerivedFrom = slices.Clone(saved.lineage.DerivedFrom)
			fact.Replaces = slices.Clone(saved.lineage.Replaces)
			fact.LegacySources = slices.Clone(saved.lineage.LegacySources)
			fact.Evidence = nil
			if saved.evidence != nil {
				evidence := *saved.evidence
				fact.Evidence = &evidence
			}
			fact.CitationUnavailable = saved.citationUnavailable
			fact.Subjects = slices.Clone(saved.subjects)
			return nil
		}
	}
	if err := hydrateLineageTx(tx, fact); err != nil {
		return err
	}
	if !exactRow {
		fact.Subjects = slices.Clone(view.byVersion[NewEvidenceSnapshot(*fact).key()])
	}
	return nil
}

var emptySubjectReadView = subjectReadView{fingerprint: sha256.Sum256(nil)}

func buildSubjectReadView(tx *bolt.Tx, namespace string) subjectReadView {
	idx, err := resolveSubjectIndexTx(tx, namespace)
	if err != nil {
		return subjectReadView{indexErr: err}
	}
	view := subjectReadView{
		byVersion:   idx.ByVersion,
		fingerprint: sha256.Sum256([]byte(subjectIndexFingerprint(idx))),
	}
	if len(idx.ByVersion) != 0 {
		view.facts = make(map[string]subjectFactReadView, len(idx.ByVersion))
		for _, head := range idx.Heads {
			if head.Closed || head.Unresolved != nil {
				continue
			}
			fact := head.Fact
			if _, exists := view.facts[fact.ID]; exists {
				continue
			}
			subjects := idx.ByVersion[NewEvidenceSnapshot(fact).key()]
			err := hydrateLineageTx(tx, &fact)
			view.facts[fact.ID] = subjectFactReadView{
				lineage: fact.LineageProjection, evidence: fact.Evidence,
				citationUnavailable: fact.CitationUnavailable, subjects: subjects, err: err,
			}
		}
	}
	view.issues.add(namespace, idx)
	if namespaceHasRows(tx, bucketEvidenceSnapshots, namespace) {
		ids, err := namespaceFactIDsTx(tx, namespace)
		view.prefixErr = err
		if err == nil {
			view.shortIDs = shortFactIDs(ids)
		}
	} else {
		// With no retained IDs, neighboring keys are enough to find the
		// shortest prefix. Validate canonical identities before trusting
		// those keys for this snapshot.
		_, view.prefixErr = canonicalFactsTx(tx, namespace)
	}
	return view
}

func (s *Store) initSubjectReadViews() error {
	return s.db.View(func(tx *bolt.Tx) error {
		views := make(map[string]subjectReadView)
		for _, bucket := range [][]byte{bucketSubjectHeads, bucketFacts, bucketEvidenceSnapshots} {
			if err := walkLineageNamespaces(tx.Bucket(bucket), "", func(namespace string, _ *bolt.Bucket) error {
				if _, exists := views[namespace]; !exists {
					views[namespace] = buildSubjectReadView(tx, namespace)
				}
				return nil
			}); err != nil {
				// Let the existing read and inspection paths surface malformed
				// buckets without making unrelated store operations unavailable.
				continue
			}
		}
		s.subjectReadTx = tx.ID()
		s.subjectReadViews = views
		return nil
	})
}

func (s *Store) subjectReadView(tx *bolt.Tx, namespace string) subjectReadView {
	s.subjectReadMu.Lock()
	view, found := s.subjectReadViews[namespace]
	current := s.subjectReadTx == tx.ID()
	s.subjectReadMu.Unlock()
	if current && found {
		return view
	}
	// The public DB handle also serves other packages. Any transaction not
	// published by this Store has to be checked before reusing a projection.
	// An older, concurrent read never publishes over a newer committed view.
	if lineageRowBucket(tx, namespace) == nil && !namespaceHasRows(tx, bucketSubjectHeads, namespace) &&
		!namespaceHasRows(tx, bucketEvidenceSnapshots, namespace) {
		return emptySubjectReadView
	}
	view = buildSubjectReadView(tx, namespace)
	s.publishSubjectReadView(tx.ID(), namespace, view)
	return view
}

func (s *Store) publishSubjectReadView(id int, namespace string, view subjectReadView) {
	s.subjectReadMu.Lock()
	defer s.subjectReadMu.Unlock()
	if id < s.subjectReadTx {
		return
	}
	if id > s.subjectReadTx {
		s.subjectReadViews = nil
		s.subjectReadTx = id
	}
	if s.subjectReadViews == nil {
		s.subjectReadViews = make(map[string]subjectReadView)
	}
	s.subjectReadViews[namespace] = view
}

// advanceSubjectReadTx runs only for a committed transaction. Keeping the
// previous version requires proof that it was this transaction's predecessor.
func (s *Store) advanceSubjectReadTx(id int) {
	s.subjectReadMu.Lock()
	defer s.subjectReadMu.Unlock()
	if s.subjectReadTx == id-1 {
		s.subjectReadTx = id
	}
}

func (s *Store) updateSubjectReadView(namespace string, preservesRowIdentity bool, write func(*bolt.Tx) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		id := tx.ID()
		s.subjectReadMu.Lock()
		previous, found := s.subjectReadViews[namespace]
		verified := s.subjectReadTx == id-1 && found && previous.prefixErr == nil && previous.indexErr == nil
		s.subjectReadMu.Unlock()
		canKeepRowVerification := preservesRowIdentity && (verified || lineageRowBucket(tx, namespace) == nil)
		if err := write(tx); err != nil {
			return err
		}
		view := emptySubjectReadView
		if !canKeepRowVerification || namespaceHasRows(tx, bucketSubjectHeads, namespace) ||
			namespaceHasRows(tx, bucketEvidenceSnapshots, namespace) {
			view = buildSubjectReadView(tx, namespace)
		}
		tx.OnCommit(func() {
			s.subjectReadMu.Lock()
			defer s.subjectReadMu.Unlock()
			if id < s.subjectReadTx {
				return
			}
			if s.subjectReadTx < id-1 || s.subjectReadViews == nil {
				s.subjectReadViews = make(map[string]subjectReadView)
			}
			s.subjectReadTx = id
			s.subjectReadViews[namespace] = view
		})
		return nil
	})
}

// Access tracking and reformulation bookkeeping change neither IDs nor exact
// versions. Their commits can advance the transaction stamp without rebuilding.
func (s *Store) updateSubjectReadMetadata(write func(*bolt.Tx) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := write(tx); err != nil {
			return err
		}
		id := tx.ID()
		tx.OnCommit(func() { s.advanceSubjectReadTx(id) })
		return nil
	})
}

func namespaceHasRows(tx *bolt.Tx, bucket []byte, namespace string) bool {
	parent := tx.Bucket(bucket)
	if parent == nil {
		return false
	}
	child := parent.Bucket([]byte(namespace))
	if child == nil {
		return false
	}
	key, _ := child.Cursor().First()
	return key != nil
}

// A collision in sorted IDs can only involve the immediate predecessor or
// successor. Retained snapshots have their own verified prefix map.
func (view subjectReadView) shortID(tx *bolt.Tx, namespace, id string) string {
	if view.shortIDs != nil {
		return view.shortIDs[id]
	}
	rows := lineageRowBucket(tx, namespace)
	if rows == nil {
		return ""
	}
	cursor := rows.Cursor()
	key, _ := cursor.Seek([]byte(id))
	if string(key) != id {
		return ""
	}
	neighbors := []string{id}
	if next, _ := cursor.Next(); next != nil {
		neighbors = append(neighbors, string(next))
	}
	cursor.Seek([]byte(id))
	if prev, _ := cursor.Prev(); prev != nil {
		neighbors = append(neighbors, string(prev))
	}
	slices.Sort(neighbors)
	return shortFactIDs(neighbors)[id]
}
