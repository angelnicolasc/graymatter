package kg

import (
	"fmt"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
)

var metaKeyGraphIdentity = []byte("graph_identity")

// GraphAdapter wraps *Graph to satisfy the memory.GraphAccessor interface.
// This keeps pkg/memory free of a direct dependency on pkg/kg.
type GraphAdapter struct {
	g *Graph
}

// NewGraphAdapter returns a GraphAdapter for use as a memory.GraphAccessor.
func NewGraphAdapter(g *Graph) *GraphAdapter { return &GraphAdapter{g: g} }

// UpsertNode implements memory.GraphAccessor.
func (a *GraphAdapter) UpsertNode(id, label, entityType string) error {
	return a.g.Upsert(Node{ID: id, Label: label, EntityType: entityType})
}

// LinkNodes records explicit manual support for a relationship. Implements mcp.KGLinker.
func (a *GraphAdapter) LinkNodes(from, to, relation string) error {
	return a.g.Link(Edge{From: from, To: to, Relation: relation})
}

// LinkEdges implements memory.EdgeWriter for callers with only a fact ID.
// Those receipts cannot establish completeness or prove manual support.
func (a *GraphAdapter) LinkEdges(links []memory.EntityLink, sourceFactID string) error {
	for _, link := range links {
		edge := Edge{From: link.From, To: link.To, Relation: link.Relation}
		if sourceFactID != "" {
			edge.Sources = []string{sourceFactID}
		}
		if err := a.g.LinkLegacy(edge); err != nil {
			return err
		}
	}
	return nil
}

// OwnsGraphTx reports whether the fact and graph writes can share tx.
func (a *GraphAdapter) OwnsGraphTx(tx *bolt.Tx) bool {
	return tx != nil && tx.DB() == a.g.db
}

// GraphIdentity identifies this durable graph as an outbox delivery target.
func (a *GraphAdapter) GraphIdentity() (string, error) {
	var identity string
	if err := a.g.db.View(func(tx *bolt.Tx) error {
		if meta := tx.Bucket(bucketKGMeta); meta != nil {
			identity = string(meta.Get(metaKeyGraphIdentity))
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("kg: read graph identity: %w", err)
	}
	if identity != "" {
		return identity, nil
	}
	if err := a.g.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(bucketKGMeta)
		if err != nil {
			return err
		}
		// Another writer may have allocated the identity since the read.
		identity = string(meta.Get(metaKeyGraphIdentity))
		if identity != "" {
			return nil
		}
		identity = ulid.Make().String()
		return meta.Put(metaKeyGraphIdentity, []byte(identity))
	}); err != nil {
		return "", fmt.Errorf("kg: persist graph identity: %w", err)
	}
	return identity, nil
}

// WriteGraphExtraction commits all nodes and support from one fact snapshot.
func (a *GraphAdapter) WriteGraphExtraction(refs []memory.EntityRef, links []memory.EntityLink, source memory.GraphSupport) error {
	return a.g.db.Update(func(tx *bolt.Tx) error {
		return a.WriteGraphExtractionTx(tx, refs, links, source)
	})
}

// WriteGraphExtractionTx lets the store validate and record an extraction
// alongside the graph writes in one transaction.
func (a *GraphAdapter) WriteGraphExtractionTx(tx *bolt.Tx, refs []memory.EntityRef, links []memory.EntityLink, source memory.GraphSupport) error {
	if !a.OwnsGraphTx(tx) || !tx.Writable() {
		return fmt.Errorf("kg: extraction requires a writable graph transaction")
	}
	for _, ref := range refs {
		if err := a.g.UpsertTx(tx, Node{ID: ref.ID, Label: ref.Label, EntityType: ref.EntityType}); err != nil {
			return fmt.Errorf("upsert node %s: %w", ref.ID, err)
		}
	}
	support := SourceSupport{Namespace: source.Namespace, FactID: source.FactID, Digest: source.Digest}
	for _, link := range links {
		if err := a.g.LinkDerivedTx(tx, Edge{From: link.From, To: link.To, Relation: link.Relation}, support); err != nil {
			return fmt.Errorf("link %s to %s: %w", link.From, link.To, err)
		}
	}
	return nil
}

// RemoveGraphSupport removes the exact version of a fact's graph support.
func (a *GraphAdapter) RemoveGraphSupport(source memory.GraphSupport) error {
	return a.g.RemoveSupport(SourceSupport{Namespace: source.Namespace, FactID: source.FactID, Digest: source.Digest})
}

// RemoveGraphSupportTx applies support cleanup in the caller's graph transaction.
func (a *GraphAdapter) RemoveGraphSupportTx(tx *bolt.Tx, source memory.GraphSupport) error {
	if !a.OwnsGraphTx(tx) || !tx.Writable() {
		return fmt.Errorf("kg: cleanup requires a writable graph transaction")
	}
	return a.g.RemoveSupportTx(tx, SourceSupport{Namespace: source.Namespace, FactID: source.FactID, Digest: source.Digest})
}

// MigrateGraphProvenance advances the resumable provenance inventory before
// consolidation prunes the canonical facts it can still inspect.
func (a *GraphAdapter) MigrateGraphProvenance() error {
	return a.g.MigrateProvenance()
}

// NeighborTexts implements memory.GraphAccessor.
// It returns the Label of each node reachable from nodeID within depth hops.
func (a *GraphAdapter) NeighborTexts(nodeID string, depth int) ([]string, error) {
	nodes, _, err := a.g.Neighbors(nodeID, depth)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Label)
	}
	return out, nil
}

// DecayGraph implements memory.GraphDecayer: consolidation calls it once per
// cycle so graph weights follow the same lifecycle as fact weights.
func (a *GraphAdapter) DecayGraph(halfLife time.Duration) error {
	return a.g.DecayGraph(halfLife)
}

// ExtractorAdapter wraps EntityExtractor to satisfy memory.EntityExtractorAccessor.
type ExtractorAdapter struct {
	e EntityExtractor
}

// NewExtractorAdapter returns an ExtractorAdapter.
func NewExtractorAdapter(e EntityExtractor) *ExtractorAdapter {
	return &ExtractorAdapter{e: e}
}

// ExtractIDs implements memory.EntityExtractorAccessor.
// It returns canonical node IDs for all entities found in text.
func (a *ExtractorAdapter) ExtractIDs(text string) ([]string, error) {
	nodes, _, err := a.e.Extract(text)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.ID != "" {
			ids = append(ids, n.ID)
		}
	}
	return ids, nil
}

// ExtractTyped implements memory.TypedEntityExtractor: it preserves the
// label and entity type the extractor produced instead of collapsing
// everything to a bare ID.
func (a *ExtractorAdapter) ExtractTyped(text string) ([]memory.EntityRef, []memory.EntityLink, error) {
	nodes, edges, err := a.e.Extract(text)
	if err != nil {
		return nil, nil, err
	}
	refs := make([]memory.EntityRef, 0, len(nodes))
	for _, n := range nodes {
		if n.ID == "" {
			continue
		}
		refs = append(refs, memory.EntityRef{ID: n.ID, Label: n.Label, EntityType: n.EntityType})
	}
	links := make([]memory.EntityLink, 0, len(edges))
	for _, e := range edges {
		links = append(links, memory.EntityLink{From: e.From, To: e.To, Relation: e.Relation})
	}
	return refs, links, nil
}

var (
	_ memory.GraphDecayer          = (*GraphAdapter)(nil)
	_ memory.GraphProvenanceWriter = (*GraphAdapter)(nil)
)
