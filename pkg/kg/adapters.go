package kg

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

// LinkNodes creates an edge between two nodes. Implements mcp.KGLinker.
func (a *GraphAdapter) LinkNodes(from, to, relation string) error {
	return a.g.Link(Edge{From: from, To: to, Relation: relation})
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
