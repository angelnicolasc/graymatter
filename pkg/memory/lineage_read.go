package memory

import (
	"context"
	"errors"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// legacyLineageTx returns only observable predecessor relationships from
// historical tombstones. Their original operation and source bytes are
// unknown, even when the current rows are still available.
func legacyLineageTx(tx *bolt.Tx, namespace string) (map[string][]Fact, error) {
	rows := lineageRowBucket(tx, namespace)
	if rows == nil {
		return nil, nil
	}
	incoming := make(map[string][]Fact)
	err := rows.ForEach(func(id, raw []byte) error {
		f, err := unmarshalFactLite(raw)
		if err != nil {
			return err
		}
		if f.AgentID != namespace || f.ID != string(id) {
			return errors.New("legacy fact identity mismatch")
		}
		if f.SupersededBy != "" && f.SupersededBy != SupersededByAgent {
			incoming[f.SupersededBy] = append(incoming[f.SupersededBy], f)
		}
		return nil
	})
	return incoming, err
}

// captureLegacyLineageTx snapshots observable legacy relationships before
// their rows can be pruned. A requested subset also walks its ancestors.
func captureLegacyLineageTx(tx *bolt.Tx, namespace string, now time.Time, roots []string, added *int64) error {
	incoming, err := legacyLineageTx(tx, namespace)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		roots = make([]string, 0, len(incoming))
		for id := range incoming {
			head, err := factInTx(tx, namespace, id)
			if errors.Is(err, ErrFactChanged) {
				continue
			}
			if err != nil {
				return err
			}
			if !head.IsSuperseded() {
				roots = append(roots, id)
			}
		}
		sort.Strings(roots)
	}
	seen := make(map[string]bool)
	for i := 0; i < len(roots); i++ {
		id := roots[i]
		if seen[id] {
			continue
		}
		seen[id] = true
		predecessors := incoming[id]
		if len(predecessors) == 0 || lineageHeadKey(tx, namespace, id) != "" {
			continue
		}
		head, err := factInTx(tx, namespace, id)
		if errors.Is(err, ErrFactChanged) {
			continue // a missing head has no version to assign
		}
		if err != nil {
			return err
		}
		snapshot := NewEvidenceSnapshot(head)
		record := lineageRecord{Head: lineageRef(snapshot), Captured: &now}
		sources := make([]EvidenceSnapshot, 0, len(predecessors))
		for _, f := range predecessors {
			source := NewEvidenceSnapshot(f)
			sources = append(sources, source)
			record.Legacy = append(record.Legacy, lineageRef(source))
			roots = append(roots, f.ID)
		}
		if err := writeLineageTx(tx, snapshot, sources, record, added); err != nil {
			return err
		}
	}
	return nil
}

func lineageInspectionAt(ctx context.Context, tx *bolt.Tx, namespace, key string) (LineageInspection, error) {
	snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	if snapshots == nil || snapshots.Get([]byte(key)) == nil {
		return LineageInspection{}, ErrFactNotFound
	}
	entry, err := evidenceEntry(snapshots, key)
	if err != nil {
		return LineageInspection{}, err
	}
	if entry.Snapshot.Namespace != namespace {
		return LineageInspection{}, errors.New("lineage snapshot namespace mismatch")
	}
	out := LineageInspection{Snapshot: &entry.Snapshot}
	out.ContentHash = entry.Snapshot.ContentHash
	records, _ := evidenceNamespace(tx, bucketLineageRecords, namespace, false)
	record, found, err := lineageRecordAt(records, namespace, key)
	if err != nil {
		return LineageInspection{}, err
	}
	if found {
		out.LineageProjection = record.projection()
		out.CapturedAt = record.Captured
		_, _, status, err := walkLineageTx(ctx, tx, namespace, []string{key})
		if err != nil {
			return LineageInspection{}, err
		}
		if status != "" {
			out.LineageStatus = status
		}
	}
	return out, nil
}

// InspectLineage resolves one exact version without touching recall metadata.
// Without a hash it reads the canonical row or the last known lineage head.
// Pruned source-only evidence is addressed by its exact version reference.
func (s *Store) InspectLineage(ctx context.Context, namespace, factID, contentHash string) (LineageInspection, error) {
	if err := ctx.Err(); err != nil {
		return LineageInspection{}, err
	}
	if namespace == "" || factID == "" {
		return LineageInspection{}, ErrFactNotFound
	}
	var out LineageInspection
	err := s.db.View(func(tx *bolt.Tx) error {
		if contentHash != "" {
			var err error
			out, err = lineageInspectionAt(ctx, tx, namespace, evidenceIdentityKey(namespace, factID, contentHash))
			return err
		}
		key := lineageHeadKey(tx, namespace, factID)
		f, err := factInTx(tx, namespace, factID)
		if err != nil && !errors.Is(err, ErrFactChanged) {
			return err
		}
		if key != "" {
			if err == nil && f.LineageStatus == LineageContentChanged {
				current := NewEvidenceSnapshot(f)
				out = LineageInspection{Snapshot: &current, LineageProjection: f.LineageProjection}
				return nil
			}
			out, err = lineageInspectionAt(ctx, tx, namespace, key)
			return err
		}
		incoming, scanErr := legacyLineageTx(tx, namespace)
		if scanErr != nil {
			return scanErr
		}
		if err != nil && len(incoming[factID]) == 0 {
			return ErrFactNotFound
		}
		if err == nil {
			snapshot := NewEvidenceSnapshot(f)
			out.Snapshot = &snapshot
			out.ContentHash = snapshot.ContentHash
		}
		for _, source := range incoming[factID] {
			out.LegacySources = append(out.LegacySources, LineageReference{FactID: source.ID})
		}
		if len(out.LegacySources) != 0 {
			out.LineageStatus = LineageLegacyIncomplete
		}
		return ctx.Err()
	})
	return out, err
}

func (s *Store) annotateLineageReceipts(ctx context.Context, receipts []RecallReceipt, namespace string) error {
	if len(receipts) == 0 {
		return nil
	}
	return s.db.View(func(tx *bolt.Tx) error {
		for i := range receipts {
			if err := ctx.Err(); err != nil {
				return err
			}
			p := &receipts[i].Provenance
			if p.LineageStatus == LineageContentChanged {
				// A predecessor's tombstone is not backing for the current text.
				p.Supersedes = nil
				continue
			}
			if p.LineageStatus == "" && len(p.Supersedes) > 0 {
				p.LineageStatus = LineageLegacyIncomplete
			}
			if p.ContentHash == "" {
				continue
			}
			key := evidenceIdentityKey(namespace, p.FactID, p.ContentHash)
			_, ids, status, err := walkLineageTx(ctx, tx, namespace, []string{key})
			if err != nil {
				return err
			}
			if status != "" {
				p.LineageStatus = status
			}
			// For a versioned head, the durable graph is the authority.
			p.Supersedes = nil
			seen := map[string]bool{p.FactID: true}
			for _, id := range ids {
				if !seen[id] {
					seen[id] = true
					p.Supersedes = append(p.Supersedes, id)
				}
			}
		}
		return nil
	})
}

// projectLegacyLineage preserves only the historical links visible in the
// rows. Reading an older store never claims that these are typed or versioned.
func projectLegacyLineage(facts []Fact, incoming map[string][]string) {
	for i := range facts {
		f := &facts[i]
		if f.LineageStatus != "" || len(incoming[f.ID]) == 0 {
			continue
		}
		ids := incoming[f.ID]
		sort.Strings(ids)
		f.LineageStatus = LineageLegacyIncomplete
		f.LegacySources = make([]LineageReference, 0, len(ids))
		for _, id := range ids {
			f.LegacySources = append(f.LegacySources, LineageReference{FactID: id})
		}
	}
}

func legacyLineageIDs(facts []Fact) map[string][]string {
	var incoming map[string][]string
	for _, f := range facts {
		if f.SupersededBy == "" || f.SupersededBy == SupersededByAgent {
			continue
		}
		if incoming == nil {
			incoming = make(map[string][]string)
		}
		incoming[f.SupersededBy] = append(incoming[f.SupersededBy], f.ID)
	}
	return incoming
}

// pendingLegacyEvidenceTx keeps only observable historical predecessors that
// still reach a live, uncaptured head. This covers quota failures without
// protecting unrelated rows, and is checked under the prune transaction.
func pendingLegacyEvidenceTx(tx *bolt.Tx, f Fact) (bool, error) {
	destination, err := legacyEvidenceDestinationTx(tx, f, nil)
	if err != nil || destination.version == "" {
		return destination.live, err
	}
	reachable, err := lineageEvidenceRootsTx(tx, f.AgentID)
	return reachable[destination.version], err
}

type legacyEvidenceDestination struct {
	live    bool
	version string
}

// legacyEvidenceDestinationTx identifies where an unrecorded supersession
// ends. A pruned successor can still be reached through its version record.
// Collection shares a cache of forward paths within its transaction.
func legacyEvidenceDestinationTx(tx *bolt.Tx, f Fact, cache map[string]legacyEvidenceDestination) (destination legacyEvidenceDestination, err error) {
	next := f.SupersededBy
	if next == "" || next == SupersededByAgent {
		return legacyEvidenceDestination{}, nil
	}
	// An indexed successor can anchor this history independently, for
	// example through a journal even after its own successor is gone.
	if key := lineageHeadKey(tx, f.AgentID, next); key != "" {
		records, _ := evidenceNamespace(tx, bucketLineageRecords, f.AgentID, false)
		successor, _, err := lineageRecordAt(records, f.AgentID, key)
		if err != nil {
			return legacyEvidenceDestination{}, err
		}
		currentKey := NewEvidenceSnapshot(f).key()
		predecessorKey := lineageHeadKey(tx, f.AgentID, f.ID)
		if predecessorKey == "" {
			predecessorKey = currentKey
		}
		for _, refs := range [][]LineageReference{successor.Replaces, successor.Derived} {
			for _, source := range refs {
				if sourceKey := source.key(f.AgentID); sourceKey == currentKey || sourceKey == predecessorKey {
					return legacyEvidenceDestination{}, nil
				}
			}
		}
		for _, source := range successor.Legacy {
			if source.key(f.AgentID) == predecessorKey {
				return legacyEvidenceDestination{}, nil
			}
		}
		return legacyEvidenceDestination{version: key}, nil
	}
	rows := lineageRowBucket(tx, f.AgentID)
	seen := map[string]bool{f.ID: true}
	defer func() {
		if err == nil && cache != nil {
			for id := range seen {
				if id != f.ID {
					cache[id] = destination
				}
			}
		}
	}()
	for next != "" && next != SupersededByAgent && !seen[next] {
		if key := lineageHeadKey(tx, f.AgentID, next); key != "" {
			return legacyEvidenceDestination{version: key}, nil
		}
		if known, ok := cache[next]; ok {
			return known, nil
		}
		seen[next] = true
		raw := rows.Get([]byte(next))
		if raw == nil {
			return legacyEvidenceDestination{}, nil
		}
		head, err := unmarshalFactLite(raw)
		if err != nil {
			return legacyEvidenceDestination{}, err
		}
		if head.AgentID != f.AgentID || head.ID != next {
			return legacyEvidenceDestination{}, errors.New("legacy target identity mismatch")
		}
		if !head.IsSuperseded() {
			return legacyEvidenceDestination{live: true}, nil
		}
		next = head.SupersededBy
	}
	return legacyEvidenceDestination{}, nil
}

func verifyLineageProjectionTx(ctx context.Context, tx *bolt.Tx, f *Fact) error {
	if f.ContentHash == "" || f.LineageStatus == LineageContentChanged {
		return nil
	}
	key := evidenceIdentityKey(f.AgentID, f.ID, f.ContentHash)
	_, _, status, err := walkLineageTx(ctx, tx, f.AgentID, []string{key})
	if err != nil {
		return err
	}
	if status != "" {
		f.LineageStatus = status
	}
	return nil
}
