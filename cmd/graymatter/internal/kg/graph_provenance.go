package kg

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

const edgeProvenanceVersion = 1

var bucketSupportEdges = []byte("kg_support_edges")

// SourceSupport identifies the exact revision used to derive an edge.
type SourceSupport struct {
	Namespace string `json:"namespace"`
	FactID    string `json:"fact_id"`
	Digest    string `json:"digest"`
}

func (s SourceSupport) valid() bool {
	return s.Namespace != "" && s.FactID != "" && s.Digest != "" &&
		utf8.ValidString(s.Namespace) && utf8.ValidString(s.FactID) && utf8.ValidString(s.Digest)
}

func (s SourceSupport) indexKey() []byte {
	// Struct encoding gives the three variable-length fields unambiguous
	// boundaries. Hashing leaves room for an arbitrarily long edge key.
	data, _ := json.Marshal(s)
	digest := sha256.Sum256(data)
	return digest[:]
}

// EdgeProvenance records the support known to the current writer. LegacySources
// are retained as opaque IDs. Their relationship to exact receipts is unknown.
type EdgeProvenance struct {
	ProvenanceVersion int             `json:"provenance_version"`
	ManualSupport     bool            `json:"manual_support"`
	SourcesComplete   bool            `json:"sources_complete"`
	SourcesTruncated  bool            `json:"sources_truncated"`
	Supports          []SourceSupport `json:"supports,omitempty"`
	LegacySources     []string        `json:"legacy_sources,omitempty"`
	Reason            string          `json:"pending_reason,omitempty"`
}

// UnmarshalJSON ensures a partial or internally inconsistent record cannot
// acquire deletion authority simply because an older writer omitted a field.
func (e *Edge) UnmarshalJSON(data []byte) error {
	type wireEdge Edge
	var value wireEdge
	wire := struct {
		*wireEdge
		Manual    *bool `json:"manual_support"`
		Complete  *bool `json:"sources_complete"`
		Truncated *bool `json:"sources_truncated"`
	}{wireEdge: &value}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = Edge(value)
	p := e.EdgeProvenance
	if p == nil {
		return nil
	}
	p.ManualSupport = wire.Manual != nil && *wire.Manual
	p.SourcesTruncated = wire.Truncated != nil && *wire.Truncated
	p.SourcesComplete = wire.Complete != nil && *wire.Complete
	if wire.Manual == nil || wire.Complete == nil || wire.Truncated == nil ||
		(p.SourcesComplete && (p.SourcesTruncated || len(p.LegacySources) > 0 ||
			!validSupports(p.Supports) || (!p.ManualSupport && len(p.Supports) == 0) ||
			!sameSourceIDs(e.Sources, provenanceIDs(p)))) {
		p.SourcesComplete = false
		p.Reason = "invalid_provenance"
	}
	return nil
}

func sameSourceIDs(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for _, id := range expected {
		if !slices.Contains(actual, id) {
			return false
		}
	}
	return true
}

func validSupports(supports []SourceSupport) bool {
	if len(supports) > maxEdgeSources {
		return false
	}
	seen := make(map[SourceSupport]bool, len(supports))
	for _, source := range supports {
		if !source.valid() || seen[source] {
			return false
		}
		seen[source] = true
	}
	return true
}

// PendingReason returns the persisted provenance classification for inspection.
// ProvenanceReport adds canonical source availability where it can verify it.
func (e Edge) PendingReason() string {
	p := e.EdgeProvenance
	switch {
	case p == nil && len(e.Sources) == 0:
		return "legacy_no_sources"
	case p == nil:
		return "legacy_provenance"
	case p.ProvenanceVersion != edgeProvenanceVersion:
		return "unsupported_provenance_version"
	case p.SourcesTruncated:
		return "sources_truncated"
	case p.Reason != "":
		return p.Reason
	case !p.SourcesComplete:
		return "incomplete_provenance"
	default:
		return ""
	}
}

// Link records manual support when edge has no Sources. A caller with only
// fact IDs supplies legacy, incomplete provenance. Both merge into the edge.
func (g *Graph) Link(edge Edge) error {
	return g.db.Update(func(tx *bolt.Tx) error {
		return linkTx(tx, edge, SourceSupport{}, len(edge.Sources) > 0)
	})
}

// LinkLegacy records an edge from a writer that cannot verify source
// revisions, including writes for which no fact ID was supplied.
func (g *Graph) LinkLegacy(edge Edge) error {
	return g.db.Update(func(tx *bolt.Tx) error {
		return linkTx(tx, edge, SourceSupport{}, true)
	})
}

// LinkDerived records an exact, versioned source receipt.
func (g *Graph) LinkDerived(edge Edge, support SourceSupport) error {
	return g.db.Update(func(tx *bolt.Tx) error { return g.LinkDerivedTx(tx, edge, support) })
}

// LinkDerivedTx records a receipt in a caller-owned graph transaction.
func (g *Graph) LinkDerivedTx(tx *bolt.Tx, edge Edge, support SourceSupport) error {
	if tx == nil || tx.DB() != g.db {
		return errors.New("kg: link: transaction belongs to another database")
	}
	if !support.valid() || len(edge.Sources) > 0 {
		return errors.New("kg: link: exactly one versioned support is required")
	}
	return linkTx(tx, edge, support, false)
}

func linkTx(tx *bolt.Tx, edge Edge, support SourceSupport, legacy bool) error {
	if edge.From == "" || edge.To == "" {
		return errors.New("kg: link: From and To must not be empty")
	}
	if edge.CreatedAt.IsZero() {
		edge.CreatedAt = time.Now().UTC()
	}
	if edge.Weight == 0 {
		edge.Weight = 1
	}
	if err := ensureEndpoints(tx.Bucket(bucketNodes), edge); err != nil {
		return err
	}

	key := []byte(edgeKey(edge.From, edge.To, edge.Relation))
	raw := tx.Bucket(bucketEdges).Get(key)
	var old Edge
	if raw != nil {
		var err error
		old, err = decodeKeyedEdge(key, raw)
		if err != nil {
			return fmt.Errorf("kg: link: read edge: %w", err)
		}
		if old.From != edge.From || old.To != edge.To || old.Relation != edge.Relation {
			return errors.New("kg: link: edge key collision")
		}
		if old.EdgeProvenance != nil && old.ProvenanceVersion > edgeProvenanceVersion {
			return errors.New("kg: link: unsupported provenance version")
		}
	}
	p := edgeForWrite(old, raw != nil)
	switch {
	case legacy:
		p.SourcesComplete = false
		if p.Reason == "" {
			p.Reason = "legacy_no_sources"
		}
		if len(edge.Sources) > 0 && p.Reason == "legacy_no_sources" {
			p.Reason = "legacy_provenance"
		}
		for _, id := range edge.Sources {
			if id == "" || slices.Contains(p.LegacySources, id) {
				continue
			}
			if len(p.LegacySources) >= maxEdgeSources {
				p.SourcesTruncated = true
				continue
			}
			p.LegacySources = append(p.LegacySources, id)
		}
	case support.valid():
		if !slices.Contains(p.Supports, support) {
			if len(p.Supports) >= maxEdgeSources {
				p.SourcesTruncated = true
			} else {
				p.Supports = append(p.Supports, support)
			}
		}
	default:
		p.ManualSupport = true
	}
	edge.EdgeProvenance = p
	edge.Sources = projectedSources(p, old.Sources)
	if len(provenanceIDs(p)) > maxEdgeSources {
		p.SourcesTruncated = true
	}
	if p.SourcesTruncated {
		p.SourcesComplete = false
	}
	return saveEdge(tx, key, raw, old, &edge)
}

func ensureEndpoints(nodes *bolt.Bucket, edge Edge) error {
	for _, id := range [2]string{edge.From, edge.To} {
		if nodes.Get([]byte(id)) != nil {
			continue
		}
		node := Node{ID: id, Label: id, EntityType: "unknown", FirstSeen: edge.CreatedAt, LastSeen: edge.CreatedAt, Weight: 1}
		data, err := json.Marshal(node)
		if err != nil {
			return err
		}
		if err := nodes.Put([]byte(id), data); err != nil {
			return err
		}
	}
	return nil
}

func edgeForWrite(old Edge, exists bool) *EdgeProvenance {
	if p := old.EdgeProvenance; p != nil && p.ProvenanceVersion == edgeProvenanceVersion {
		cloned := *p
		cloned.Supports = slices.Clone(p.Supports)
		cloned.LegacySources = slices.Clone(p.LegacySources)
		known := provenanceIDs(&cloned)
		for _, id := range old.Sources {
			if id != "" && !slices.Contains(known, id) {
				cloned.LegacySources = append(cloned.LegacySources, id)
				known = append(known, id)
			}
		}
		return &cloned
	}
	p := &EdgeProvenance{ProvenanceVersion: edgeProvenanceVersion, SourcesComplete: !exists}
	if exists {
		p.Reason = old.PendingReason()
		p.LegacySources = slices.Clone(old.Sources)
		if old.EdgeProvenance != nil {
			p.SourcesTruncated = old.SourcesTruncated
		}
	}
	return p
}

func provenanceIDs(p *EdgeProvenance) []string {
	var ids []string
	for _, id := range p.LegacySources {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, source := range p.Supports {
		if source.FactID != "" && !slices.Contains(ids, source.FactID) {
			ids = append(ids, source.FactID)
		}
	}
	return ids
}

func projectedSources(p *EdgeProvenance, previous []string) []string {
	ids := provenanceIDs(p)
	var projection []string
	for _, id := range previous {
		if slices.Contains(ids, id) && !slices.Contains(projection, id) {
			projection = append(projection, id)
		}
	}
	for _, id := range ids {
		if !slices.Contains(projection, id) {
			projection = append(projection, id)
		}
	}
	return projection[:min(maxEdgeSources, len(projection))]
}

func saveEdge(tx *bolt.Tx, key, raw []byte, before Edge, after *Edge) error {
	index := tx.Bucket(bucketSupportEdges)
	edges := tx.Bucket(bucketEdges)
	var remaining []SourceSupport
	if after != nil && after.EdgeProvenance != nil {
		remaining = after.Supports
	}
	if index != nil && before.EdgeProvenance != nil {
		for _, source := range before.Supports {
			if !slices.Contains(remaining, source) {
				if err := dropSupportIndex(index, source, key); err != nil {
					return err
				}
			}
		}
	}
	if len(remaining) > 0 && index == nil {
		var err error
		index, err = tx.CreateBucketIfNotExists(bucketSupportEdges)
		if err != nil {
			return err
		}
	}
	for _, source := range remaining {
		if !source.valid() {
			continue
		}
		b, err := index.CreateBucketIfNotExists(source.indexKey())
		if err != nil {
			return err
		}
		if err := b.Put(key, []byte{1}); err != nil {
			return err
		}
	}
	if after == nil {
		return edges.Delete(key)
	}
	data, err := edgeBytes(raw, *after)
	if err != nil {
		return err
	}
	return edges.Put(key, data)
}

func dropSupportIndex(index *bolt.Bucket, source SourceSupport, edge []byte) error {
	key := source.indexKey()
	b := index.Bucket(key)
	if b == nil {
		return nil
	}
	if err := b.Delete(edge); err != nil {
		return err
	}
	if first, _ := b.Cursor().First(); first == nil {
		return index.DeleteBucket(key)
	}
	return nil
}

func edgeBytes(raw []byte, edge Edge) ([]byte, error) {
	data, err := json.Marshal(edge)
	if err != nil || raw == nil {
		return data, err
	}
	// A provenance rewrite does not erase fields owned by other writers.
	var old, next map[string]json.RawMessage
	if err := json.Unmarshal(raw, &old); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &next); err != nil {
		return nil, err
	}
	for key := range old {
		switch strings.ToLower(key) {
		case "from", "to", "relation", "created_at", "weight", "sources",
			"provenance_version", "manual_support", "sources_complete", "sources_truncated",
			"supports", "legacy_sources", "pending_reason":
			delete(old, key)
		}
	}
	for key, value := range next {
		old[key] = value
	}
	return json.Marshal(old)
}

// RemoveSupport consumes one exact receipt. The lookup and provenance check
// happen inside the same transaction, including when an older writer changed
// the edge after the removal was scheduled.
func (g *Graph) RemoveSupport(support SourceSupport) error {
	return g.db.Update(func(tx *bolt.Tx) error { return g.RemoveSupportTx(tx, support) })
}

// RemoveSupportTx consumes one exact receipt in a caller-owned graph transaction.
func (g *Graph) RemoveSupportTx(tx *bolt.Tx, support SourceSupport) error {
	if tx == nil || tx.DB() != g.db {
		return errors.New("kg: remove support: transaction belongs to another database")
	}
	if !support.valid() {
		return errors.New("kg: remove support: exact support required")
	}
	index := tx.Bucket(bucketSupportEdges)
	if index == nil {
		return nil // A legacy graph has no exact receipts to consume.
	}
	edges := tx.Bucket(bucketEdges)
	b := index.Bucket(support.indexKey())
	if b == nil {
		return nil
	}
	var keys [][]byte
	if err := b.ForEach(func(k, _ []byte) error {
		keys = append(keys, slices.Clone(k))
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		raw := edges.Get(key)
		current, err := decodeKeyedEdge(key, raw)
		if err != nil && (raw != nil || edges.Bucket(key) != nil) {
			// The index identifies the key, but an unreadable payload cannot
			// confirm which edge or support occupies it.
			continue
		}
		if raw == nil || current.EdgeProvenance == nil || current.ProvenanceVersion != edgeProvenanceVersion ||
			!slices.Contains(current.Supports, support) {
			if err := dropSupportIndex(index, support, key); err != nil {
				return err
			}
			continue
		}
		if err := removeEdgeSupports(tx, key, raw, current, func(candidate SourceSupport) bool {
			return candidate == support
		}); err != nil {
			return err
		}
	}
	return nil
}

func removeEdgeSupports(tx *bolt.Tx, key, raw []byte, current Edge, remove func(SourceSupport) bool) error {
	next := current
	next.EdgeProvenance = edgeForWrite(current, true)
	next.Supports = slices.DeleteFunc(next.Supports, remove)
	next.Sources = projectedSources(next.EdgeProvenance, current.Sources)
	if next.PendingReason() == "" && !next.ManualSupport && len(next.Supports) == 0 {
		return saveEdge(tx, key, raw, current, nil)
	}
	return saveEdge(tx, key, raw, current, &next)
}
