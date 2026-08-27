package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kgrender"
)

func kgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kg",
		Short: "Knowledge graph inspection and rendering",
		Long: `Work with the auto-populated knowledge graph: entities extracted
from memory, edges with fact-ID receipts.

Enable auto-population with ` + "`graymatter init --kg`" + ` (writes the kg.auto
sentinel) or by running the daemon with --kg.`,
	}
	cmd.AddCommand(kgRenderCmd())
	return cmd
}

func kgRenderCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render the knowledge graph as a self-contained HTML page or Graphviz DOT",
		Long: `Renders the current knowledge graph to a file.

The HTML output is a single self-contained page — inline force-directed
SVG, no CDN, no external fonts or scripts — so it works offline and can be
committed or attached. Node colour is the entity type, node size the
weight, edge thickness the weight, and hovering an edge shows its
receipts: the fact IDs the co-mention was extracted from.

The DOT output is Graphviz source for people who run their own layout.

The format follows the --out extension (.html or .dot); --html on
` + "`graymatter doctor --graph`" + ` writes the same page for quick inspection.`,
		Example: `  graymatter kg render
  graymatter kg render --out graph.html
  graymatter kg render --out graph.dot && dot -Tsvg graph.dot -o graph.svg`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				out = "kg-graph.html"
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			nodes, err := store.KGNodes()
			if err != nil {
				return fmt.Errorf("read graph nodes: %w", err)
			}
			edges, err := store.KGEdges()
			if err != nil {
				return fmt.Errorf("read graph edges: %w", err)
			}
			if len(nodes) == 0 {
				return fmt.Errorf("knowledge graph is empty; enable auto-population with `graymatter init --kg` (or daemon --kg), then let consolidation run")
			}

			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil && !os.IsExist(err) {
				return fmt.Errorf("mkdir: %w", err)
			}
			f, err := os.Create(out)
			if err != nil {
				return fmt.Errorf("create %s: %w", out, err)
			}
			defer func() { _ = f.Close() }()

			switch strings.ToLower(filepath.Ext(out)) {
			case ".html", ".htm":
				err = renderKGHTML(f, nodes, edges)
			case ".dot", ".gv":
				err = renderKGDOT(f, nodes, edges)
			default:
				return fmt.Errorf("cannot infer format from %q (want .html or .dot)", out)
			}
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Printf("Rendered %d entities and %d edges to %s\n", len(nodes), len(edges), out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "output file; extension picks the format: .html (self-contained page) or .dot (Graphviz). Default kg-graph.html")
	return cmd
}

// Indirections so tests can stub the renderers without a filesystem.
var (
	renderKGHTML = kgrender.HTML
	renderKGDOT  = kgrender.DOT
)
