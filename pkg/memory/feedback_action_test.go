package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// The text side of the pairing the uninstructed-agent arm exposed.
//
// The CLI registers memory.FeedbackAction as an alias of its alias command,
// so the two halves already read one symbol and cannot be edited apart. What
// a constant cannot pin is that the block still ACTUALLY prints it: someone
// rewording the action line could drop the name entirely and leave the caller
// with advice naming no action at all, which is the failure this arm measured
// — 98 calls, 0 aliases, a store that learned nothing.
//
// So this asserts on a block produced by a real recall, not on the format
// string.
func TestWeakMatchBlockNamesTheActionCommand(t *testing.T) {
	ctx := context.Background()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 8760 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	for _, txt := range []string{
		"billing submissions go out on the first of the month",
		"billing submissions are reconciled by finance",
		"the billing team owns the submission window",
		"submission errors are queued for review",
		"finance signs off on the monthly close",
	} {
		if err := s.Put(ctx, "action", txt); err != nil {
			t.Fatal(err)
		}
	}

	// A query whose vocabulary the store does not share: this is the case the
	// spec's trigger exists for.
	_, block, err := s.RecallDetailed(ctx, "action", "who runs the clearinghouse?", 8)
	if err != nil {
		t.Fatal(err)
	}
	if block == "" {
		t.Fatal("the weak-match trigger did not fire; this test needs the block it is asserting on")
	}
	if !strings.Contains(block, FeedbackAction) {
		t.Errorf("the block does not name the action a caller can take.\n"+
			"want it to contain %q\ngot:\n%s", FeedbackAction, block)
	}
}

// The specified block text is frozen. Routing the action name through a constant must
// not have changed a byte of what the caller reads.
//
// One deliberate exception, recorded here rather than absorbed: the
// specification was written in Spanish and so was this block, in a product whose every other
// user-facing string is English. The block was translated before it shipped.
// What the experiment measured is untouched by that — the trigger, the
// neighbourhood, the ten-term ceiling and the action name are the same, and
// the action name is the only part any measurement depended on. The pin
// below now holds the English text to the same standard the Spanish one had.
func TestWeakMatchBlockTextIsUnchanged(t *testing.T) {
	const want = "If one of those is what you meant, re-query with that word. " +
		"If your term is a synonym this store does not know, record it with " +
		"memory_alias and the next query finds it."

	ctx := context.Background()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 8760 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, txt := range []string{
		"billing submissions go out on the first of the month",
		"billing submissions are reconciled by finance",
		"the billing team owns the submission window",
		"submission errors are queued for review",
		"finance signs off on the monthly close",
	} {
		if err := s.Put(ctx, "action", txt); err != nil {
			t.Fatal(err)
		}
	}
	_, block, err := s.RecallDetailed(ctx, "action", "who runs the clearinghouse?", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(block, want) {
		t.Errorf("the frozen action line changed.\nwant suffix:\n%s\ngot:\n%s", want, block)
	}
}
