package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

// inspectGraph observes an existing database or daemon without initializing
// either. When no graph exists, callers can report an empty inspection.
func inspectGraph(dir string) (result daemon.KGInspection, err error) {
	if err := doctorDBLeaf(dir); err != nil {
		if errors.Is(err, errDoctorNoDatabase) {
			return result, nil
		}
		return result, err
	}
	if !noDaemon && os.Getenv("GRAYMATTER_NO_DAEMON") != "1" {
		if client, dialErr := daemon.ConnectNoSpawn(dir); dialErr == nil {
			defer func() { err = errors.Join(err, client.Close()) }()
			return client.KGInspect()
		}
	}
	store, err := doctorOpenReadOnlyStore(dir)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	if store.DB() == nil {
		return result, errors.New("store has no database handle")
	}
	graph, err := kg.OpenRead(store.DB())
	if errors.Is(err, kg.ErrNoGraph) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return daemon.ReadKGInspection(graph)
}

func kgInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [from to relation]",
		Short: "Inspect graph edge provenance without changing the graph",
		Long: `List graph edges and pending provenance. Supply all three identifiers to
select one edge. JSON includes each observed edge snapshot and its pending
reason, when known. This command can use an already running daemon.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return cobra.ExactArgs(3)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			graph, err := inspectGraph(dataDir)
			if err != nil {
				return fmt.Errorf("inspect graph: %w", err)
			}
			edges := make([]kg.EdgeInspection, 0)
			for _, entry := range graph.Edges {
				edge := entry.Edge
				if len(args) == 3 && (edge.From != args[0] || edge.To != args[1] || edge.Relation != args[2]) {
					continue
				}
				edges = append(edges, entry)
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					Provenance *kg.ProvenanceReport `json:"provenance,omitempty"`
					Edges      []kg.EdgeInspection  `json:"edges"`
				}{Provenance: graph.Provenance, Edges: edges})
			}
			if graph.Provenance != nil {
				fmt.Fprintln(out, graphProvenanceSummary(*graph.Provenance))
			}
			if len(edges) == 0 {
				fmt.Fprintln(out, "No matching graph edges.")
			}
			for _, item := range edges {
				state := "complete"
				if item.PendingReason != "" {
					state = "pending (" + item.PendingReason + ")"
				}
				fmt.Fprintf(out, "%s -> %s [%s] · %s\n", item.Edge.From, item.Edge.To, item.Edge.Relation, state)
			}
			return nil
		},
	}
}
