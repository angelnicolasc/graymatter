package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

const (
	DefaultMaxSourceBytes        = 512
	DefaultMaxQuoteBytes         = 2048
	DefaultEvidenceResponseBytes = 8192
)

var ErrCitationUnsupported = errors.New("citation evidence unsupported: update and restart the store daemon")
var bucketCitations = []byte("citations_v1")

// Citation is writer-declared, untrusted evidence. CapturedAt records receipt,
// not verification. Strings are immutable UTF-8, without normalization.
type Citation struct {
	Source     string    `json:"source"`
	Quote      string    `json:"quote"`
	CapturedAt time.Time `json:"captured_at"`
}

type CitationReference struct {
	Namespace   string `json:"namespace"`
	FactID      string `json:"fact_id"`
	ContentHash string `json:"content_hash"`
}

type CitationResult struct {
	Reference CitationReference `json:"reference"`
	Evidence  *Citation         `json:"evidence"`
}

type storedCitation struct {
	Version   int               `json:"version"`
	Reference CitationReference `json:"reference"`
	Evidence  Citation          `json:"evidence"`
}

func (*Store) SupportsCitationEvidence() bool { return true }

func (o WriteOptions) HasEvidence() bool { return o.Source != nil || o.Quote != nil }

// Clone snapshots caller-owned options before callbacks or asynchronous work.
func (o WriteOptions) Clone() WriteOptions {
	if o.Subject != nil {
		value := *o.Subject
		o.Subject = &value
	}
	if o.Replaces != nil {
		o.Replaces = append([]string{}, o.Replaces...)
	}
	if o.Confidence != nil {
		v := *o.Confidence
		o.Confidence = &v
	}
	if o.Source != nil {
		v := *o.Source
		o.Source = &v
	}
	if o.Quote != nil {
		v := *o.Quote
		o.Quote = &v
	}
	return o
}

func validateCitationStrings(source, quote string) error {
	if !utf8.ValidString(source) || !utf8.ValidString(quote) {
		return errors.New("source and quote must be valid UTF-8")
	}
	if len(source) > DefaultMaxSourceBytes {
		return fmt.Errorf("source exceeds limit of %d bytes", DefaultMaxSourceBytes)
	}
	return nil
}

func (s *Store) validateCitationOptions(o WriteOptions) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.Quote != nil && len(*o.Quote) > s.cfg.MaxQuoteBytes {
		return fmt.Errorf("quote exceeds limit of %d bytes", s.cfg.MaxQuoteBytes)
	}
	return nil
}

func citationEntry(tx *bolt.Tx, namespace, key string) (*storedCitation, error) {
	b, err := evidenceNamespace(tx, bucketCitations, namespace, false)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, nil
	}
	if b.Bucket([]byte(key)) != nil {
		return nil, errors.New("unexpected bucket in citation payload")
	}
	if b.Get([]byte(key)) == nil {
		return nil, nil
	}
	var entry storedCitation
	if err = json.Unmarshal(b.Get([]byte(key)), &entry); err != nil {
		return nil, fmt.Errorf("decode citation: %w", err)
	}
	r := entry.Reference
	hash, e := hex.DecodeString(r.ContentHash)
	if entry.Version != 1 || r.Namespace != namespace || r.FactID == "" || e != nil || len(hash) != 32 || evidenceIdentityKey(r.Namespace, r.FactID, r.ContentHash) != key {
		return nil, errors.New("citation identity mismatch")
	}
	if err = validateCitationStrings(entry.Evidence.Source, entry.Evidence.Quote); err != nil {
		return nil, err
	}
	if entry.Evidence.CapturedAt.IsZero() {
		return nil, errors.New("citation capture time is missing")
	}
	return &entry, nil
}

var errCitationSnapshotMissing = errors.New("citation has no retained version snapshot")

func citationAtTx(tx *bolt.Tx, namespace, key string) (*Citation, error) {
	entry, err := citationEntry(tx, namespace, key)
	if err != nil || entry == nil {
		return nil, err
	}
	snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	if snapshots == nil || snapshots.Get([]byte(key)) == nil {
		return nil, errCitationSnapshotMissing
	}
	snapshot, err := evidenceEntry(snapshots, key)
	if err != nil {
		return nil, err
	}
	if snapshot.Snapshot.Namespace != namespace {
		return nil, errors.New("citation snapshot namespace mismatch")
	}
	return &entry.Evidence, nil
}

func hydrateCitationTx(tx *bolt.Tx, f *Fact) error {
	f.Evidence = nil
	f.CitationUnavailable = false
	key := NewEvidenceSnapshot(*f).key()
	evidence, err := citationAtTx(tx, f.AgentID, key)
	if err != nil {
		// r4 can sweep expired evidence for a retired pinned row without
		// knowing the citation bucket. A read-only open cannot prune that
		// stale citation, and it must not lose access to the canonical row.
		if f.IsSuperseded() && errors.Is(err, errCitationSnapshotMissing) {
			f.CitationUnavailable = true
			return nil
		}
		if _, damaged := damagedSubjectSnapshotTx(tx, f.AgentID, key); damaged {
			// The subject warning describes this exact missing or damaged
			// snapshot. Never present its unverified citation as evidence.
			f.CitationUnavailable = true
			return nil
		}
		return err
	}
	f.Evidence = evidence
	return nil
}

// recordCitationTx participates in the caller's atomic write and its cumulative
// logical-byte quota. Existing immutable evidence is never overwritten.
func (s *Store) recordCitationTx(tx *bolt.Tx, f *Fact, o WriteOptions, added *int64) error {
	if !o.HasEvidence() {
		f.Evidence = nil
		return nil
	}
	if err := s.validateCitationOptions(o); err != nil {
		return err
	}
	if f.IsAlias() {
		return errors.New("citation evidence is not supported on aliases")
	}
	citation := Citation{CapturedAt: s.now().UTC()}
	if o.Source != nil {
		citation.Source = *o.Source
	}
	if o.Quote != nil {
		citation.Quote = *o.Quote
	}
	snapshot := NewEvidenceSnapshot(*f)
	key := snapshot.key()
	existing, err := citationEntry(tx, f.AgentID, key)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Evidence.Source != citation.Source || existing.Evidence.Quote != citation.Quote {
			return errors.New("citation is immutable; create a new revision")
		}
		f.Evidence = &existing.Evidence
		return nil
	}
	records, _ := evidenceNamespace(tx, bucketLineageRecords, f.AgentID, false)
	record, found, err := lineageRecordAt(records, f.AgentID, key)
	if err != nil {
		return err
	}
	if !found {
		record = lineageRecord{Head: lineageRef(snapshot)}
		if err = writeLineageTx(tx, snapshot, nil, record, added); err != nil {
			return err
		}
	}
	f.LineageProjection = record.projection()
	payload, err := json.Marshal(storedCitation{Version: 1, Reference: CitationReference{Namespace: f.AgentID, FactID: f.ID, ContentHash: snapshot.ContentHash}, Evidence: citation})
	if err != nil {
		return err
	}
	b, err := evidenceNamespace(tx, bucketCitations, f.AgentID, true)
	if err != nil {
		return err
	}
	if err = b.Put([]byte(key), payload); err != nil {
		return err
	}
	*added += int64(len(key) + len(payload))
	f.Evidence = &citation
	return nil
}

func (s *Store) FetchCitation(ctx context.Context, namespace, factID, contentHash string) (CitationResult, error) {
	if err := ctx.Err(); err != nil {
		return CitationResult{}, err
	}
	hash, err := hex.DecodeString(contentHash)
	if strings.TrimSpace(namespace) == "" || factID == "" || err != nil || len(hash) != 32 {
		return CitationResult{}, errors.New("citation fetch requires namespace, fact_id and exact content_hash")
	}
	result := CitationResult{Reference: CitationReference{Namespace: namespace, FactID: factID, ContentHash: contentHash}}
	err = s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var e error
		result.Evidence, e = citationAtTx(tx, namespace, evidenceIdentityKey(namespace, factID, contentHash))
		if e != nil {
			return e
		}
		if result.Evidence == nil {
			return ErrFactNotFound
		}
		return nil
	})
	if err != nil {
		return CitationResult{}, err
	}
	return result, nil
}

// pruneCitationsTx follows the snapshot GC, including snapshots already removed
// by an older writer. A live canonical version is never silently discarded.
func pruneCitationsTx(tx *bolt.Tx, namespace string) (bool, error) {
	b, _ := evidenceNamespace(tx, bucketCitations, namespace, false)
	if b == nil {
		return false, nil
	}
	snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
	var expired [][]byte
	err := b.ForEach(func(key, _ []byte) error {
		if snapshots != nil && snapshots.Get(key) != nil {
			return nil
		}
		if _, damaged := damagedSubjectSnapshotTx(tx, namespace, string(key)); damaged {
			return nil
		}
		entry, err := citationEntry(tx, namespace, string(key))
		if err != nil {
			return err
		}
		if rows := lineageRowBucket(tx, namespace); rows != nil {
			if raw := rows.Get([]byte(entry.Reference.FactID)); raw != nil {
				f, e := unmarshalFactLite(raw)
				if e != nil {
					return e
				}
				if !f.IsSuperseded() && NewEvidenceSnapshot(f).key() == string(key) {
					return errors.New("live citation is missing its version snapshot")
				}
			}
		}
		expired = append(expired, append([]byte(nil), key...))
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, key := range expired {
		if err = b.Delete(key); err != nil {
			return false, err
		}
	}
	return len(expired) > 0, nil
}

func validateCitationBackupTx(ctx context.Context, tx *bolt.Tx) error {
	return walkLineageNamespaces(tx.Bucket(bucketCitations), "", func(ns string, b *bolt.Bucket) error {
		return b.ForEach(func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err := citationAtTx(tx, ns, string(key))
			return err
		})
	})
}

func factLexicalText(f Fact) string {
	if f.Evidence == nil || f.Evidence.Quote == "" {
		return f.Text
	}
	return f.Text + "\n" + f.Evidence.Quote
}

func (p recallPolicy) factKey(f Fact) string {
	if !p.citationIdentity {
		return f.Text
	}
	if f.Evidence == nil && len(f.Subjects) == 0 {
		return "text\x00" + f.Text
	}
	return "citation\x00" + NewEvidenceSnapshot(f).key()
}

// Use the same namespace RRF rule over canonical evidence identities. Plain
// facts retain text-level deduplication and its project preference. Separate
// cited versions may share words, but never borrow each other's evidence.
func fuseCitationFacts(project, shared []Fact, topK int, policy recallPolicy) ([]string, []Fact) {
	byKey := make(map[string]Fact, len(project)+len(shared))
	projectKeys := make([]string, 0, len(project))
	sharedKeys := make([]string, 0, len(shared))
	for _, f := range project {
		key := policy.factKey(f)
		projectKeys = append(projectKeys, key)
		byKey[key] = f
	}
	for _, f := range shared {
		key := policy.factKey(f)
		sharedKeys = append(sharedKeys, key)
		if _, exists := byKey[key]; !exists {
			byKey[key] = f
		}
	}
	keys := fuseRecallResults(projectKeys, sharedKeys, topK)
	texts := make([]string, 0, len(keys))
	facts := make([]Fact, 0, len(keys))
	for _, key := range keys {
		f := byKey[key]
		texts = append(texts, f.Text)
		facts = append(facts, f)
	}
	return texts, facts
}

// RecallForHooks preserves legacy ranking, graph strings and access accounting;
// it annotates selected canonical snapshots before the caller's token budget.
func (s *Store) RecallForHooks(ctx context.Context, namespace, query string, topK int) ([]string, error) {
	return s.RecallForHooksWithOptions(ctx, namespace, query, topK, RecallOptions{})
}

// RecallForHooksWithOptions lets transports preserve their existing bound
// legacy policy (RPC binds zero; direct Store calls use its configured default).
func (s *Store) RecallForHooksWithOptions(ctx context.Context, namespace, query string, topK int, options RecallOptions) ([]string, error) {
	policy, err := s.resolveRecallPolicy(options)
	if err != nil {
		return nil, err
	}
	if topK < 0 {
		topK = 0
	}
	fence, err := s.subjectReadFence(ctx, []string{namespace})
	if err != nil {
		return nil, err
	}
	result, _, activity, err := s.recallDetailedCollectWithPolicy(ctx, namespace, query, topK, policy, deferRecallActivity)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool)
	subjects := make(map[string][]SubjectProjection)
	if err := s.db.View(func(tx *bolt.Tx) error {
		views, err := s.checkSubjectReadFenceTx(tx, fence)
		if err != nil {
			return err
		}
		for _, f := range activity.facts {
			if f.Evidence != nil {
				present[f.Text] = true
			}
			for _, p := range views[namespace].byVersion[NewEvidenceSnapshot(f).key()] {
				found := false
				for _, prior := range subjects[f.Text] {
					if prior == p {
						found = true
					}
				}
				if !found {
					subjects[f.Text] = append(subjects[f.Text], p)
				}
			}
		}
		return ctx.Err()
	}); err != nil {
		return nil, err
	}
	for i, text := range result {
		result[i] = text + subjectHookSuffix(subjects[text])
		if present[text] {
			result[i] += " [evidence]"
		}
	}
	s.touchRecallActivity(activity)
	return result, nil
}
