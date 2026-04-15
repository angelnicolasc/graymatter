package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

func checkpointCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Manage agent session checkpoints",
	}
	cmd.AddCommand(checkpointListCmd(), checkpointResumeCmd(), checkpointSaveCmd())
	return cmd
}

func checkpointListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <agent-id>",
		Short: "List all checkpoints for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			mem, err := openMem()
			if err != nil {
				return err
			}
			defer mem.Close()

			store := mem.Advanced()
			if store == nil {
				return fmt.Errorf("store not initialised")
			}

			checkpoints, err := session.List(store.DB(), agentID)
			if err != nil {
				return err
			}

			if jsonOut {
				data, _ := json.MarshalIndent(checkpoints, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			if len(checkpoints) == 0 {
				fmt.Printf("No checkpoints for agent %q.\n", agentID)
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tCREATED\tMESSAGES")
			for _, cp := range checkpoints {
				fmt.Fprintf(w, "%s\t%s\t%d\n",
					cp.ID,
					cp.CreatedAt.Format(time.RFC3339),
					len(cp.Messages),
				)
			}
			return w.Flush()
		},
	}
}

func checkpointResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <agent-id> [checkpoint-id]",
		Short: "Print the latest (or specified) checkpoint state",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			mem, err := openMem()
			if err != nil {
				return err
			}
			defer mem.Close()

			store := mem.Advanced()
			if store == nil {
				return fmt.Errorf("store not initialised")
			}

			var cp *session.Checkpoint
			if len(args) == 2 {
				cp, err = session.Load(store.DB(), agentID, args[1])
			} else {
				cp, err = session.Resume(store.DB(), agentID)
			}
			if err != nil {
				return err
			}

			data, _ := json.MarshalIndent(cp, "", "  ")
			fmt.Println(string(data))
			return nil
		},
	}
}

func checkpointSaveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "save <agent-id>",
		Short: "Save an empty checkpoint for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			mem, err := openMem()
			if err != nil {
				return err
			}
			defer mem.Close()

			store := mem.Advanced()
			if store == nil {
				return fmt.Errorf("store not initialised")
			}

			cp := session.Checkpoint{
				AgentID:  agentID,
				Metadata: map[string]string{"source": "cli"},
			}
			saved, err := session.Save(store.DB(), cp)
			if err != nil {
				return err
			}
			fmt.Printf("Checkpoint %q saved for agent %q.\n", saved.ID, agentID)
			return nil
		},
	}
}

func openMem() (*graymatter.Memory, error) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir
	return graymatter.NewWithConfig(cfg)
}
