package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// reviseCmd and forgetCmd close the gap that made supersede unreachable from
// the CLI. The tombstone itself is not new — ADR-007 introduced it in v0.10.0
// and memory_reflect has driven it over MCP since — but an operator with a
// terminal had no way to record that a fact was replaced. The consequence was
// measurable: with no edge to follow, Recall ranks three versions of the same
// value as three independent facts, and the caller cannot tell which one
// holds. Both commands are the exact CLI mirror of the MCP actions, down to
// the write ordering, so a store curated from the shell and one curated by an
// agent end up in the same state.
//
// Matching a fact by its text is deliberately permissive: an exact match wins,
// and otherwise a case-insensitive substring is accepted **only when it is
// unique**. Requiring the operator to retype a sentence verbatim is the kind
// of friction that leaves corrections unrecorded, which is the failure this
// command exists to remove; requiring uniqueness is what keeps the permissive
// match from retiring the wrong fact.

func reviseCmd() *cobra.Command {
	var byID string
	cmd := &cobra.Command{
		Use:     "revise <agent-id> <old-fact> <new-fact>",
		Aliases: []string{"update", "supersede"},
		Short:   "Replace a fact with a corrected one, keeping the receipt",
		Long: `Record that a fact was superseded by a newer one.

The replacement is written first, then the old fact is tombstoned with a
pointer to it, so a failure part-way through can never leave the agent with a
retired fact and nothing in its place. The old fact is not deleted: it stays
in the store, stays visible to list, export and the TUI, and keeps its receipt
(ADR-007). What changes is that Recall stops returning it from the next query
onward, so the caller sees one answer instead of three.

<old-fact> matches on exact text, or on a unique case-insensitive substring.
Use --id to target a fact by its identifier when the text is ambiguous.`,
		Example: `  graymatter revise "backend" "the session timeout is 30 minutes" "the session timeout is 10 minutes"
  graymatter revise --id 01M16NAF5W5Y5SM0PBNYVAZQ37 "backend" "" "the session timeout is 10 minutes"`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRevise(cmd.Context(), args[0], args[1], args[2], byID)
		},
	}
	cmd.Flags().StringVar(&byID, "id", "", "target the fact with this ID instead of matching its text")
	return cmd
}

func forgetCmd() *cobra.Command {
	var byID string
	cmd := &cobra.Command{
		Use:   "forget <agent-id> <fact>",
		Short: "Retire a fact that has no replacement",
		Long: `Tombstone a fact with nothing to put in its place.

Recall stops returning it immediately. Like revise, this is not a delete: the
fact stays in the store with a receipt recording that an agent retired it, and
ordinary decay collects it on the same curve as everything else.

<fact> matches on exact text, or on a unique case-insensitive substring.`,
		Example: `  graymatter forget "backend" "deploys are frozen on Fridays"`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runForget(args[0], args[1], byID)
		},
	}
	cmd.Flags().StringVar(&byID, "id", "", "target the fact with this ID instead of matching its text")
	return cmd
}

// findFacts resolves the facts to retire from an ID, an exact text match, or a
// unique case-insensitive substring — in that order.
//
// Exact text can legitimately match several facts: the same sentence written
// in two sessions is the same belief stored twice, and retiring only one of
// them leaves the other live — which is precisely the stale-fact failure this
// command exists to remove. So every exact match is retired together.
//
// A substring matching several *different* sentences is a different situation:
// those are different beliefs, and guessing which one the operator meant would
// retire the wrong one. That is an error naming the candidates.
func findFacts(facts []memory.Fact, wanted, byID string) ([]memory.Fact, error) {
	if byID != "" {
		for _, f := range facts {
			if f.ID == byID {
				return []memory.Fact{f}, nil
			}
		}
		return nil, fmt.Errorf("no fact with id %q", byID)
	}
	if strings.TrimSpace(wanted) == "" {
		return nil, fmt.Errorf("the fact to target is required (or pass --id)")
	}
	var exact []memory.Fact
	for _, f := range facts {
		if f.Text == wanted {
			exact = append(exact, f)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}

	needle := strings.ToLower(wanted)
	var hits []memory.Fact
	distinct := make(map[string]bool)
	for _, f := range facts {
		if strings.Contains(strings.ToLower(f.Text), needle) {
			hits = append(hits, f)
			distinct[f.Text] = true
		}
	}
	switch {
	case len(hits) == 0:
		return nil, fmt.Errorf("fact not found: %q", wanted)
	case len(distinct) == 1:
		return hits, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d different facts; use --id or a longer phrase:", wanted, len(distinct))
		for _, f := range hits {
			fmt.Fprintf(&b, "\n  %s  %s", f.ID, f.Text)
		}
		return nil, fmt.Errorf("%s", b.String())
	}
}

func runRevise(ctx context.Context, agentID, oldText, newText, byID string) error {
	if strings.TrimSpace(newText) == "" {
		return fmt.Errorf("the corrected fact is required")
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	before, err := store.List(agentID)
	if err != nil {
		return fmt.Errorf("list facts: %w", err)
	}
	victims, err := findFacts(before, oldText, byID)
	if err != nil {
		return err
	}
	live := victims[:0:0]
	for _, v := range victims {
		if !v.IsSuperseded() {
			live = append(live, v)
		}
	}
	if len(live) == 0 {
		return fmt.Errorf("that fact is already superseded: %q", victims[0].Text)
	}

	// Write the correction before retiring what it corrects — the same order
	// the MCP handler uses, and for the same reason.
	if ctx == nil {
		ctx = context.Background()
	}
	if err := store.Remember(ctx, agentID, newText); err != nil {
		return fmt.Errorf("write the corrected fact: %w", err)
	}

	replacementID := newlyAddedID(store, agentID, before)
	if replacementID == "" {
		replacementID = memory.SupersededByAgent
	}
	retired := make([]string, 0, len(live))
	for _, v := range live {
		v.SupersededBy = replacementID
		if err := store.UpdateFact(agentID, v); err != nil {
			return fmt.Errorf("supersede the old fact: %w", err)
		}
		retired = append(retired, v.ID)
	}

	if jsonOut {
		data, _ := json.Marshal(map[string]any{
			"agent_id":      agentID,
			"superseded":    retired,
			"superseded_by": replacementID,
			"retired_text":  live[0].Text,
			"replacement":   newText,
		})
		fmt.Println(string(data))
	} else if !quiet {
		fmt.Printf("Revised: [%s]\n  was: %s", agentID, live[0].Text)
		if len(live) > 1 {
			fmt.Printf("  (%d identical copies retired)", len(live))
		}
		fmt.Printf("\n  now: %s\n", newText)
	}
	return nil
}

func runForget(agentID, text, byID string) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	facts, err := store.List(agentID)
	if err != nil {
		return fmt.Errorf("list facts: %w", err)
	}
	victims, err := findFacts(facts, text, byID)
	if err != nil {
		return err
	}
	live := victims[:0:0]
	for _, v := range victims {
		if !v.IsSuperseded() {
			live = append(live, v)
		}
	}
	if len(live) == 0 {
		return fmt.Errorf("that fact is already superseded: %q", victims[0].Text)
	}

	retired := make([]string, 0, len(live))
	for _, v := range live {
		v.SupersededBy = memory.SupersededByAgent
		if err := store.UpdateFact(agentID, v); err != nil {
			return fmt.Errorf("retire the fact: %w", err)
		}
		retired = append(retired, v.ID)
	}

	if jsonOut {
		data, _ := json.Marshal(map[string]any{
			"agent_id":     agentID,
			"superseded":   retired,
			"retired_text": live[0].Text,
		})
		fmt.Println(string(data))
	} else if !quiet {
		fmt.Printf("Retired: [%s] %s", agentID, live[0].Text)
		if len(live) > 1 {
			fmt.Printf("  (%d identical copies)", len(live))
		}
		fmt.Println()
	}
	return nil
}

// newlyAddedID returns the ID of the fact added since the `before` snapshot.
// Matching on identity rather than text keeps it correct when the correction
// repeats wording that is already stored.
func newlyAddedID(store cliStore, agentID string, before []memory.Fact) string {
	after, err := store.List(agentID)
	if err != nil {
		return ""
	}
	known := make(map[string]bool, len(before))
	for _, f := range before {
		known[f.ID] = true
	}
	for _, f := range after {
		if !known[f.ID] {
			return f.ID
		}
	}
	return ""
}
