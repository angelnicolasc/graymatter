package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	graymatter "github.com/angelnicolasc/graymatter"
)

func rememberCmd() *cobra.Command {
	var shared bool

	cmd := &cobra.Command{
		Use:   "remember <agent-id> <text>",
		Short: "Store a fact for an agent",
		Example: `  graymatter remember "sales-closer" "Maria didn't reply Wednesday. Third touchpoint due Friday."
  graymatter remember "code-reviewer" "Always check for nil pointer dereferences in Go code."
  graymatter remember --shared "Global preference: always use bullet points."`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID, text := args[0], args[1]
			cfg := graymatter.DefaultConfig()
			cfg.DataDir = dataDir

			mem, err := graymatter.NewWithConfig(cfg)
			if err != nil {
				return err
			}
			defer mem.Close()

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			if shared {
				if err := mem.RememberShared(ctx, text); err != nil {
					return err
				}
				if jsonOut {
					data, _ := json.Marshal(map[string]string{"scope": "shared", "status": "stored"})
					fmt.Println(string(data))
				} else if !quiet {
					fmt.Printf("Remembered (shared): %s\n", text)
				}
				return nil
			}

			if err := mem.Remember(ctx, agentID, text); err != nil {
				return err
			}

			if jsonOut {
				data, _ := json.Marshal(map[string]string{"agent_id": agentID, "status": "stored"})
				fmt.Println(string(data))
			} else if !quiet {
				fmt.Printf("Remembered: [%s] %s\n", agentID, text)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&shared, "shared", false, "store in shared memory (readable by all agents)")
	return cmd
}
