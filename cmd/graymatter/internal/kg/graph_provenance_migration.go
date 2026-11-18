package kg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

const provenanceBatchSize = 128

var provenanceMigrationKey = []byte("provenance_migration")

type provenanceMigration struct {
	Version  int    `json:"version"`
	After    []byte `json:"after,omitempty"`
	Examined int    `json:"examined"`
	Complete bool   `json:"complete"`
}

// ProvenanceReport is an inventory of current graph rows. Migration completion
// only describes the scan. Incomplete edges remain visible and navigable.
type ProvenanceReport struct {
	Examined          int            `json:"examined"`
	Complete          int            `json:"complete"`
	Incomplete        int            `json:"incomplete"`
	Pending           int            `json:"pending"`
	CleanupPending    int            `json:"cleanup_pending"`
	Reasons           map[string]int `json:"reasons"`
	MigrationVersion  int            `json:"migration_version"`
	MigrationExamined int            `json:"migration_examined"`
	MigrationComplete bool           `json:"migration_complete"`
}

func migrationState(meta *bolt.Bucket) (provenanceMigration, error) {
	var state provenanceMigration
	if meta != nil {
		if raw := meta.Get(provenanceMigrationKey); raw != nil {
			if err := json.Unmarshal(raw, &state); err != nil {
				return state, fmt.Errorf("kg: provenance migration checkpoint: %w", err)
			}
		}
	}
	return state, nil
}

// MigrateProvenance completes the initial inventory in bounded transactions.
// It visits edges independently of extraction watermarks and persists each
// cursor with the classified rows. A failure resumes at the last commit.
func (g *Graph) MigrateProvenance() error {
	for {
		var done bool
		err := g.db.View(func(tx *bolt.Tx) error {
			state, err := migrationState(tx.Bucket(bucketKGMeta))
			done = state.Version > edgeProvenanceVersion || (state.Version == edgeProvenanceVersion && state.Complete)
			return err
		})
		if err != nil || done {
			return err
		}
		if err := g.db.Update(func(tx *bolt.Tx) error {
			return migrateProvenanceTx(tx, provenanceBatchSize)
		}); err != nil {
			return err
		}
	}
}

func migrateProvenanceTx(tx *bolt.Tx, limit int) error {
	meta, err := tx.CreateBucketIfNotExists(bucketKGMeta)
	if err != nil {
		return err
	}
	state, err := migrationState(meta)
	if err != nil {
		return err
	}
	if state.Version > edgeProvenanceVersion || (state.Version == edgeProvenanceVersion && state.Complete) {
		return nil
	}
	if state.Version != edgeProvenanceVersion {
		state = provenanceMigration{Version: edgeProvenanceVersion}
	}
	edges := tx.Bucket(bucketEdges)
	cursor := edges.Cursor()
	key, _ := cursor.First()
	if len(state.After) > 0 {
		key, _ = cursor.Seek(state.After)
		if bytes.Equal(key, state.After) {
			key, _ = cursor.Next()
		}
	}
	var keys [][]byte
	for key != nil && len(keys) < limit {
		keys = append(keys, slices.Clone(key))
		key, _ = cursor.Next()
	}
	for _, key := range keys {
		if err := migrateEdgeTx(tx, key, edges.Get(key)); err != nil {
			return err
		}
		state.After = key
		state.Examined++
	}
	state.Complete = key == nil
	if state.Complete {
		state.After = nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return meta.Put(provenanceMigrationKey, data)
}

func migrateEdgeTx(tx *bolt.Tx, key, raw []byte) error {
	edge, err := decodeKeyedEdge(key, raw)
	if err != nil {
		return nil // Inventory reports the unreadable row without rewriting it.
	}
	if edge.EdgeProvenance != nil && edge.ProvenanceVersion != edgeProvenanceVersion {
		return nil // A newer writer's evidence is outside this migration.
	}

	next := edge
	next.EdgeProvenance = edgeForWrite(edge, true)
	if len(provenanceIDs(next.EdgeProvenance)) > maxEdgeSources {
		next.SourcesTruncated = true
		next.SourcesComplete = false
	}
	if reason := legacySourceReason(tx, next.LegacySources); reason != "" && next.Reason != "invalid_provenance" {
		next.Reason = reason
	}

	var aliases []SourceSupport
	for _, source := range next.Supports {
		kind, err := memory.ClassifyGraphSupportTx(tx, memory.GraphSupport(source))
		if err == nil && kind == "alias" {
			aliases = append(aliases, source)
		}
	}
	if len(aliases) > 0 {
		next.Supports = slices.DeleteFunc(next.Supports, func(source SourceSupport) bool {
			return slices.Contains(aliases, source)
		})
		if next.PendingReason() == "" && !next.ManualSupport && len(next.Supports) == 0 {
			return saveEdge(tx, key, raw, edge, nil)
		}
	}
	next.Sources = projectedSources(next.EdgeProvenance, edge.Sources)
	return saveEdge(tx, key, raw, edge, &next)
}

func legacySourceReason(tx *bolt.Tx, ids []string) string {
	var missing, ambiguous, alias bool
	for _, id := range ids {
		status, err := memory.InspectGraphFactTx(tx, id)
		if err != nil {
			return "unreadable_source"
		}
		switch status {
		case "unreadable":
			return "unreadable_source"
		case "missing":
			missing = true
		case "ambiguous":
			ambiguous = true
		case "alias":
			alias = true
		}
	}
	switch {
	case missing:
		return "missing_source"
	case ambiguous:
		return "ambiguous_source"
	case alias:
		return "legacy_alias_reference"
	default:
		return ""
	}
}

func currentProvenanceReason(tx *bolt.Tx, edge Edge) string {
	reason := edge.PendingReason()
	if reason == "" {
		var missing, stale, alias bool
		for _, source := range edge.Supports {
			status, err := memory.ClassifyGraphSupportTx(tx, memory.GraphSupport(source))
			if err != nil {
				return "unreadable_source"
			}
			switch status {
			case "missing":
				missing = true
			case "stale":
				stale = true
			case "alias":
				alias = true
			}
		}
		switch {
		case missing:
			return "missing_source"
		case stale:
			return "stale_source_revision"
		case alias:
			return "alias_support_pending"
		default:
			return ""
		}
	}
	switch reason {
	case "legacy_provenance", "legacy_alias_reference", "missing_source", "ambiguous_source", "unreadable_source":
		ids := edge.Sources
		if edge.EdgeProvenance != nil {
			ids = edge.LegacySources
		}
		if current := legacySourceReason(tx, ids); current != "" {
			return current
		}
		return "legacy_provenance"
	default:
		return reason
	}
}

// ProvenanceReport reads current counts and the migration checkpoint together.
func (g *Graph) ProvenanceReport() (ProvenanceReport, error) {
	var report ProvenanceReport
	err := g.db.View(func(tx *bolt.Tx) error {
		var err error
		report, err = inspectEdgesTx(tx, nil)
		return err
	})
	return report, err
}

// EdgeInspection pairs the unchanged edge snapshot with its current status.
type EdgeInspection struct {
	Edge          Edge   `json:"edge"`
	PendingReason string `json:"pending_reason,omitempty"`
}

// Inspect reads nodes, edges, and provenance counts from a single snapshot.
func (g *Graph) Inspect() (nodes []Node, edges []EdgeInspection, report ProvenanceReport, err error) {
	err = g.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketNodes).ForEach(func(key, raw []byte) error {
			var node Node
			if err := json.Unmarshal(raw, &node); err != nil {
				return fmt.Errorf("kg: inspect node %q: %w", key, err)
			}
			if node.ID == "" || node.ID != string(key) {
				return fmt.Errorf("kg: inspect node %q: record identity mismatch", key)
			}
			nodes = append(nodes, node)
			return nil
		}); err != nil {
			return err
		}
		var err error
		report, err = inspectEdgesTx(tx, &edges)
		return err
	})
	return
}

func inspectEdgesTx(tx *bolt.Tx, result *[]EdgeInspection) (ProvenanceReport, error) {
	state, err := migrationState(tx.Bucket(bucketKGMeta))
	if err != nil {
		return ProvenanceReport{}, err
	}
	report := ProvenanceReport{
		Reasons: make(map[string]int), MigrationVersion: state.Version,
		CleanupPending:    memory.GraphCleanupPendingTx(tx),
		MigrationExamined: state.Examined,
		MigrationComplete: state.Version == edgeProvenanceVersion && state.Complete,
	}
	err = tx.Bucket(bucketEdges).ForEach(func(key, raw []byte) error {
		report.Examined++
		reason := "unreadable_edge"
		edge, err := decodeKeyedEdge(key, raw)
		if err == nil {
			reason = currentProvenanceReason(tx, edge)
			if result != nil {
				*result = append(*result, EdgeInspection{Edge: edge, PendingReason: reason})
			}
		} else if result != nil {
			return fmt.Errorf("kg: inspect edge %q: %w", key, err)
		}
		if reason == "" {
			report.Complete++
		} else {
			report.Incomplete++
			report.Pending++
			report.Reasons[reason]++
		}
		return nil
	})
	return report, err
}

// SourceEvidence is an operator's explicit attestation of current fact content.
// The graph registers the exact source revision only after validating all fields
// against the canonical fact in the same write transaction.
type SourceEvidence struct {
	Namespace  string `json:"namespace"`
	FactID     string `json:"fact_id"`
	Kind       string `json:"kind"`
	TextSHA256 string `json:"text_sha256"`
}

// ReconcileProvenance resolves one inspected edge using explicit evidence of
// its full support. Known manual or exact support cannot be discarded. An
// unverified or truncated receipt remains pending.
func (g *Graph) ReconcileProvenance(expected Edge, manual bool, evidence []SourceEvidence) error {
	if expected.From == "" || expected.To == "" || len(evidence) > maxEdgeSources || (!manual && len(evidence) == 0) {
		return errors.New("kg: reconcile: edge and complete support evidence required")
	}
	return g.db.Update(func(tx *bolt.Tx) error {
		key := []byte(edgeKey(expected.From, expected.To, expected.Relation))
		raw := tx.Bucket(bucketEdges).Get(key)
		if raw == nil {
			return errors.New("kg: reconcile: inspected edge no longer exists")
		}
		current, err := decodeKeyedEdge(key, raw)
		if err != nil {
			return err
		}
		if err := requireSameEdge(expected, current); err != nil {
			return err
		}
		return reconcileEdgeTx(tx, key, raw, current, manual, evidence)
	})
}

func requireSameEdge(expected, current Edge) error {
	a, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	b, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return errors.New("kg: reconcile: edge changed since inspection")
	}
	return nil
}

func reconcileEdgeTx(tx *bolt.Tx, key, raw []byte, current Edge, manual bool, evidence []SourceEvidence) error {
	old := edgeForWrite(current, true)
	if old.SourcesTruncated || len(old.LegacySources) > maxEdgeSources {
		return errors.New("kg: reconcile: truncated evidence cannot establish complete support")
	}
	if current.EdgeProvenance != nil && current.ProvenanceVersion != edgeProvenanceVersion {
		return errors.New("kg: reconcile: unsupported provenance version")
	}
	if old.ManualSupport && !manual {
		return errors.New("kg: reconcile: known manual support must be retained")
	}
	p := &EdgeProvenance{ProvenanceVersion: edgeProvenanceVersion, ManualSupport: manual, SourcesComplete: true}
	verified := make(map[SourceSupport]bool, len(evidence))
	covered := make(map[string]bool, len(evidence))
	for _, item := range evidence {
		source, err := memory.RegisterAttestedGraphSupportTx(tx, item.Namespace, item.FactID, item.Kind, item.TextSHA256)
		if err != nil {
			return fmt.Errorf("kg: reconcile: source %q: %w", item.FactID, err)
		}
		exact := SourceSupport(source)
		if verified[exact] {
			return errors.New("kg: reconcile: duplicate exact source")
		}
		verified[exact] = true
		covered[item.FactID] = true
		kind, err := memory.ClassifyGraphSupportTx(tx, source)
		if err != nil {
			return err
		}
		switch kind {
		case "fact":
			p.Supports = append(p.Supports, exact)
		case "alias":
			// The attested alias cannot supply semantic graph support.
		default:
			return fmt.Errorf("kg: reconcile: source %q is %s", item.FactID, kind)
		}
	}
	for _, id := range old.LegacySources {
		status, err := memory.InspectGraphFactTx(tx, id)
		if err != nil {
			return err
		}
		if !covered[id] || status == "ambiguous" || status == "unreadable" {
			return fmt.Errorf("kg: reconcile: legacy source %q remains unverified", id)
		}
	}
	for _, source := range old.Supports {
		if !verified[source] {
			return fmt.Errorf("kg: reconcile: exact source %q must be retained", source.FactID)
		}
	}
	if !p.ManualSupport && len(p.Supports) == 0 {
		return saveEdge(tx, key, raw, current, nil)
	}
	next := current
	next.EdgeProvenance = p
	next.Sources = projectedSources(p, current.Sources)
	return saveEdge(tx, key, raw, current, &next)
}
