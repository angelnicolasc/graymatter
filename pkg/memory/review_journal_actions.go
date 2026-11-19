package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"
)

const reviewActionBudget = 4 << 10

func validateReviewAction(action ReviewAction) error {
	if action.Decision != "" && !reviewValidText(action.Decision, ReviewMaxNoteBytes) {
		return reviewInvalid("decision exceeds its limit or is blank")
	}
	if len(action.References) > ReviewMaxSources {
		return reviewInvalid("too many decision references")
	}
	seen := make(map[string]bool, len(action.References))
	for _, reference := range action.References {
		if !reviewValidText(reference, 256) || seen[reference] {
			return reviewInvalid("decision references must be unique and bounded")
		}
		seen[reference] = true
	}
	switch action.Action {
	case "reject":
		if len(action.References) != 0 {
			return reviewInvalid("reject does not accept references")
		}
	case "resolve", "dismiss":
		if action.Decision == "" || len(action.References) == 0 {
			return reviewInvalid("resolution requires an explicit decision and references")
		}
	case "purge":
		if action.Decision != "" || len(action.References) != 0 {
			return reviewInvalid("purge does not accept decision metadata")
		}
	default:
		return reviewInvalid("unknown review action")
	}
	raw, err := json.Marshal(action)
	if err != nil {
		return err
	}
	if len(raw) > reviewActionBudget {
		return reviewInvalid("serialized review action exceeds four KiB")
	}
	return nil
}

func reviewIsClosed(record ReviewRecord) bool {
	return record.Status == "rejected" || record.Resolution == "resolved" || record.Resolution == "dismissed"
}

func (s *Store) ReviewApply(ctx context.Context, namespace, id string, action ReviewAction) error {
	if s.readOnly {
		return ErrStoreReadOnly
	}
	if !reviewValidText(namespace, 512) || !reviewValidText(id, 128) {
		return reviewInvalid("namespace and record ID are required")
	}
	if err := validateReviewAction(action); err != nil {
		return err
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		journal, err := reviewBucketsTx(tx, namespace, false)
		if err != nil {
			return err
		}
		entry, err := getStoredReview(journal.records, id)
		if err != nil {
			return err
		}
		if entry.Record.Namespace != namespace {
			return reviewInvalid("record is outside the namespace")
		}
		now := s.now().UTC()
		if action.Action == "purge" {
			if !reviewIsClosed(entry.Record) {
				return fmt.Errorf("%w: only closed records can be purged", ErrReviewTransition)
			}
			if err := purgeReviewTx(tx, journal, entry, now); err != nil {
				return err
			}
			return ctx.Err()
		}
		if err := applyReviewDecision(journal, &entry, action, now); err != nil {
			return err
		}
		if _, err := saveReviewRecordTx(tx, journal.records, entry); err != nil {
			return err
		}
		if err := enforceReviewQuotaTx(tx, namespace, journal.records, now); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err == errFactUnchanged {
		return nil
	}
	return err
}

func applyReviewDecision(journal reviewBuckets, entry *storedReview, action ReviewAction, now time.Time) error {
	record := &entry.Record
	status, resolution := record.Status, record.Resolution
	switch action.Action {
	case "reject":
		if record.Kind != ReviewKindSummary {
			return fmt.Errorf("%w: reject requires a summary candidate", ErrReviewTransition)
		}
		status = "rejected"
	case "resolve":
		if record.Kind == ReviewKindSummary {
			return fmt.Errorf("%w: resolve requires a conflict", ErrReviewTransition)
		}
		resolution = "resolved"
	case "dismiss":
		if record.Kind == ReviewKindSummary {
			return fmt.Errorf("%w: dismiss requires a conflict", ErrReviewTransition)
		}
		resolution = "dismissed"
	}
	if reviewIsClosed(*record) {
		if record.Status == status && record.Resolution == resolution && record.Decision == action.Decision &&
			slices.Equal(record.References, action.References) {
			return errFactUnchanged
		}
		return fmt.Errorf("%w: review record is already closed", ErrReviewTransition)
	}
	if record.Kind == ReviewKindConflict {
		if err := removeReviewNoticeIndex(journal.notices, record.ID, entry.GroupIDs); err != nil {
			return err
		}
	}
	record.Status, record.Resolution = status, resolution
	record.Decision, record.References = action.Decision, slices.Clone(action.References)
	if !now.Before(record.UpdatedAt) {
		record.UpdatedAt = now
	}
	return nil
}

func purgeReviewTx(tx *bolt.Tx, journal reviewBuckets, entry storedReview, now time.Time) error {
	record := entry.Record
	if string(journal.dedupe.Get([]byte(entry.Dedupe))) != record.ID {
		return reviewInvalid("record dedupe identity is inconsistent")
	}
	for _, key := range entry.Evidence {
		if err := releaseEvidenceTx(tx, record.Namespace, key, reviewEvidenceOwnerPrefix+record.ID, now); err != nil {
			return err
		}
	}
	if err := journal.dedupe.Delete([]byte(entry.Dedupe)); err != nil {
		return err
	}
	if err := journal.records.Delete([]byte(record.ID)); err != nil {
		return err
	}
	return collectLineageTx(tx, record.Namespace, now)
}
