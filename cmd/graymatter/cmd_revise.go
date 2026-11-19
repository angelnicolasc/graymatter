package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

// reviseCmd and forgetCmd close the gap that made supersede unreachable from
// the CLI. The tombstone itself is not new — ADR-007 introduced it in v0.10.0
// and memory_reflect has driven it over MCP since — but an operator with a
// terminal had no way to record that a fact was replaced. The consequence was
// measurable: with no edge to follow, Recall ranks three versions of the same
// value as three independent facts, and the caller cannot tell which one
// holds. The CLI and MCP apply the same revision and retirement contracts.
//
// Matching a fact by its text is deliberately permissive: an exact match wins,
// and otherwise a case-insensitive substring is accepted **only when it is
// unique**. Requiring the operator to retype a sentence verbatim is the kind
// of friction that leaves corrections unrecorded, which is the failure this
// command exists to remove; requiring uniqueness is what keeps the permissive
// match from retiring the wrong fact.

func reviseCmd() *cobra.Command {
	var byID string
	var confidence string
	var shared bool
	cmd := &cobra.Command{
		Use:     "revise <agent-id> <old-fact> <new-fact>",
		Aliases: []string{"update", "supersede"},
		Short:   "Replace a fact with a corrected one and mark the original as retired",
		Long: `Record that a fact was superseded by a newer one.

The replacement and all selected source retirements commit together. A changed
source or failed write rolls back the revision. Recall excludes retired rows
immediately. Ordinary pruning can remove unpinned retirement rows. The recorded
source versions remain inspectable while the replacement owns their evidence.

<old-fact> matches on exact text, or on a unique case-insensitive substring.
Use --id to target a fact by its identifier when the text is ambiguous.`,
		Example: `  graymatter revise "backend" "the session timeout is 30 minutes" "the session timeout is 10 minutes"
  graymatter revise --id 01M16NAF5W5Y5SM0PBNYVAZQ37 "backend" "" "the session timeout is 10 minutes"`,
		Args: func(cmd *cobra.Command, args []string) error {
			if shared && len(args) == 2 {
				return nil
			}
			return cobra.ExactArgs(3)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := cliWriteOptions(cmd, confidence)
			if err != nil {
				return err
			}
			agentID := args[0]
			if shared {
				agentID = memory.SharedAgentID
			}
			return runRevise(cmd.Context(), agentID, args[len(args)-2], args[len(args)-1], byID, opts)
		},
	}
	cmd.Flags().StringVar(&byID, "id", "", "target the fact with this ID instead of matching its text")
	cmd.Flags().StringVar(&confidence, "confidence", "", "replacement confidence; omitted: min(inferred, effective target confidence)")
	cmd.Flags().BoolVar(&shared, "shared", false, "revise shared memory")
	return cmd
}

func forgetCmd() *cobra.Command {
	var byID string
	cmd := &cobra.Command{
		Use:   "forget <agent-id> <fact>",
		Short: "Retire a fact that has no replacement",
		Long: `Tombstone a fact with nothing to put in its place.

Recall stops returning it immediately. The fact remains in the store as a
tombstone. Ordinary pruning can remove an unpinned tombstone once its weight
drops below the threshold.

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

func runRevise(ctx context.Context, agentID, oldText, newText, byID string, options ...memory.WriteOptions) error {
	var opts memory.WriteOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if err := opts.Validate(); err != nil {
		return err
	}
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

	writer, ok := store.(rpc.ConfidenceWriter)
	if !ok {
		return errors.New("atomic revision is unavailable on this backend")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	replacement, err := writer.ReviseFactsWithOptions(ctx, agentID, newText, opts, live...)
	if err != nil {
		if replacement.ID != "" {
			return fmt.Errorf("revision committed as fact_id %s (confidence %s). Follow-up error: %w",
				replacement.ID, memory.EffectiveConfidence(replacement.Confidence), err)
		}
		return fmt.Errorf("revise fact: %w", err)
	}
	if replacement.ID == "" {
		return errors.New("atomic revision returned no committed fact")
	}
	replacementID, replacementConfidence := replacement.ID, replacement.Confidence
	retired := make([]string, 0, len(live))
	for _, victim := range live {
		retired = append(retired, victim.ID)
	}

	if jsonOut {
		data, _ := json.Marshal(map[string]any{
			"agent_id":      agentID,
			"superseded":    retired,
			"superseded_by": replacementID,
			"retired_text":  live[0].Text,
			"replacement":   newText,
			"confidence":    replacementConfidence,
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
	patcher, patches := store.(rpc.ConfidenceLifecycle)
	if patches {
		if err := patcher.Retire(agentID, live...); err != nil {
			return fmt.Errorf("retire the fact: %w", err)
		}
	}
	for _, v := range live {
		v.SupersededBy = memory.SupersededByAgent
		if !patches {
			if err := store.UpdateFact(agentID, v); err != nil {
				return fmt.Errorf("retire the fact: %w", err)
			}
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
