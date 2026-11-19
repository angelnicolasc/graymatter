package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
)

const (
	ReviewKindSummary  = "summary"
	ReviewKindConflict = "conflict"
	ReviewKindUntyped  = "untyped"

	ReviewMaxSources      = 12
	ReviewMaxSourceBytes  = 16 << 10
	ReviewMaxGroups       = 12
	ReviewMaxNoteBytes    = 2 << 10
	ReviewMaxRecordBytes  = 128 << 10
	ReviewMaxRecords      = 256
	ReviewMaxJournalBytes = 8 << 20

	reviewHistoryLimit = 4
)

var (
	ErrReviewNotFound   = errors.New("review record not found")
	ErrReviewQuota      = errors.New("review journal quota exceeded")
	ErrReviewInvalid    = errors.New("invalid review record")
	ErrReviewTransition = errors.New("invalid review transition")

	bucketReviewJournal = []byte("review_journal_v1")
	bucketReviewRecords = []byte("records")
	bucketReviewDedupe  = []byte("dedupe")
	bucketReviewNotices = []byte("open_conflicts")
)

type ReviewSubmission struct {
	Namespace     string
	Provider      string
	Model         string
	PromptVersion string
	Snapshots     []EvidenceSnapshot
	Findings      []ReviewFinding
}

type ReviewFinding struct {
	Kind      string
	Summary   string
	SourceIDs []string
	Note      string
}

type ReviewSource struct {
	Snapshot EvidenceSnapshot `json:"snapshot"`
	State    string           `json:"state"`
}

type ReviewObservation struct {
	At             time.Time   `json:"at"`
	Provider       string      `json:"provider"`
	Model          string      `json:"model"`
	BatchHash      string      `json:"batch_hash"`
	BatchIDs       []string    `json:"batch_ids,omitempty"`
	BatchVersions  []string    `json:"batch_versions,omitempty"`
	BatchCreatedAt []time.Time `json:"batch_created_at,omitempty"`
}

type ReviewRecord struct {
	ID                 string              `json:"id"`
	Namespace          string              `json:"namespace"`
	Kind               string              `json:"kind"`
	Status             string              `json:"status,omitempty"`
	Resolution         string              `json:"resolution,omitempty"`
	SourceState        string              `json:"source_state"`
	Summary            string              `json:"summary,omitempty"`
	Note               string              `json:"note,omitempty"`
	Provider           string              `json:"provider,omitempty"`
	Model              string              `json:"model,omitempty"`
	PromptVersion      string              `json:"prompt_version,omitempty"`
	Sources            []ReviewSource      `json:"sources,omitempty"`
	AdditionalEvidence []EvidenceSnapshot  `json:"additional_evidence,omitempty"`
	BatchIDs           []string            `json:"batch_ids,omitempty"`
	BatchVersions      []string            `json:"batch_versions,omitempty"`
	BatchCreatedAt     []time.Time         `json:"batch_created_at,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	Observations       uint64              `json:"observations"`
	History            []ReviewObservation `json:"history,omitempty"`
	Decision           string              `json:"decision,omitempty"`
	References         []string            `json:"references,omitempty"`
}

type ReviewAction struct {
	Action     string   `json:"action"`
	Decision   string   `json:"decision,omitempty"`
	References []string `json:"references,omitempty"`
}

type ReviewNotice struct {
	ID          string   `json:"conflict_id"`
	SourceIDs   []string `json:"source_ids"`
	SourceState string   `json:"source_state"`
}

type storedReview struct {
	Record   ReviewRecord `json:"record"`
	Dedupe   string       `json:"dedupe"`
	Group    []string     `json:"group"`
	GroupIDs []string     `json:"group_ids"`
	Batch    []string     `json:"batch"`
	Evidence []string     `json:"evidence"`
}

type preparedReview struct {
	finding ReviewFinding
	dedupe  string
	group   []string
	ids     []string
}

func reviewValidText(value string, maxBytes int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maxBytes && utf8.ValidString(value)
}

func reviewInvalid(message string) error {
	return fmt.Errorf("%w: %s", ErrReviewInvalid, message)
}

func prepareReview(in ReviewSubmission) ([]preparedReview, error) {
	if !reviewValidText(in.Namespace, 512) || !reviewValidText(in.Provider, 64) ||
		!reviewValidText(in.Model, 256) || !reviewValidText(in.PromptVersion, 128) {
		return nil, reviewInvalid("namespace, provider, model and prompt version are required")
	}
	if len(in.Snapshots) == 0 || len(in.Snapshots) > ReviewMaxSources {
		return nil, reviewInvalid("observed batch must contain one to twelve sources")
	}
	if len(in.Findings) == 0 || len(in.Findings) > ReviewMaxGroups {
		return nil, reviewInvalid("response must contain one to twelve findings")
	}

	batch := make(map[string]EvidenceSnapshot, len(in.Snapshots))
	for _, snapshot := range in.Snapshots {
		if snapshot.Version != EvidenceSnapshotVersion || snapshot.Namespace != in.Namespace ||
			!reviewValidText(snapshot.FactID, 128) || !reviewValidText(snapshot.Text, ReviewMaxSourceBytes) ||
			snapshot.Kind == KindAlias || !utf8.ValidString(snapshot.Kind) || !utf8.ValidString(snapshot.Confidence) ||
			snapshot.ContentHash != evidenceHash("graymatter-evidence-content-v1", snapshot.Text, snapshot.Kind, snapshot.Confidence) {
			return nil, reviewInvalid("source snapshot is malformed or outside the namespace")
		}
		if _, exists := batch[snapshot.FactID]; exists {
			return nil, reviewInvalid("observed batch contains a duplicate source")
		}
		batch[snapshot.FactID] = snapshot
	}

	prepared := make([]preparedReview, 0, len(in.Findings))
	seen := make(map[string]bool, len(in.Findings))
	for _, finding := range in.Findings {
		ids, err := reviewFindingIDs(finding, batch)
		if err != nil {
			return nil, err
		}
		sort.Strings(ids)
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = batch[id].key()
		}
		parts := []string{in.Namespace, finding.Kind, in.PromptVersion}
		switch finding.Kind {
		case ReviewKindSummary:
			parts = append(parts, in.Provider, in.Model)
		case ReviewKindUntyped:
			// Legacy observations have no group identity beyond the
			// observed batch. Distinct hypotheses on that batch stay separate.
			parts = append(parts, finding.Note)
		}
		dedupe := evidenceHash("graymatter-review-record-v1", append(parts, keys...)...)
		if seen[dedupe] {
			return nil, reviewInvalid("response contains duplicate finding groups")
		}
		seen[dedupe] = true
		prepared = append(prepared, preparedReview{finding, dedupe, keys, ids})
	}
	return prepared, nil
}

func reviewFindingIDs(finding ReviewFinding, batch map[string]EvidenceSnapshot) ([]string, error) {
	switch finding.Kind {
	case ReviewKindSummary:
		if !reviewValidText(finding.Summary, ReviewMaxRecordBytes) || finding.Note != "" || len(finding.SourceIDs) == 0 {
			return nil, reviewInvalid("summary requires text and source references")
		}
	case ReviewKindConflict:
		if finding.Summary != "" || !reviewValidText(finding.Note, ReviewMaxNoteBytes) || len(finding.SourceIDs) < 2 {
			return nil, reviewInvalid("typed conflict requires a bounded note and at least two sources")
		}
	case ReviewKindUntyped:
		if finding.Summary != "" || !reviewValidText(finding.Note, ReviewMaxNoteBytes) || len(finding.SourceIDs) != 0 {
			return nil, reviewInvalid("untyped conflict requires a bounded note without source IDs")
		}
		ids := make([]string, 0, len(batch))
		for id := range batch {
			ids = append(ids, id)
		}
		return ids, nil
	default:
		return nil, reviewInvalid("unknown finding kind")
	}
	if len(finding.SourceIDs) > len(batch) {
		return nil, reviewInvalid("finding exceeds the observed source count")
	}
	seen := make(map[string]bool, len(finding.SourceIDs))
	for _, id := range finding.SourceIDs {
		if _, exists := batch[id]; !exists || seen[id] {
			return nil, reviewInvalid("finding contains an unknown or duplicate source ID")
		}
		seen[id] = true
	}
	return slices.Clone(finding.SourceIDs), nil
}

type reviewBuckets struct {
	records, dedupe, notices *bolt.Bucket
}

func reviewBucketsTx(tx *bolt.Tx, namespace string, create bool) (reviewBuckets, error) {
	owner, err := evidenceNamespace(tx, bucketReviewJournal, namespace, create)
	if err != nil || owner == nil {
		return reviewBuckets{}, err
	}
	if create {
		for _, name := range [][]byte{bucketReviewRecords, bucketReviewDedupe, bucketReviewNotices} {
			if _, err := owner.CreateBucketIfNotExists(name); err != nil {
				return reviewBuckets{}, err
			}
		}
	}
	buckets := reviewBuckets{owner.Bucket(bucketReviewRecords), owner.Bucket(bucketReviewDedupe), owner.Bucket(bucketReviewNotices)}
	if buckets.records == nil || buckets.dedupe == nil || buckets.notices == nil {
		return reviewBuckets{}, errors.New("review journal is missing a required bucket")
	}
	return buckets, nil
}

func getStoredReview(rows *bolt.Bucket, id string) (storedReview, error) {
	var entry storedReview
	if rows == nil || rows.Get([]byte(id)) == nil {
		return entry, ErrReviewNotFound
	}
	if err := json.Unmarshal(rows.Get([]byte(id)), &entry); err != nil {
		return entry, fmt.Errorf("decode review record: %w", err)
	}
	if entry.Record.ID != id || entry.Dedupe == "" || len(entry.Group) == 0 || len(entry.Group) != len(entry.GroupIDs) {
		return entry, errors.New("review record identity or source group mismatch")
	}
	return entry, nil
}

type reviewUsage struct {
	records, bytes, reserved int
}

func reviewQuotaTx(tx *bolt.Tx, namespace string, rows *bolt.Bucket) (reviewUsage, error) {
	var usage reviewUsage
	if rows != nil {
		if err := rows.ForEach(func(key, raw []byte) error {
			if raw == nil {
				return errors.New("unexpected bucket in review records")
			}
			var flags struct {
				Record struct {
					Status     string `json:"status"`
					Resolution string `json:"resolution"`
				} `json:"record"`
			}
			if err := json.Unmarshal(raw, &flags); err != nil {
				return err
			}
			usage.records++
			usage.bytes += len(key) + len(raw)
			if !reviewIsClosed(ReviewRecord{Status: flags.Record.Status, Resolution: flags.Record.Resolution}) {
				usage.reserved += reviewActionBudget
			}
			return nil
		}); err != nil {
			return reviewUsage{}, err
		}
	}
	snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	if err != nil || snapshots == nil {
		return usage, err
	}
	owners, err := evidenceNamespace(tx, bucketEvidenceOwners, namespace, false)
	if err != nil {
		return reviewUsage{}, err
	}
	err = snapshots.ForEach(func(key, raw []byte) error {
		if raw == nil {
			return errors.New("unexpected bucket in evidence snapshots")
		}
		if owners != nil {
			if rooted := owners.Bucket(key); rooted != nil {
				if hasReviewEvidenceOwner(rooted) {
					usage.bytes += len(key) + len(raw)
				}
				return nil
			}
		}
		// A snapshot released by its last review owner remains in the
		// journal budget during grace. Lineage-only grace is charged only
		// to the shared evidence budget.
		var metadata struct {
			LineageOnly bool `json:"lineage_only"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return err
		}
		if !metadata.LineageOnly {
			usage.bytes += len(key) + len(raw)
		}
		return nil
	})
	return usage, err
}

func enforceReviewQuotaTx(tx *bolt.Tx, namespace string, rows *bolt.Bucket, now time.Time) error {
	usage, err := reviewQuotaTx(tx, namespace, rows)
	if err != nil {
		return err
	}
	if usage.records > ReviewMaxRecords {
		return fmt.Errorf("%w: %d retained records exceed %d", ErrReviewQuota, usage.records, ReviewMaxRecords)
	}
	if usage.bytes+usage.reserved <= ReviewMaxJournalBytes {
		return nil
	}
	if err := collectLineageTx(tx, namespace, now); err != nil {
		return err
	}
	usage, err = reviewQuotaTx(tx, namespace, rows)
	if err != nil {
		return err
	}
	if usage.bytes+usage.reserved > ReviewMaxJournalBytes {
		return fmt.Errorf("%w: %d logical bytes including decision capacity exceed %d", ErrReviewQuota, usage.bytes+usage.reserved, ReviewMaxJournalBytes)
	}
	return nil
}

// ReviewCommit records a validated response without changing canonical facts.
// A source that changed or disappeared while the provider was working causes
// the entire response to be rejected in the journal write transaction.
func (s *Store) ReviewCommit(ctx context.Context, in ReviewSubmission) ([]ReviewRecord, error) {
	if s.readOnly {
		return nil, ErrStoreReadOnly
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prepared, err := prepareReview(in)
	if err != nil {
		return nil, err
	}

	var result []ReviewRecord
	err = s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, snapshot := range in.Snapshots {
			current, err := factInTx(tx, in.Namespace, snapshot.FactID)
			if err != nil {
				return err
			}
			if current.IsSuperseded() || NewEvidenceSnapshot(current).ContentHash != snapshot.ContentHash ||
				!current.CreatedAt.Equal(snapshot.CreatedAt) {
				return fmt.Errorf("%w: review source %s no longer matches the observed revision", ErrFactChanged, snapshot.FactID)
			}
		}
		journal, err := reviewBucketsTx(tx, in.Namespace, true)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		batchKeys := make([]string, 0, len(in.Snapshots))
		batchIDs := make([]string, 0, len(in.Snapshots))
		batchVersions := make([]string, 0, len(in.Snapshots))
		batchCreatedAt := make([]time.Time, 0, len(in.Snapshots))
		batchHashFields := make([]string, 0, 2*len(in.Snapshots))
		for _, snapshot := range in.Snapshots {
			batchKeys = append(batchKeys, snapshot.key())
			batchIDs = append(batchIDs, snapshot.FactID)
			batchVersions = append(batchVersions, snapshot.ContentHash)
			batchCreatedAt = append(batchCreatedAt, snapshot.CreatedAt.UTC())
			batchHashFields = append(batchHashFields, snapshot.key(), snapshot.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
		observation := ReviewObservation{At: now, Provider: in.Provider, Model: in.Model,
			BatchHash: evidenceHash("graymatter-review-batch-v1", batchHashFields...),
			BatchIDs:  batchIDs, BatchVersions: batchVersions, BatchCreatedAt: batchCreatedAt}
		lineage, err := evidenceNamespace(tx, bucketLineageRecords, in.Namespace, false)
		if err != nil {
			return err
		}
		changes := reviewEvidenceChanges{lineage: lineage}
		for _, finding := range prepared {
			record, err := storeReviewFindingTx(tx, journal, in, finding, batchKeys, observation, &changes)
			if err != nil {
				return err
			}
			result = append(result, record)
		}
		if changes.lineageChanged {
			if err := collectLineageTx(tx, in.Namespace, now); err != nil {
				return err
			}
		}
		if err := enforceReviewQuotaTx(tx, in.Namespace, journal.records, now); err != nil {
			return err
		}
		if changes.added > 0 {
			if err := checkEvidenceQuotaTx(tx, in.Namespace, changes.added); err != nil {
				if errors.Is(err, ErrEvidenceQuota) {
					return fmt.Errorf("%w: %w", ErrReviewQuota, err)
				}
				return err
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type reviewEvidenceChanges struct {
	lineage        *bolt.Bucket
	added          int64
	lineageChanged bool
}

func (changes *reviewEvidenceChanges) mark(key string) {
	if changes.lineage != nil && changes.lineage.Get([]byte(key)) != nil {
		changes.lineageChanged = true
	}
}

func storeReviewFindingTx(tx *bolt.Tx, journal reviewBuckets, in ReviewSubmission, f preparedReview, batchKeys []string, observation ReviewObservation, changes *reviewEvidenceChanges) (ReviewRecord, error) {
	dedupeKey := []byte(f.dedupe)
	id := string(journal.dedupe.Get(dedupeKey))
	var entry storedReview
	if id == "" {
		id = ulid.Make().String()
		entry = storedReview{
			Dedupe: f.dedupe, Group: f.group, GroupIDs: f.ids, Batch: batchKeys,
			Record: ReviewRecord{
				ID: id, Namespace: in.Namespace, Kind: f.finding.Kind, SourceState: "current",
				Summary: f.finding.Summary, Note: f.finding.Note, Provider: in.Provider,
				Model: in.Model, PromptVersion: in.PromptVersion, CreatedAt: observation.At,
				BatchIDs: observation.BatchIDs, BatchVersions: observation.BatchVersions, BatchCreatedAt: observation.BatchCreatedAt,
			},
		}
		if f.finding.Kind == ReviewKindSummary {
			entry.Record.Status = "pending"
		} else {
			entry.Record.Resolution = "unresolved"
		}
		if err := journal.dedupe.Put(dedupeKey, []byte(id)); err != nil {
			return ReviewRecord{}, err
		}
		if f.finding.Kind == ReviewKindConflict {
			if err := addReviewNoticeIndex(journal.notices, id, entry.GroupIDs); err != nil {
				return ReviewRecord{}, err
			}
		}
	} else {
		var err error
		entry, err = getStoredReview(journal.records, id)
		if err != nil {
			return ReviewRecord{}, err
		}
		if entry.Dedupe != f.dedupe || entry.Record.Namespace != in.Namespace {
			return ReviewRecord{}, errors.New("review dedupe index mismatch")
		}
	}
	for i, key := range batchKeys {
		if slices.Contains(entry.Evidence, key) {
			continue
		}
		owner := reviewEvidenceOwnerPrefix + id
		added, err := evidenceRootAddedBytesTx(tx, in.Snapshots[i], owner)
		if err != nil {
			return ReviewRecord{}, err
		}
		if err := rootEvidenceTx(tx, in.Snapshots[i], owner); err != nil {
			return ReviewRecord{}, err
		}
		changes.added += added
		changes.mark(key)
		entry.Evidence = append(entry.Evidence, key)
	}
	if entry.Record.Observations < ^uint64(0) {
		entry.Record.Observations++
	}
	if !observation.At.Before(entry.Record.UpdatedAt) {
		entry.Record.UpdatedAt = observation.At
	}
	entry.Record.History = append(entry.Record.History, observation)
	if excess := len(entry.Record.History) - reviewHistoryLimit; excess > 0 {
		entry.Record.History = entry.Record.History[excess:]
		if err := trimReviewEvidenceTx(tx, &entry, observation.At, changes); err != nil {
			return ReviewRecord{}, err
		}
	}
	return saveReviewRecordTx(tx, journal.records, entry)
}

func retainedReviewEvidence(entry storedReview) (map[string]bool, error) {
	retained := make(map[string]bool, len(entry.Evidence))
	for _, key := range entry.Batch {
		retained[key] = true
	}
	for _, observation := range entry.Record.History {
		if len(observation.BatchIDs) != len(observation.BatchVersions) || len(observation.BatchIDs) != len(observation.BatchCreatedAt) {
			return nil, errors.New("review history source IDs and versions do not match")
		}
		for i, id := range observation.BatchIDs {
			key := evidenceIdentityKey(entry.Record.Namespace, id, observation.BatchVersions[i])
			retained[key] = true
		}
	}
	return retained, nil
}

func trimReviewEvidenceTx(tx *bolt.Tx, entry *storedReview, now time.Time, changes *reviewEvidenceChanges) error {
	retained, err := retainedReviewEvidence(*entry)
	if err != nil {
		return err
	}
	keep := entry.Evidence[:0]
	for _, key := range entry.Evidence {
		if retained[key] {
			keep = append(keep, key)
			continue
		}
		if err := releaseEvidenceTx(tx, entry.Record.Namespace, key, reviewEvidenceOwnerPrefix+entry.Record.ID, now); err != nil {
			return err
		}
		changes.mark(key)
	}
	entry.Evidence = keep
	return nil
}

func saveReviewRecordTx(tx *bolt.Tx, rows *bolt.Bucket, entry storedReview) (ReviewRecord, error) {
	record, err := projectReviewTx(tx, entry, true)
	if err != nil {
		return ReviewRecord{}, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return ReviewRecord{}, err
	}
	limit := ReviewMaxRecordBytes
	if !reviewIsClosed(record) {
		limit -= reviewActionBudget
	}
	if len(data) > limit {
		return ReviewRecord{}, fmt.Errorf("%w: %d record bytes exceed %d", ErrReviewQuota, len(data), limit)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return ReviewRecord{}, err
	}
	if len(raw) > limit {
		return ReviewRecord{}, fmt.Errorf("%w: %d journal record bytes exceed %d", ErrReviewQuota, len(raw), limit)
	}
	if err := rows.Put([]byte(record.ID), raw); err != nil {
		return ReviewRecord{}, err
	}
	return record, nil
}
