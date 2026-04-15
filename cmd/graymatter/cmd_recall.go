package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	graymatter "github.com/angelnicolasc/graymatter"
)

func recallCmd() *cobra.Command {
	var topK int
	var shared bool
	var all bool

	cmd := &cobra.Command{
		Use:   "recall <agent-id> <query>",
		Short: "Retrieve relevant memories for an agent",
		Example: `  graymatter recall "sales-closer" "follow up Maria"
  graymatter recall "code-reviewer" "nil pointer" --top-k 5
  graymatter recall --shared "global preferences"
  graymatter recall --all "sales-closer" "Maria follow up"`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID, query := args[0], args[1]
			cfg := graymatter.DefaultConfig()
			cfg.DataDir = dataDir
			if topK > 0 {
				cfg.TopK = topK
			}

			mem, err := graymatter.NewWithConfig(cfg)
			if err != nil {
				return err
			}
			defer mem.Close()

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			var facts []string
			var scope string
			switch {
			case all:
				facts, err = mem.RecallAll(ctx, agentID, query)
				scope = "all"
			case shared:
				facts, err = mem.RecallShared(ctx, query)
				scope = "shared"
			default:
				facts, err = mem.Recall(ctx, agentID, query)
				scope = agentID
			}
			if err != nil {
				return err
			}

			if jsonOut {
				data, _ := json.Marshal(map[string]any{
					"agent_id": agentID,
					"scope":    scope,
					"query":    query,
					"facts":    facts,
					"count":    len(facts),
				})
				fmt.Println(string(data))
				return nil
			}

			if len(facts) == 0 {
				if !quiet {
					fmt.Printf("No memories found for agent %q matching %q.\n", agentID, query)
				}
				return nil
			}

			if !quiet {
				fmt.Printf("# Memory context [%s] / %q\n\n", scope, query)
			}
			fmt.Println(strings.Join(facts, "\n"))
			return nil
		},
	}
	cmd.Flags().IntVar(&topK, "top-k", 0, "maximum facts to return (default from config)")
	cmd.Flags().BoolVar(&shared, "shared", false, "recall from shared memory only")
	cmd.Flags().BoolVar(&all, "all", false, "recall from both agent and shared memory, merged")
	return cmd
}
