package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// The weak-match block tells the caller to "record it with memory_alias".
// The uninstructed-agent
// arm measured what an unresolvable suggestion costs: 98 calls, 0 aliases
// written, a store that learns nothing — against 6 calls and 6 aliases once
// the affordance was named in the caller's own context. This pins the
// pairing from the CLI side; pkg/memory's TestWeakMatchBlockNamesTheActionCommand
// pins the text side. If block text and command ever drift apart again, the
// build fails instead of the experiment.
func TestFeedbackActionResolvesInCLI(t *testing.T) {
	root := &cobra.Command{Use: "graymatter"}
	root.AddCommand(aliasCmd())

	// The name must resolve both as a command alias and through cobra's
	// argument resolution, which is what a caller typing the block's
	// suggestion actually exercises.
	found := false
	for _, a := range aliasCmd().Aliases {
		if a == "memory_alias" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the alias command must answer to %q — the frozen feedback block names it", "memory_alias")
	}

	sub, _, err := root.Find([]string{"memory_alias", "agent-1", "term", "equivalent"})
	if err != nil {
		t.Fatalf("memory_alias does not resolve in the command tree: %v", err)
	}
	if sub.Name() != "alias" {
		t.Fatalf("memory_alias resolved to %q, want the alias command", sub.Name())
	}
}
