package memory

import (
	"context"
	"fmt"
)

// Revise writes newText and retires every fact in victims, pointing each
// tombstone at the fact that replaced it. It returns the replacement's ID.
//
// The ordering is the contract, not an implementation detail: the replacement
// is written first, so a failure part-way through leaves the agent with both
// values rather than with a retired fact and nothing in its place. Retiring
// first and crashing before the write loses the belief entirely.
//
// Every victim is retired in the same call because the alternative — retiring
// one copy of a belief that was stored twice — leaves the other live, which is
// the stale-fact failure the tombstone exists to prevent.
//
// Nothing is deleted. Victims keep their text, their receipt and their decay
// curve (ADR-007); what changes is that Recall drops them before scoring, and
// the live fact's receipt names them under Provenance.Supersedes.
//
// The revision benchmark calls this method directly. The CLI implements its
// own daemon-capable revision path because Revise is not part of the RPC surface.
func (s *Store) Revise(ctx context.Context, agentID, newText string, victims ...Fact) (string, error) {
	if newText == "" {
		return "", fmt.Errorf("revise: the replacement text is required")
	}
	for _, v := range victims {
		if v.IsSuperseded() {
			return "", fmt.Errorf("revise: %q is already superseded", v.Text)
		}
	}

	// The replacement inherits the victim's kind: a revised alias stays an
	// alias — and stays non-injectable — instead of leaking back into the
	// result set as a content fact whose text happens to start with
	// "alias:". With mixed victims the first one wins; revising across kinds
	// has no coherent meaning and callers should not do it.
	kind := KindFact
	if len(victims) > 0 {
		kind = victims[0].Kind
	}
	replacement, err := s.putReturningFactKind(ctx, agentID, newText, kind, "")
	if err != nil {
		return "", fmt.Errorf("revise: write the replacement: %w", err)
	}
	replacementID := replacement.ID

	for _, v := range victims {
		v.SupersededBy = replacementID
		if err := s.UpdateFact(agentID, v); err != nil {
			return replacementID, fmt.Errorf("revise: retire %q: %w", v.Text, err)
		}
	}
	return replacementID, nil
}

// Retire tombstones facts that have nothing to replace them. Recall stops
// returning them at once; the facts themselves stay in the store with a
// receipt recording that an agent dropped them.
func (s *Store) Retire(agentID string, victims ...Fact) error {
	for _, v := range victims {
		if v.IsSuperseded() {
			return fmt.Errorf("retire: %q is already superseded", v.Text)
		}
	}
	for _, v := range victims {
		v.SupersededBy = SupersededByAgent
		if err := s.UpdateFact(agentID, v); err != nil {
			return fmt.Errorf("retire %q: %w", v.Text, err)
		}
	}
	return nil
}
