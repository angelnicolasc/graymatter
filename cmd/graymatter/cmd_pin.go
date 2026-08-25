package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// pinCmd and unpinCmd expose the pin exemption (ADR-010) on the CLI: a
// pinned fact is exempt from decay, pruning and summarisation, so a dormant
// project cannot collect standing obligations or architecture decisions.
// Pinning is visible by design — status counts pinned facts, the TUI marks
// them, export flags them — because an invisible exemption would reintroduce
// the stale-fact problem the decay model exists to prevent.

func pinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pin <agent-id> <text>",
		Short: "Exempt a fact from decay, pruning and summarisation",
		Long: `Mark a fact as permanent. Pinned facts are exempt from the forgetting
curve (ADR-001) and from consolidation: they are never pruned, never
summarised away, and their weight does not decay. The pin is visible —
status counts pinned facts, the TUI marks them, export flags them — and
graymatter unpin restores normal decay.`,
		Example: `  graymatter pin "backend-architect" "The write path is single-writer by design; readers never block on consolidation"`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPin(true, args)
		},
	}
}

func unpinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpin <agent-id> <text>",
		Short: "Restore normal decay for a pinned fact",
		Example: `  graymatter unpin "backend-architect" "The write path is single-writer by design; readers never block on consolidation"`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPin(false, args)
		},
	}
}

func runPin(pinned bool, args []string) error {
	agentID, text := args[0], args[1]
	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	facts, err := store.List(agentID)
	if err != nil {
		return fmt.Errorf("list facts: %w", err)
	}
	var victim memory.Fact
	found := false
	for _, f := range facts {
		if f.Text == text {
			victim = f
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("fact not found for agent %q: %q", agentID, text)
	}
	if victim.IsSuperseded() {
		return fmt.Errorf("cannot pin a superseded fact")
	}
	if victim.Pinned == pinned {
		if !quiet {
			state := "pinned"
			if !pinned {
				state = "not pinned"
			}
			fmt.Printf("Nothing to do: fact is already %s.\n", state)
		}
		return nil
	}
	victim.Pinned = pinned
	if pinned {
		victim.PinnedAt = time.Now().UTC()
	} else {
		victim.PinnedAt = time.Time{}
	}
	if err := store.UpdateFact(agentID, victim); err != nil {
		return fmt.Errorf("update fact: %w", err)
	}

	if jsonOut {
		data, _ := json.Marshal(map[string]any{"agent_id": agentID, "pinned": pinned})
		fmt.Println(string(data))
	} else if !quiet {
		verb := "Pinned"
		if !pinned {
			verb = "Unpinned"
		}
		fmt.Printf("%s: [%s] %s\n", verb, agentID, text)
	}
	return nil
}
