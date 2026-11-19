package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

type ReviewOverview struct {
	Pending      int `json:"pending"`
	Stale        int `json:"stale"`
	Rejected     int `json:"rejected"`
	Unresolved   int `json:"unresolved"`
	Resolved     int `json:"resolved"`
	Dismissed    int `json:"dismissed"`
	LogicalBytes int `json:"logical_bytes"`
}

func currentReviewSourceTx(tx *bolt.Tx, namespace, id, key string) (string, error) {
	root := tx.Bucket(bucketFacts)
	if root == nil {
		return "missing", nil
	}
	rows := root.Bucket([]byte(namespace))
	if rows == nil || rows.Get([]byte(id)) == nil {
		return "missing", nil
	}
	fact, err := unmarshalFactLite(rows.Get([]byte(id)))
	if err != nil {
		return "", err
	}
	if fact.ID != id || fact.AgentID != namespace {
		return "", errors.New("review source identity mismatch")
	}
	if len(fact.Text) > ReviewMaxSourceBytes || NewEvidenceSnapshot(fact).key() != key {
		return "changed", nil
	}
	if fact.IsSuperseded() {
		return "retired", nil
	}
	return "current", nil
}

func reviewStateRank(state string) int {
	switch state {
	case "missing":
		return 3
	case "changed":
		return 2
	case "retired":
		return 1
	default:
		return 0
	}
}

func reviewGroupStateTx(tx *bolt.Tx, entry storedReview) ([]string, string, error) {
	states := make([]string, len(entry.Group))
	aggregate := "current"
	for i, key := range entry.Group {
		state, err := currentReviewSourceTx(tx, entry.Record.Namespace, entry.GroupIDs[i], key)
		if err != nil {
			return nil, "", err
		}
		states[i] = state
		if reviewStateRank(state) > reviewStateRank(aggregate) {
			aggregate = state
		}
	}
	return states, aggregate, nil
}

func projectReviewTx(tx *bolt.Tx, entry storedReview, full bool) (ReviewRecord, error) {
	states, aggregate, err := reviewGroupStateTx(tx, entry)
	if err != nil {
		return ReviewRecord{}, err
	}
	record := entry.Record
	record.SourceState = aggregate
	if record.Status == "pending" && aggregate != "current" {
		record.Status = "stale"
	}
	if !full {
		return ReviewRecord{
			ID: record.ID, Namespace: record.Namespace, Kind: record.Kind, Status: record.Status,
			Resolution: record.Resolution, SourceState: record.SourceState, CreatedAt: record.CreatedAt,
			UpdatedAt: record.UpdatedAt, Observations: record.Observations,
		}, nil
	}

	snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, record.Namespace, false)
	if err != nil {
		return ReviewRecord{}, err
	}
	if len(entry.Batch) != len(record.BatchCreatedAt) || len(entry.Batch) != len(record.BatchIDs) ||
		len(entry.Batch) != len(record.BatchVersions) {
		return ReviewRecord{}, errors.New("review batch evidence is inconsistent")
	}
	observed := make(map[string]time.Time, len(entry.Batch))
	for i, key := range entry.Batch {
		if evidenceIdentityKey(record.Namespace, record.BatchIDs[i], record.BatchVersions[i]) != key {
			return ReviewRecord{}, errors.New("review batch source identity mismatch")
		}
		observed[key] = record.BatchCreatedAt[i]
	}
	inGroup := make(map[string]bool, len(entry.Group))
	for i, key := range entry.Group {
		snapshot, err := evidenceEntry(snapshots, key)
		if err != nil {
			return ReviewRecord{}, err
		}
		if snapshot.Snapshot.FactID != entry.GroupIDs[i] || snapshot.Snapshot.Namespace != record.Namespace {
			return ReviewRecord{}, errors.New("review snapshot does not match the source group")
		}
		at, ok := observed[key]
		if !ok {
			return ReviewRecord{}, errors.New("review source is outside the observed batch")
		}
		snapshot.Snapshot.CreatedAt = at
		record.Sources = append(record.Sources, ReviewSource{Snapshot: snapshot.Snapshot, State: states[i]})
		inGroup[key] = true
	}
	// The initial batch takes precedence. For evidence added later, use its
	// first retained observation in journal order. The shared snapshot's
	// stored timestamp may come from a different record.
	for _, observation := range record.History {
		if len(observation.BatchIDs) != len(observation.BatchVersions) ||
			len(observation.BatchIDs) != len(observation.BatchCreatedAt) {
			return ReviewRecord{}, errors.New("review history evidence is inconsistent")
		}
		for i, id := range observation.BatchIDs {
			key := evidenceIdentityKey(record.Namespace, id, observation.BatchVersions[i])
			if _, exists := observed[key]; !exists {
				observed[key] = observation.BatchCreatedAt[i]
			}
		}
	}
	for _, key := range entry.Evidence {
		if inGroup[key] {
			continue
		}
		snapshot, err := evidenceEntry(snapshots, key)
		if err != nil {
			return ReviewRecord{}, err
		}
		if snapshot.Snapshot.Namespace != record.Namespace {
			return ReviewRecord{}, errors.New("review snapshot is outside the namespace")
		}
		at, ok := observed[key]
		if !ok {
			return ReviewRecord{}, errors.New("review snapshot is outside the retained observations")
		}
		snapshot.Snapshot.CreatedAt = at
		record.AdditionalEvidence = append(record.AdditionalEvidence, snapshot.Snapshot)
	}
	return record, nil
}

func (s *Store) ReviewList(ctx context.Context, namespace string) ([]ReviewRecord, error) {
	if !reviewValidText(namespace, 512) {
		return nil, reviewInvalid("namespace is required")
	}
	var result []ReviewRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		buckets, err := reviewBucketsTx(tx, namespace, false)
		if err != nil || buckets.records == nil {
			return err
		}
		return buckets.records.ForEach(func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry, err := getStoredReview(buckets.records, string(key))
			if err != nil {
				return err
			}
			if entry.Record.Namespace != namespace {
				return errors.New("review record is outside the namespace")
			}
			record, err := projectReviewTx(tx, entry, false)
			if err == nil {
				result = append(result, record)
			}
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result, ctx.Err()
}

func (s *Store) ReviewInspect(ctx context.Context, namespace, id string) (ReviewRecord, error) {
	if !reviewValidText(namespace, 512) || !reviewValidText(id, 128) {
		return ReviewRecord{}, reviewInvalid("namespace and record ID are required")
	}
	var record ReviewRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		buckets, err := reviewBucketsTx(tx, namespace, false)
		if err != nil {
			return err
		}
		entry, err := getStoredReview(buckets.records, id)
		if err != nil {
			return err
		}
		if entry.Record.Namespace != namespace {
			return errors.New("review record is outside the namespace")
		}
		record, err = projectReviewTx(tx, entry, true)
		return err
	})
	if err != nil {
		return ReviewRecord{}, err
	}
	return record, nil
}

func (s *Store) ReviewOverview(ctx context.Context) (ReviewOverview, error) {
	return ReadReviewOverview(ctx, s.db)
}

func ReadReviewOverview(ctx context.Context, db *bolt.DB) (ReviewOverview, error) {
	var result ReviewOverview
	err := db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		root := tx.Bucket(bucketReviewJournal)
		if root == nil {
			return nil
		}
		return root.ForEachBucket(func(namespace []byte) error {
			buckets, err := reviewBucketsTx(tx, string(namespace), false)
			if err != nil {
				return err
			}
			usage, err := reviewQuotaTx(tx, string(namespace), buckets.records)
			if err != nil {
				return err
			}
			result.LogicalBytes += usage.bytes
			return buckets.records.ForEach(func(key, _ []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				entry, err := getStoredReview(buckets.records, string(key))
				if err != nil {
					return err
				}
				if entry.Record.Namespace != string(namespace) {
					return fmt.Errorf("review record %s is outside the namespace", key)
				}
				record, err := projectReviewTx(tx, entry, false)
				if err != nil {
					return err
				}
				switch record.Status {
				case "pending":
					result.Pending++
				case "stale":
					result.Stale++
				case "rejected":
					result.Rejected++
				}
				switch record.Resolution {
				case "unresolved":
					result.Unresolved++
				case "resolved":
					result.Resolved++
				case "dismissed":
					result.Dismissed++
				}
				return nil
			})
		})
	})
	if err != nil {
		return ReviewOverview{}, err
	}
	return result, nil
}

func addReviewNoticeIndex(index *bolt.Bucket, id string, factIDs []string) error {
	for _, factID := range factIDs {
		fact, err := index.CreateBucketIfNotExists([]byte(factID))
		if err != nil {
			return err
		}
		if err := fact.Put([]byte(id), []byte{1}); err != nil {
			return err
		}
	}
	return nil
}

func removeReviewNoticeIndex(index *bolt.Bucket, id string, factIDs []string) error {
	for _, factID := range factIDs {
		key := []byte(factID)
		fact := index.Bucket(key)
		if fact == nil {
			continue
		}
		if err := fact.Delete([]byte(id)); err != nil {
			return err
		}
		if first, _ := fact.Cursor().First(); first == nil {
			if err := index.DeleteBucket(key); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReviewNotices returns at most four typed open conflicts for canonical IDs
// already selected by the caller. Neither provider notes nor unseen source
// IDs are returned.
func (s *Store) ReviewNotices(ctx context.Context, namespace string, factIDs []string) ([]ReviewNotice, error) {
	if len(factIDs) == 0 {
		return nil, ctx.Err()
	}
	var result []ReviewNotice
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		buckets, err := reviewBucketsTx(tx, namespace, false)
		if err != nil || buckets.notices == nil {
			return err
		}
		visible := make(map[string]bool, len(factIDs))
		for _, id := range factIDs {
			visible[id] = true
		}
		seen := make(map[string]bool, 4)
		for _, factID := range factIDs {
			fact := buckets.notices.Bucket([]byte(factID))
			if fact == nil {
				continue
			}
			cursor := fact.Cursor()
			for key, _ := cursor.First(); key != nil && len(result) < 4; key, _ = cursor.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				id := string(key)
				if seen[id] {
					continue
				}
				seen[id] = true
				entry, err := getStoredReview(buckets.records, id)
				if err != nil {
					return err
				}
				if entry.Record.Namespace != namespace || entry.Record.Kind != ReviewKindConflict || entry.Record.Resolution != "unresolved" {
					return errors.New("open conflict index does not match its record")
				}
				_, aggregate, err := reviewGroupStateTx(tx, entry)
				if err != nil {
					return err
				}
				notice := ReviewNotice{ID: id, SourceState: aggregate}
				for _, sourceID := range entry.GroupIDs {
					if visible[sourceID] {
						notice.SourceIDs = append(notice.SourceIDs, sourceID)
					}
				}
				if len(notice.SourceIDs) > 0 {
					result = append(result, notice)
				}
			}
			if len(result) == 4 {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
