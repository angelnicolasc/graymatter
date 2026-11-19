// Package kg provides a lightweight entity-relationship knowledge graph for
// GrayMatter, backed by bbolt. Entities (Nodes) and their relationships (Edges)
// are persisted alongside the memory store and decay over time via an
// exponential weight curve. The graph is populated automatically by
// EntityExtractor during Consolidate and enriches Recall results.
package kg

import (
	"encoding/json"
	"errors"
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
	bucketNodes   = []byte("kg_nodes")
	bucketEdges   = []byte("kg_edges")
	bucketKGMeta  = []byte("kg_meta")
	metaKeyScheme = "id_scheme"

	// idSchemeVersion is the node-ID scheme this build writes. v1 IDs were
	// bare lowercased labels, which merged distinct entities ("apple" the
	// organization and "apple" the concept collapsed into one node). v2 IDs
	// are type-scoped ("<type>:<label>"). A scheme change schedules typed
	// re-extraction while preserving earlier graph rows and their evidence.
	idSchemeVersion = "2"
)

// Node represents a named entity in the knowledge graph.
type Node struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`       // e.g. "Maria", "sales-closer"
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
	// Sources is the capped, deduplicated fact-ID projection retained for
	// existing readers. It cannot establish that every support is known.
	Sources []string `json:"sources,omitempty"`
	*EdgeProvenance
}

// maxEdgeSources caps provenance receipts per edge.
const maxEdgeSources = 10

// Graph is a bbolt-backed, in-process knowledge graph.
// It reuses the existing gray.db handle via Store.DB().
type Graph struct {
	db *bolt.DB
}

// Open initializes the graph buckets and advances the provenance inventory.
// A node-ID scheme change schedules re-extraction with typed IDs. Existing
// rows remain available because a rebuild cannot reproduce all legacy support.
func Open(db *bolt.DB) (*Graph, error) {
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketNodes, bucketEdges, bucketKGMeta, bucketSupportEdges} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		mb := tx.Bucket(bucketKGMeta)
		if string(mb.Get([]byte(metaKeyScheme))) != idSchemeVersion {
			// The watermark is derived from facts. Clearing it schedules
			// typed extraction without claiming to repair legacy evidence.
			if wb := tx.Bucket([]byte("kg_extracted")); wb != nil {
				if err := tx.DeleteBucket([]byte("kg_extracted")); err != nil {
					return err
				}
				if _, err := tx.CreateBucket([]byte("kg_extracted")); err != nil {
					return err
				}
			}
			if err := mb.Put([]byte(metaKeyScheme), []byte(idSchemeVersion)); err != nil {
				return err
			}
		}
		return migrateProvenanceTx(tx, provenanceBatchSize)
	}); err != nil {
		return nil, fmt.Errorf("kg: init buckets: %w", err)
	}
	return &Graph{db: db}, nil
}

// ErrNoGraph reports that the database carries no graph buckets at all —
// distinct from "a graph with zero nodes", and from an IO failure.
var ErrNoGraph = errors.New("kg: no graph in this database")

// OpenRead returns a Graph over an existing one without initialising
// anything, so it works on read-only bbolt handles where Open's bucket
// creation would fail. Callers distinguish three states: nil error means a
// readable graph; ErrNoGraph means the store never had one; any other error
// is a real failure.
func OpenRead(db *bolt.DB) (*Graph, error) {
	err := db.View(func(tx *bolt.Tx) error {
		nodes, edges := tx.Bucket(bucketNodes), tx.Bucket(bucketEdges)
		if nodes == nil && edges == nil {
			return ErrNoGraph
		}
		if nodes == nil || edges == nil {
			return errors.New("kg: graph buckets are incomplete")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Graph{db: db}, nil
}

// Upsert inserts or updates a node. If a node with the same ID already exists,
// its Label, EntityType, and LastSeen are updated; Weight is max(existing, new).
func (g *Graph) Upsert(node Node) error {
	return g.db.Update(func(tx *bolt.Tx) error { return g.UpsertTx(tx, node) })
}

// UpsertTx applies Upsert in a caller-owned graph transaction.
func (g *Graph) UpsertTx(tx *bolt.Tx, node Node) error {
	if tx == nil || tx.DB() != g.db {
		return errors.New("kg: upsert: transaction belongs to another database")
	}
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
}

// Neighbors returns all nodes and edges reachable from nodeID within depth hops.
func (g *Graph) Neighbors(nodeID string, depth int) ([]Node, []Edge, error) {
	if depth <= 0 {
		return nil, nil, nil
	}
	var resultNodes []Node
	var resultEdges []Edge
	err := g.db.View(func(tx *bolt.Tx) error {
		allEdges, err := readEdgesTx(tx)
		if err != nil {
			return err
		}

		visited := map[string]bool{nodeID: true}
		frontier := []string{nodeID}
		for d := 0; d < depth && len(frontier) > 0; d++ {
			var next []string
			for _, from := range frontier {
				for _, edge := range allEdges {
					if edge.From == from && !visited[edge.To] {
						visited[edge.To] = true
						next = append(next, edge.To)
						resultEdges = append(resultEdges, edge)
					}
				}
			}
			frontier = next
		}

		b := tx.Bucket(bucketNodes)
		if b == nil {
			return nil
		}
		for id := range visited {
			if id == nodeID {
				continue
			}
			var node Node
			if err := json.Unmarshal(b.Get([]byte(id)), &node); err == nil {
				resultNodes = append(resultNodes, node)
			}
		}
		return nil
	})
	return resultNodes, resultEdges, err
}

// DecayGraph recomputes graph weights and prunes stale orphan nodes.
func (g *Graph) DecayGraph(halfLife time.Duration) error {
	return g.DecayGraphAt(time.Now(), halfLife)
}

// DecayGraphAt accepts a fixed clock. Edge support governs removal. A stale
// endpoint remains traversable until its final edge is removed.
func (g *Graph) DecayGraphAt(now time.Time, halfLife time.Duration) error {
	if halfLife <= 0 {
		halfLife = 720 * time.Hour
	}
	lambda := math.Log(2) / halfLife.Hours()
	return g.db.Update(func(tx *bolt.Tx) error {
		if err := migrateProvenanceTx(tx, provenanceBatchSize); err != nil {
			return err
		}

		incident := make(map[string]bool)
		unreadableEdge := false
		if err := decayBucket(tx.Bucket(bucketEdges), now, lambda, func(key, raw []byte) (time.Time, bool, bool) {
			edge, err := decodeKeyedEdge(key, raw)
			if err != nil {
				unreadableEdge = true
				return time.Time{}, false, false
			}
			incident[edge.From] = true
			incident[edge.To] = true
			return edge.CreatedAt, false, true
		}); err != nil {
			return err
		}
		return decayBucket(tx.Bucket(bucketNodes), now, lambda, func(key, raw []byte) (time.Time, bool, bool) {
			var node Node
			if err := json.Unmarshal(raw, &node); err != nil || node.ID == "" || node.ID != string(key) {
				return time.Time{}, false, false
			}
			return node.LastSeen, !unreadableEdge && !incident[node.ID], true
		})
	})
}

// decayBucket receives the freshness anchor, pruning eligibility, and whether
// the row is readable. Unreadable records stay available for inspection.
func decayBucket(b *bolt.Bucket, now time.Time, lambda float64, classify func([]byte, []byte) (time.Time, bool, bool)) error {
	if b == nil {
		return nil
	}
	type entry struct{ key, value []byte }
	var changed []entry
	var removed [][]byte
	if err := b.ForEach(func(k, v []byte) error {
		anchor, prunable, readable := classify(k, v)
		if !readable {
			return nil
		}
		var hours float64
		if !anchor.IsZero() {
			hours = now.Sub(anchor).Hours()
		}
		weight, err := weightOf(v)
		if err != nil {
			return nil
		}
		w := math.Min(weight, math.Exp(-lambda*hours))
		if w < 0.01 && prunable {
			removed = append(removed, append([]byte(nil), k...))
		} else if w != weight {
			nv, err := withWeight(v, w)
			if err != nil {
				return err
			}
			changed = append(changed, entry{append([]byte(nil), k...), nv})
		}
		return nil
	}); err != nil {
		return err
	}
	for _, entry := range changed {
		if err := b.Put(entry.key, entry.value); err != nil {
			return err
		}
	}
	for _, key := range removed {
		if err := b.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

// weightOf extracts the weight field without mutating the rest of the record.
func weightOf(val []byte) (float64, error) {
	m := map[string]any{}
	if err := json.Unmarshal(val, &m); err != nil {
		return 1, err
	}
	if w, ok := m["weight"].(float64); ok {
		return w, nil
	}
	return 1, nil
}

// withWeight rewrites only the weight field of a JSON record, preserving
// every other byte-level field exactly as stored.
func withWeight(val []byte, w float64) ([]byte, error) {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(val, &m); err != nil {
		return nil, err
	}
	weight, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	m["weight"] = weight
	return json.Marshal(m)
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

	// Wikilinks must name the note file (sanitized label), not the raw node
	// ID — an [[ID]] link matches no file and Obsidian drops it from the
	// graph view entirely.
	labelBy := make(map[string]string, len(nodes))
	for _, n := range nodes {
		labelBy[n.ID] = n.Label
	}

	// One naming authority for this export: distinct entities whose labels
	// sanitize to the same filename ("A B" vs "A_B") each keep their own
	// note instead of silently overwriting each other. Every writer below -
	// node files, Related links, the MOC - resolves through this one map.
	names := EntityNoteNames(nodes)

	// Write one .md file per node.
	for _, n := range nodes {
		content := fmt.Sprintf("---\nid: %s\nentity_type: %s\ntags:\n  - entity\n  - %s\nfirst_seen: %s\nlast_seen: %s\nweight: %.4f\n---\n\n# %s\n\n**Type:** %s\n",
			n.ID, n.EntityType, n.EntityType, n.FirstSeen.Format(time.RFC3339), n.LastSeen.Format(time.RFC3339), n.Weight, n.Label, n.EntityType)

		// Add related nodes as backlinks, with provenance receipt counts.
		var related []string
		for _, e := range edges {
			if e.From == n.ID {
				label := labelBy[e.To]
				target, ok := names[e.To]
				if !ok && label != "" {
					target = SanitizeFilename(label)
				}
				line := fmt.Sprintf("- [[%s|%s]] (%s)", target, label, e.Relation)
				if len(e.Sources) > 0 {
					line += fmt.Sprintf(" · %d receipt(s)", len(e.Sources))
				}
				related = append(related, line)
			}
		}
		if len(related) > 0 {
			content += "\n## Related\n" + strings.Join(related, "\n") + "\n"
		}

		fname := names[n.ID] + ".md"
		if err := os.WriteFile(filepath.Join(outDir, fname), []byte(content), 0o644); err != nil {
			return fmt.Errorf("kg: write node file: %w", err)
		}
	}

	// MOC (Map of Content) grouped by entity type, so the vault has a single
	// entry point into the graph.
	if err := writeEntitiesMOC(outDir, nodes, names); err != nil {
		return fmt.Errorf("kg: write entities index: %w", err)
	}

	// Ship the .obsidian/graph.json template so the vault opens with the
	// color-grouped, force-tuned view instead of the raw default.
	if err := WriteGraphConfig(outDir); err != nil {
		return fmt.Errorf("kg: write graph config: %w", err)
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
		var err error
		edges, err = readEdgesTx(tx)
		return err
	})
	return edges, err
}

func readEdgesTx(tx *bolt.Tx) ([]Edge, error) {
	var edges []Edge
	b := tx.Bucket(bucketEdges)
	if b == nil {
		return nil, nil
	}
	err := b.ForEach(func(key, raw []byte) error {
		if edge, err := decodeKeyedEdge(key, raw); err == nil {
			edges = append(edges, edge)
		}
		return nil
	})
	return edges, err
}

func edgeKey(from, to, relation string) string {
	return from + "|" + to + "|" + relation
}

func decodeKeyedEdge(key, raw []byte) (Edge, error) {
	var edge Edge
	if err := json.Unmarshal(raw, &edge); err != nil {
		return Edge{}, err
	}
	if edge.From == "" || edge.To == "" || edgeKey(edge.From, edge.To, edge.Relation) != string(key) {
		return Edge{}, errors.New("record identity mismatch")
	}
	return edge, nil
}

// writeEntitiesMOC writes _graphs/entities-index.md: one section per entity
// type linking every entity note, so the vault has a single graph entry point.
func writeEntitiesMOC(outDir string, nodes []Node, names map[string]string) error {
	mocDir := filepath.Join(outDir, "_graphs")
	if err := os.MkdirAll(mocDir, 0o755); err != nil {
		return err
	}
	byType := map[string][]Node{}
	types := []string{}
	for _, n := range nodes {
		t := n.EntityType
		if t == "" {
			t = "unknown"
		}
		if byType[t] == nil {
			byType[t] = nil
			types = append(types, t)
		}
		byType[t] = append(byType[t], n)
	}
	sort.Strings(types)

	var sb strings.Builder
	sb.WriteString("---\ntags: [graymatter, moc]\n---\n\n# Entities Index\n\n")
	for _, t := range types {
		fmt.Fprintf(&sb, "## %s (%d)\n\n", t, len(byType[t]))
		for _, n := range byType[t] {
			fmt.Fprintf(&sb, "- [[%s|%s]]\n", names[n.ID], n.Label)
		}
		sb.WriteString("\n")
	}
	return os.WriteFile(filepath.Join(mocDir, "entities-index.md"), []byte(sb.String()), 0o644)
}

// graphJSON is the .obsidian/graph.json template shipped with --include-graph
// so the vault opens with the color-grouped, force-tuned view instead of the
// raw default. Colors group entity notes by their type tag.
type graphColorGroup struct {
	Query string `json:"query"`
	Color struct {
		A   float64 `json:"a"`
		Rgb int     `json:"rgb"`
	} `json:"color"`
}

func graphJSONTemplate() string {
	group := func(query string, rgb int) graphColorGroup {
		g := graphColorGroup{Query: query}
		g.Color.A = 1
		g.Color.Rgb = rgb
		return g
	}
	cfg := map[string]any{
		"collapse-filter":       true,
		"search":                "",
		"showTags":              false,
		"showAttachments":       false,
		"hideUnresolved":        false,
		"showOrphans":           true,
		"collapse-color-groups": false,
		"colorGroups": []graphColorGroup{
			group("tag:person", 0x7CB342),       // green
			group("tag:organization", 0xFFB300), // amber
			group("tag:project", 0x42A5F5),      // blue
			group("tag:role", 0xAB47BC),         // purple
			group("tag:preference", 0xFF7043),   // coral
		},
		"collapse-display":   true,
		"showArrow":          false,
		"textFadeMultiplier": 0,
		"nodeSizeMultiplier": 1.4,
		"lineSizeMultiplier": 1,
		"collapse-forces":    true,
		"centerStrength":     0.4,
		"repelStrength":      12,
		"linkStrength":       1,
		"linkDistance":       200,
		"nodeSize":           6,
		"scale":              0.8,
		"close":              false,
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return string(data)
}

// WriteGraphConfig writes the .obsidian/graph.json template into vaultRoot
// (the directory Obsidian opens as a vault). Best-effort: Obsidian regenerates
// this file on first open if missing.
func WriteGraphConfig(vaultRoot string) error {
	cfgDir := filepath.Join(vaultRoot, ".obsidian")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfgDir, "graph.json"), []byte(graphJSONTemplate()), 0o644)
}

// PLACEHOLDER maps an entity label to a filename-safe note name. It is
// the single authority for entity note naming: ExportObsidian writes notes
// with it and every wikilink to an entity must use it, or the link will not
// EntityNoteNames assigns each node its Obsidian note filename (without
// .md). Deterministic for a given node slice: labels sanitize to their base
// name, and when two distinct nodes sanitize to the same name ("A B" and
// "A_B" both become "A_B") the later one in canonical-ID order gets a
// numeric suffix, so both notes exist instead of the second write silently
// destroying the first. ExportObsidian writes by these names; anything that
// links to entity notes from inside this package must resolve through it.
func EntityNoteNames(nodes []Node) map[string]string {
	ordered := make([]Node, len(nodes))
	copy(ordered, nodes)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	used := make(map[string]bool, len(ordered))
	names := make(map[string]string, len(ordered))
	for _, n := range ordered {
		base := SanitizeFilename(n.Label)
		if base == "" {
			base = "entity"
		}
		name := base
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		used[name] = true
		names[n.ID] = name
	}
	return names
}

// resolve in Obsidian. Covers the characters Obsidian's link syntax and
// Windows filenames both reject, and trims trailing dots/spaces.
func SanitizeFilename(s string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "-", "\"", "-", "<", "-", ">", "-", "|", "-",
		"#", "-", "^", "-", "[", "-", "]", "-", " ", "_")
	return strings.TrimRight(r.Replace(s), ". ")
}
