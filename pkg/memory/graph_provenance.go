package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketGraphRevisions  = []byte("kg_source_revisions")
	bucketGraphCleanup    = []byte("kg_cleanup_pending")
	graphTargetKey        = []byte("kg_provenance_target")
	errGraphTargetChanged = errors.New("graph provenance target changed")
)

// GraphSupport identifies an exact canonical content revision. Restoring
// previously held text creates a new digest, so delayed cleanup cannot remove
// the restored support.
type GraphSupport struct {
	Namespace string `json:"namespace"`
	FactID    string `json:"fact_id"`
	Digest    string `json:"digest"`
}

// GraphProvenanceWriter extends GraphAccessor with exact graph receipts.
// Tx methods use the supplied transaction. Non-Tx methods must be idempotent
// for the same support and serve configurations with a separate graph DB.
type GraphProvenanceWriter interface {
	OwnsGraphTx(tx *bolt.Tx) bool
	WriteGraphExtractionTx(tx *bolt.Tx, refs []EntityRef, links []EntityLink, source GraphSupport) error
	RemoveGraphSupportTx(tx *bolt.Tx, source GraphSupport) error
	WriteGraphExtraction(refs []EntityRef, links []EntityLink, source GraphSupport) error
	RemoveGraphSupport(source GraphSupport) error
}

// SeparateGraphIdentity is required for a provenance writer that cannot use
// the canonical transaction. Its identity persists across reopen and differs
// between independent graph databases.
type SeparateGraphIdentity interface {
	GraphIdentity() (string, error)
}

const sharedGraphTarget = "shared"

func (s *Store) graphTarget(writer GraphProvenanceWriter) (string, error) {
	var shared bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		shared = writer.OwnsGraphTx(tx)
		return nil
	}); err != nil {
		return "", err
	}
	if shared {
		return sharedGraphTarget, nil
	}
	identified, ok := writer.(SeparateGraphIdentity)
	if !ok {
		return "", errors.New("separate graph requires a durable identity")
	}
	identity, err := identified.GraphIdentity()
	if err != nil {
		return "", err
	}
	if identity == "" {
		return "", errors.New("separate graph returned an empty identity")
	}
	return "external:" + identity, nil
}

func checkGraphTargetTx(tx *bolt.Tx, target string, bind bool) error {
	meta := tx.Bucket(bucketMeta)
	if meta == nil {
		return errors.New("graph provenance requires canonical store metadata")
	}
	if current := meta.Get(graphTargetKey); current != nil {
		if string(current) != target {
			return errGraphTargetChanged
		}
		return nil
	}
	if bind {
		return meta.Put(graphTargetKey, []byte(target))
	}
	return nil
}

type graphProvenanceMigrator interface {
	MigrateGraphProvenance() error
}

type graphRevision struct {
	Digest      string
	Fingerprint [32]byte
}

func graphSourceKey(namespace, factID string) []byte {
	key := make([]byte, 4, 4+len(namespace)+len(factID))
	binary.BigEndian.PutUint32(key, uint32(len(namespace)))
	key = append(key, namespace...)
	return append(key, factID...)
}

func graphFingerprint(f Fact) [32]byte {
	return sha256.Sum256(fmt.Appendf(nil, "%d:%s%d:%s%s",
		len(f.Kind), f.Kind, len(f.SupersededBy), f.SupersededBy, f.Text))
}

func graphRevisionTx(tx *bolt.Tx, namespace, factID string) (graphRevision, error) {
	var revision graphRevision
	if b := tx.Bucket(bucketGraphRevisions); b != nil {
		if raw := b.Get(graphSourceKey(namespace, factID)); raw != nil {
			if err := json.Unmarshal(raw, &revision); err != nil {
				return graphRevision{}, fmt.Errorf("decode graph source revision: %w", err)
			}
			if revision.Digest == "" {
				return graphRevision{}, errors.New("graph source revision lacks a digest")
			}
		}
	}
	return revision, nil
}

// RegisterGraphSupportTx resolves the current revision of an explicitly
// identified fact in a shared facts/graph transaction. Reconciliation
// callers must independently verify that the fact produced the edge.
func RegisterGraphSupportTx(tx *bolt.Tx, namespace, factID string) (GraphSupport, error) {
	f, err := factInTx(tx, namespace, factID)
	if err != nil {
		return GraphSupport{}, err
	}
	return registerGraphRevisionTx(tx, f, sharedGraphTarget)
}

func registerGraphRevisionTx(tx *bolt.Tx, f Fact, target string) (GraphSupport, error) {
	if err := checkGraphTargetTx(tx, target, true); err != nil {
		return GraphSupport{}, err
	}
	revision, err := graphRevisionTx(tx, f.AgentID, f.ID)
	if err != nil {
		return GraphSupport{}, err
	}
	source := GraphSupport{Namespace: f.AgentID, FactID: f.ID, Digest: revision.Digest}
	fingerprint := graphFingerprint(f)
	if source.Digest != "" {
		if revision.Fingerprint == fingerprint {
			return source, nil
		}
		// An older writer can modify a fact without updating the ledger.
		if err := queueGraphCleanupTx(tx, source); err != nil {
			return GraphSupport{}, err
		}
	}
	source.Digest = ulid.Make().String()
	data, err := json.Marshal(graphRevision{Digest: source.Digest, Fingerprint: fingerprint})
	if err != nil {
		return GraphSupport{}, err
	}
	b, err := tx.CreateBucketIfNotExists(bucketGraphRevisions)
	if err != nil {
		return GraphSupport{}, err
	}
	return source, b.Put(graphSourceKey(f.AgentID, f.ID), data)
}

// InspectGraphFactTx inventories an ID-only receipt. Duplicate IDs across
// namespaces are ambiguous. Retired aliases still classify as aliases.
func InspectGraphFactTx(tx *bolt.Tx, factID string) (string, error) {
	parent := tx.Bucket(bucketFacts)
	if parent == nil {
		return "unavailable", nil
	}
	class := "missing"
	err := parent.ForEachBucket(func(namespace []byte) error {
		raw := parent.Bucket(namespace).Get([]byte(factID))
		if raw == nil || class == "ambiguous" {
			return nil
		}
		if class != "missing" {
			class = "ambiguous"
			return nil
		}
		f, err := unmarshalFactLite(raw)
		if err != nil || f.AgentID != string(namespace) || f.ID != factID {
			class = "unreadable"
		} else if f.IsAlias() {
			class = "alias"
		} else {
			class = "fact"
		}
		return nil
	})
	return class, err
}

// ClassifyGraphSupportTx checks an exact receipt against a shared canonical
// snapshot. A graph database without canonical facts reports unavailable.
func ClassifyGraphSupportTx(tx *bolt.Tx, source GraphSupport) (string, error) {
	if tx.Bucket(bucketFacts) == nil {
		return "unavailable", nil
	}
	f, err := factInTx(tx, source.Namespace, source.FactID)
	if errors.Is(err, ErrFactChanged) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	revision, err := graphRevisionTx(tx, source.Namespace, source.FactID)
	if err != nil {
		return "", err
	}
	if source.Digest == "" || revision.Digest != source.Digest || revision.Fingerprint != graphFingerprint(f) {
		return "stale", nil
	}
	if f.IsAlias() {
		return "alias", nil
	}
	return "fact", nil
}

func graphWatermarkKey(namespace, factID string) []byte {
	return []byte(namespace + "\x00" + factID)
}

func provenanceWatermark(source GraphSupport) string {
	return "revision:" + source.Digest
}

func (s *Store) prepareGraphExtraction(ctx context.Context, snapshot Fact, target string) (GraphSupport, error) {
	fingerprint := graphFingerprint(snapshot)
	var done bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		if err := checkGraphTargetTx(tx, target, false); err != nil {
			return err
		}
		revision, err := graphRevisionTx(tx, snapshot.AgentID, snapshot.ID)
		if err != nil || revision.Digest == "" || revision.Fingerprint != fingerprint {
			return err
		}
		source := GraphSupport{Namespace: snapshot.AgentID, FactID: snapshot.ID, Digest: revision.Digest}
		done = string(tx.Bucket(bucketKGExtracted).Get(graphWatermarkKey(source.Namespace, source.FactID))) == provenanceWatermark(source)
		return nil
	}); err != nil || done {
		return GraphSupport{}, err
	}
	var source GraphSupport
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := factInTx(tx, snapshot.AgentID, snapshot.ID)
		if err != nil {
			return err
		}
		if !current.Extractable() || graphFingerprint(current) != fingerprint {
			return ErrFactChanged
		}
		source, err = registerGraphRevisionTx(tx, current, target)
		return err
	})
	return source, err
}

func pendingGraphExtractionTx(tx *bolt.Tx, source GraphSupport) (bool, error) {
	f, err := factInTx(tx, source.Namespace, source.FactID)
	if err != nil {
		return false, err
	}
	revision, err := graphRevisionTx(tx, source.Namespace, source.FactID)
	if err != nil {
		return false, err
	}
	if !f.Extractable() || revision.Digest != source.Digest || revision.Fingerprint != graphFingerprint(f) {
		return false, ErrFactChanged
	}
	return string(tx.Bucket(bucketKGExtracted).Get(graphWatermarkKey(source.Namespace, source.FactID))) != provenanceWatermark(source), nil
}

func (s *Store) writeGraphExtraction(ctx context.Context, writer GraphProvenanceWriter, source GraphSupport, refs []EntityRef, links []EntityLink, target string) error {
	key, watermark := graphWatermarkKey(source.Namespace, source.FactID), []byte(provenanceWatermark(source))
	if target == sharedGraphTarget {
		err := s.db.Update(func(tx *bolt.Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			pending, err := pendingGraphExtractionTx(tx, source)
			if err != nil {
				return err
			}
			if !pending {
				return errFactUnchanged
			}
			if err := writer.WriteGraphExtractionTx(tx, refs, links, source); err != nil {
				return err
			}
			return tx.Bucket(bucketKGExtracted).Put(key, watermark)
		})
		if err == errFactUnchanged {
			return nil
		}
		return err
	}

	// A canonical mutation can enqueue cleanup during the external write.
	// Cleanup delivery waits until that write finishes, preventing a late
	// write from reintroducing an already acknowledged support.
	s.graphDeliveryMu.Lock()
	defer s.graphDeliveryMu.Unlock()
	var pending bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		pending, err = pendingGraphExtractionTx(tx, source)
		return err
	})
	if err != nil || !pending {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writer.WriteGraphExtraction(refs, links, source); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if _, err := pendingGraphExtractionTx(tx, source); err != nil {
			return err
		}
		return tx.Bucket(bucketKGExtracted).Put(key, watermark)
	})
}

// RegisterAttestedGraphSupportTx binds reconciliation evidence to the exact
// kind and text observed by the operator.
func RegisterAttestedGraphSupportTx(tx *bolt.Tx, namespace, factID, kind, textSHA256 string) (GraphSupport, error) {
	f, err := factInTx(tx, namespace, factID)
	if err != nil {
		return GraphSupport{}, err
	}
	if f.Kind != kind || fmt.Sprintf("%x", sha256.Sum256([]byte(f.Text))) != textSHA256 {
		return GraphSupport{}, fmt.Errorf("%w: graph source %s does not match the inspected kind and text", ErrFactChanged, factID)
	}
	return registerGraphRevisionTx(tx, f, sharedGraphTarget)
}

func queueGraphCleanupTx(tx *bolt.Tx, source GraphSupport) error {
	b, err := tx.CreateBucketIfNotExists(bucketGraphCleanup)
	if err != nil {
		return err
	}
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return b.Put([]byte(source.Digest), data)
}

func (s *Store) invalidateGraphSupportTx(tx *bolt.Tx, namespace, factID string) error {
	revision, err := graphRevisionTx(tx, namespace, factID)
	if err != nil || revision.Digest == "" {
		return err
	}
	source := GraphSupport{Namespace: namespace, FactID: factID, Digest: revision.Digest}
	s.mu.RLock()
	writer, _ := s.graphMaintenance.(GraphProvenanceWriter)
	s.mu.RUnlock()
	if writer != nil && string(tx.Bucket(bucketMeta).Get(graphTargetKey)) == sharedGraphTarget && writer.OwnsGraphTx(tx) {
		if err := writer.RemoveGraphSupportTx(tx, source); err != nil {
			return err
		}
	} else if err := queueGraphCleanupTx(tx, source); err != nil {
		return err
	}
	if err := tx.Bucket(bucketGraphRevisions).Delete(graphSourceKey(namespace, factID)); err != nil {
		return err
	}
	return tx.Bucket(bucketKGExtracted).Delete(graphWatermarkKey(namespace, factID))
}

const graphCleanupBatch = 128

func (s *Store) drainGraphCleanup(writer GraphProvenanceWriter) error {
	s.graphDeliveryMu.Lock()
	defer s.graphDeliveryMu.Unlock()
	target, err := s.graphTarget(writer)
	if err != nil {
		return err
	}
	shared := target == sharedGraphTarget
	var sources []GraphSupport
	err = s.db.View(func(tx *bolt.Tx) error {
		if err := checkGraphTargetTx(tx, target, false); err != nil {
			return err
		}
		b := tx.Bucket(bucketGraphCleanup)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for key, value := c.First(); key != nil && len(sources) < graphCleanupBatch; key, value = c.Next() {
			var source GraphSupport
			if err := json.Unmarshal(value, &source); err != nil {
				return fmt.Errorf("decode graph cleanup intent: %w", err)
			}
			if source.Digest != string(key) || source.FactID == "" {
				return errors.New("invalid graph cleanup intent")
			}
			sources = append(sources, source)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, source := range sources {
		if !shared {
			if err := writer.RemoveGraphSupport(source); err != nil {
				return err
			}
		}
		if err := s.db.Update(func(tx *bolt.Tx) error {
			if shared {
				if err := writer.RemoveGraphSupportTx(tx, source); err != nil {
					return err
				}
			}
			return tx.Bucket(bucketGraphCleanup).Delete([]byte(source.Digest))
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) extractGraphRevision(ctx context.Context, extractor TypedEntityExtractor, writer GraphProvenanceWriter, fact Fact, target string) error {
	source, err := s.prepareGraphExtraction(ctx, fact, target)
	if err != nil || source.Digest == "" {
		return err
	}
	refs, links, err := extractor.ExtractTyped(fact.Text)
	if err != nil {
		return err
	}
	return s.writeGraphExtraction(ctx, writer, source, refs, links, target)
}

// GraphCleanupPendingTx counts exact cleanup intents in the canonical store.
func GraphCleanupPendingTx(tx *bolt.Tx) int {
	if b := tx.Bucket(bucketGraphCleanup); b != nil {
		return b.Stats().KeyN
	}
	return 0
}
