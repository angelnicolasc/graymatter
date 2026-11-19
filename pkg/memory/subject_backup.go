package memory

import (
	"context"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
)

func validateSubjectBackupTx(ctx context.Context, tx *bolt.Tx) error {
	if err := walkLineageNamespaces(tx.Bucket(bucketSubjectHeads), "", func(ns string, _ *bolt.Bucket) error {
		idx, err := resolveSubjectIndexTx(tx, ns)
		if err != nil {
			return err
		}
		owners, err := evidenceNamespace(tx, bucketEvidenceOwners, ns, false)
		if err != nil {
			return err
		}
		for key, resolved := range idx.Heads {
			if err := ctx.Err(); err != nil {
				return err
			}
			if resolved.Unresolved != nil {
				return resolved.Unresolved
			}
			h := resolved.Head
			if h.State == "closed" {
				continue
			}
			if owners == nil {
				return fmt.Errorf("subject evidence owners missing")
			}
			b := owners.Bucket([]byte(h.Reference.key(ns)))
			if b == nil || b.Get([]byte(subjectOwnerPrefix+key)) == nil {
				return fmt.Errorf("subject evidence owner missing for %q", key)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return walkLineageNamespaces(tx.Bucket(bucketEvidenceOwners), "", func(ns string, owners *bolt.Bucket) error {
		heads, err := subjectHeadsTx(tx, ns)
		if err != nil {
			return err
		}
		return owners.ForEach(func(version, _ []byte) error {
			b := owners.Bucket(version)
			if b == nil {
				return fmt.Errorf("malformed evidence owners")
			}
			return b.ForEach(func(owner, _ []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				key, subject := strings.CutPrefix(string(owner), subjectOwnerPrefix)
				if !subject {
					return nil
				}
				h, exists := heads[key]
				if !exists || h.State != "active" || h.Reference.key(ns) != string(version) {
					return fmt.Errorf("unclaimed subject evidence owner %q", owner)
				}
				return nil
			})
		})
	})
}
