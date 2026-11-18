package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// validateReviewEvidenceBackupTx verifies that the journal and evidence index
// agree in both directions. The caller separately validates snapshot integrity.
func validateReviewEvidenceBackupTx(ctx context.Context, tx *bolt.Tx) error {
	expectedOwners := make(map[string]int)
	if err := walkLineageNamespaces(tx.Bucket(bucketReviewJournal), "", func(namespace string, _ *bolt.Bucket) error {
		journal, err := reviewBucketsTx(tx, namespace, false)
		if err != nil {
			return err
		}
		owners, err := evidenceNamespace(tx, bucketEvidenceOwners, namespace, false)
		if err != nil {
			return err
		}
		return journal.records.ForEach(func(id, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry, err := getStoredReview(journal.records, string(id))
			if err != nil {
				return err
			}
			if entry.Record.Namespace != namespace {
				return errors.New("backup review record namespace mismatch")
			}
			retained, err := retainedReviewEvidence(entry)
			if err != nil {
				return err
			}
			if len(retained) != len(entry.Evidence) {
				return fmt.Errorf("backup review %s has incomplete retained evidence", id)
			}
			if _, err := projectReviewTx(tx, entry, true); err != nil {
				return fmt.Errorf("backup review %s: %w", id, err)
			}
			owner := []byte(reviewEvidenceOwnerPrefix + string(id))
			for _, key := range entry.Evidence {
				if !retained[key] {
					return fmt.Errorf("backup review %s has an invalid retained evidence key", id)
				}
				delete(retained, key)
				if owners == nil {
					return fmt.Errorf("backup review %s is missing an evidence owner", id)
				}
				rooted := owners.Bucket([]byte(key))
				if rooted == nil || rooted.Get(owner) == nil {
					return fmt.Errorf("backup review %s is missing an evidence owner", id)
				}
			}
			expectedOwners[namespace] += len(entry.Evidence)
			return nil
		})
	}); err != nil {
		return err
	}

	prefix := []byte(reviewEvidenceOwnerPrefix)
	return walkLineageNamespaces(tx.Bucket(bucketEvidenceOwners), "", func(namespace string, owners *bolt.Bucket) error {
		var actualOwners int
		if err := owners.ForEach(func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			rooted := owners.Bucket(key)
			if rooted == nil {
				return errors.New("backup contains an invalid evidence owner index")
			}
			c := rooted.Cursor()
			for owner, value := c.Seek(prefix); bytes.HasPrefix(owner, prefix); owner, value = c.Next() {
				if len(value) != 1 || value[0] != 1 {
					return errors.New("backup contains an invalid review evidence owner")
				}
				actualOwners++
			}
			return nil
		}); err != nil {
			return err
		}
		if actualOwners != expectedOwners[namespace] {
			return fmt.Errorf("backup contains an unclaimed review evidence owner in namespace %q", namespace)
		}
		return nil
	})
}
