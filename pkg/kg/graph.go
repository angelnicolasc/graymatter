// Package kg provides a lightweight entity-relationship knowledge graph for
// GrayMatter, backed by bbolt. Entities (Nodes) and their relationships (Edges)
// are persisted alongside the memory store and decay over time via an
// exponential weight curve. The graph is populated automatically by
// EntityExtractor during Consolidate and enriches Recall results.
package kg

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketNodes = []byte("kg_nodes")
	bucketEdges = []byte("kg_edges")
)

// Node represents a named entity in the knowledge graph.
type Node struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`      // e.g. "Maria", "sales-closer"
	EntityType string    `json:"entity_type"` // person/project/decision/preference/fact
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Weight     float64   `json:"weight"` // increases on access, decays over time
}

// Edge represents a directed relationship between two nodes.
type Edge struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Relation  string    `json:"relation"` // mentioned_in / related_to / contradicts
	CreatedAt time.Time `json:"created_at"`
	Weight    float64   `json:"weight"`
}

// Graph is a bbolt-backed, in-process knowledge graph.
// It reuses the existing gray.db handle via Store.DB().
type Graph struct {
	db *bolt.DB
}

// Open initialises the kg_nodes and kg_edges buckets in db and returns a Graph.
func Open(db *bolt.DB) (*Graph, error) {
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketNodes, bucketEdges} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("kg: init buckets: %w", err)
	}
	return &Graph{db: db}, nil
}

// Upsert inserts or updates a node. If a node with the same ID already exists,
// its Label, EntityType, and LastSeen are updated; Weight is max(existing, new).
func (g *Graph) Upsert(node Node) error {
	if node.ID == "" {
		return fmt.Errorf("kg: upsert: node ID must not be empty")
	}
	now := time.Now().UTC()
	if node.FirstSeen.IsZero() {
		node.FirstSeen = now
	}
	if node.LastSeen.IsZero() {
		node.LastSeen = now
	}
	if node.Weight == 0 {
		node.Weight = 1.0
	}

	return g.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		existing := b.Get([]byte(node.ID))
		if existing != nil {
			var old Node
			if err := json.Unmarshal(existing, &old); err == nil {
				// Preserve FirstSeen; take the higher weight.
				node.FirstSeen = old.FirstSeen
				if old.Weight > node.Weight {
					node.Weight = old.Weight
				}
			}
		}
		data, err := json.Marshal(node)
		if err != nil {
			return err
		}
		return b.Put([]byte(node.ID), data)
	})
}

// Link inserts or updates an edge between two nodes.
// Edges are keyed by "from|to|relation" so duplicate links upsert, not append.
func (g *Graph) Link(edge Edge) error {
	if edge.From == "" || edge.To == "" {
		return fmt.Errorf("kg: link: From and To must not be empty")
	}
	if edge.CreatedAt.IsZero() {
		edge.CreatedAt = time.Now().UTC()
	}
	if edge.Weight == 0 {
		edge.Weight = 1.0
	}
	key := edgeKey(edge.From, edge.To, edge.Relation)
	return g.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEdges)
		data, err := json.Marshal(edge)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
}

// Neighbors returns all nodes and edges reachable from nodeID within depth hops.
func (g *Graph) Neighbors(nodeID string, depth int) ([]Node, []Edge, error) {
	if depth <= 0 {
		return nil, nil, nil
	}

	allEdges, err := g.allEdges()
	if err != nil {
		return nil, nil, err
	}

	// BFS.
	visited := map[string]bool{nodeID: true}
	frontier := []string{nodeID}
	var resultEdges []Edge

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, from := range frontier {
			for _, e := range allEdges {
				if e.From == from && !visited[e.To] {
					visited[e.To] = true
					next = append(next, e.To)
					resultEdges = append(resultEdges, e)
				}
			}
		}
		frontier = next
	}

	// Load all visited nodes (excluding the seed node itself).
	var resultNodes []Node
	err = g.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		if b == nil {
			return nil
		}
		for id := range visited {
			if id == nodeID {
				continue
			}
			data := b.Get([]byte(id))
			if data == nil {
				continue
			}
			var n Node
			if err := json.Unmarshal(data, &n); err == nil {
				resultNodes = append(resultNodes, n)
			}
		}
		return nil
	})
	return resultNodes, resultEdges, err
}

// Decay applies exponential decay to all node and edge weights based on LastSeen.
// Nodes and edges with weight below the pruneThreshold (0.01) are deleted.
func (g *Graph) Decay(halfLife time.Duration) error {
	if halfLife == 0 {
		halfLife = 720 * time.Hour
	}
	lambda := math.Log(2) / halfLife.Hours()

	return g.db.Update(func(tx *bolt.Tx) error {
		// Decay nodes.
		nb := tx.Bucket(bucketNodes)
		if nb != nil {
			var toDelete [][]byte
			if err := nb.ForEach(func(k, v []byte) error {
				var n Node
				if err := json.Unmarshal(v, &n); err != nil {
					return nil
				}
				hours := time.Since(n.LastSeen).Hours()
				n.Weight *= math.Exp(-lambda * hours)
				if n.Weight < 0.01 {
					toDelete = append(toDelete, k)
					return nil
				}
				data, err := json.Marshal(n)
				if err != nil {
					return err
				}
				return nb.Put(k, data)
			}); err != nil {
				return err
			}
			for _, k := range toDelete {
				_ = nb.Delete(k)
			}
		}

		// Decay edges.
		eb := tx.Bucket(bucketEdges)
		if eb != nil {
			var toDelete [][]byte
			if err := eb.ForEach(func(k, v []byte) error {
				var e Edge
				if err := json.Unmarshal(v, &e); err != nil {
					return nil
				}
				hours := time.Since(e.CreatedAt).Hours()
				e.Weight *= math.Exp(-lambda * hours)
				if e.Weight < 0.01 {
					toDelete = append(toDelete, k)
					return nil
				}
				data, err := json.Marshal(e)
				if err != nil {
					return err
				}
				return eb.Put(k, data)
			}); err != nil {
				return err
			}
			for _, k := range toDelete {
				_ = eb.Delete(k)
			}
		}
		return nil
	})
}

// ExportObsidian writes one Markdown file per node + a graph-canvas.json file
// (Obsidian canvas format) to outDir.
func (g *Graph) ExportObsidian(outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("kg: mkdir %q: %w", outDir, err)
	}

	nodes, err := g.allNodes()
	if err != nil {
		return err
	}
	edges, err := g.allEdges()
	if err != nil {
		return err
	}

	// Write one .md file per node.
	for _, n := range nodes {
		content := fmt.Sprintf("---\nid: %s\nentity_type: %s\nfirst_seen: %s\nlast_seen: %s\nweight: %.4f\n---\n\n# %s\n\n**Type:** %s\n",
			n.ID, n.EntityType, n.FirstSeen.Format(time.RFC3339), n.LastSeen.Format(time.RFC3339), n.Weight, n.Label, n.EntityType)

		// Add related nodes as backlinks.
		var related []string
		for _, e := range edges {
			if e.From == n.ID {
				related = append(related, fmt.Sprintf("- [[%s]] (%s)", e.To, e.Relation))
			}
		}
		if len(related) > 0 {
			content += "\n## Related\n" + strings.Join(related, "\n") + "\n"
		}

		fname := sanitizeFilename(n.Label) + ".md"
		if err := os.WriteFile(filepath.Join(outDir, fname), []byte(content), 0o644); err != nil {
			return fmt.Errorf("kg: write node file: %w", err)
		}
	}

	// Write Obsidian canvas JSON.
	type canvasNode struct {
		ID     string  `json:"id"`
		Type   string  `json:"type"`
		Text   string  `json:"text"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
	}
	type canvasEdge struct {
		ID       string `json:"id"`
		FromNode string `json:"fromNode"`
		ToNode   string `json:"toNode"`
		Label    string `json:"label,omitempty"`
	}
	type canvas struct {
		Nodes []canvasNode `json:"nodes"`
		Edges []canvasEdge `json:"edges"`
	}

	c := canvas{}
	for i, n := range nodes {
		angle := float64(i) / float64(len(nodes)+1) * 2 * math.Pi
		c.Nodes = append(c.Nodes, canvasNode{
			ID:     n.ID,
			Type:   "text",
			Text:   n.Label,
			X:      500 * math.Cos(angle),
			Y:      500 * math.Sin(angle),
			Width:  200,
			Height: 60,
		})
	}
	for i, e := range edges {
		c.Edges = append(c.Edges, canvasEdge{
			ID:       fmt.Sprintf("edge-%d", i),
			FromNode: e.From,
			ToNode:   e.To,
			Label:    e.Relation,
		})
	}

	canvasData, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("kg: marshal canvas: %w", err)
	}
	return os.WriteFile(filepath.Join(outDir, "graph-canvas.json"), canvasData, 0o644)
}

// AllNodes returns all nodes sorted by weight descending (public for tests/TUI).
func (g *Graph) AllNodes() ([]Node, error) {
	nodes, err := g.allNodes()
	if err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Weight > nodes[j].Weight })
	return nodes, nil
}

// AllEdges returns all edges sorted by weight descending (public for tests/TUI).
func (g *Graph) AllEdges() ([]Edge, error) {
	edges, err := g.allEdges()
	if err != nil {
		return nil, err
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].Weight > edges[j].Weight })
	return edges, nil
}

// --- internal helpers ---

func (g *Graph) allNodes() ([]Node, error) {
	var nodes []Node
	err := g.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var n Node
			if err := json.Unmarshal(v, &n); err == nil {
				nodes = append(nodes, n)
			}
			return nil
		})
	})
	return nodes, err
}

func (g *Graph) allEdges() ([]Edge, error) {
	var edges []Edge
	err := g.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEdges)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e Edge
			if err := json.Unmarshal(v, &e); err == nil {
				edges = append(edges, e)
			}
			return nil
		})
	})
	return edges, err
}

func edgeKey(from, to, relation string) string {
	return from + "|" + to + "|" + relation
}

func sanitizeFilename(s string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "-", "\"", "-", "<", "-", ">", "-", "|", "-", " ", "_")
	return r.Replace(s)
}
