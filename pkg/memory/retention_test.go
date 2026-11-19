package memory

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestConsolidate_PrunesAgedFactsAndTombstones(t *testing.T) {
	const agent = "retention"
	dir := t.TempDir()
	store := openForgetStore(t, dir)
	defer func() { _ = store.Close() }()

	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	ctx := context.Background()
	put := func(text string) Fact {
		fact, err := store.putReturningFact(ctx, agent, text)
		if err != nil {
			t.Fatalf("put %q: %v", text, err)
		}
		return fact
	}
	agedLive := put("aged live fact")
	agedRetired := put("aged retired fact")
	if err := store.Retire(agent, agedRetired); err != nil {
		t.Fatalf("retire aged fixture: %v", err)
	}
	agedRetired.SupersededBy = SupersededByAgent
	pinned := put("aged pinned fact")
	if err := store.SetPinned(agent, true, pinned); err != nil {
		t.Fatalf("pin: %v", err)
	}
	pinned.Pinned = true

	// After 201 days at the default half-life, weight 1 falls below 0.01.
	now = now.Add(201 * 24 * time.Hour)
	youngLive := put("young live fact")
	youngRetired := put("young retired fact")
	if err := store.Retire(agent, youngRetired); err != nil {
		t.Fatalf("retire young fixture: %v", err)
	}
	youngRetired.SupersededBy = SupersededByAgent
	assertRetentionRows(t, store, agent, agedLive, agedRetired, pinned, youngLive, youngRetired)

	if err := store.Consolidate(ctx, agent, defaultTestCfg()); err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	assertRetentionRows(t, store, agent, pinned, youngLive, youngRetired)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store = openForgetStore(t, dir)
	assertRetentionRows(t, store, agent, pinned, youngLive, youngRetired)
}

func TestDelete_RemovesRecentPinnedFactImmediately(t *testing.T) {
	const agent = "retention"
	dir := t.TempDir()
	store := openForgetStore(t, dir)
	defer func() { _ = store.Close() }()
	store.now = func() time.Time { return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC) }

	fact, err := store.putReturningFact(context.Background(), agent, "recent pinned fact")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.SetPinned(agent, true, fact); err != nil {
		t.Fatalf("pin: %v", err)
	}
	fact.Pinned = true
	assertRetentionRows(t, store, agent, fact)

	if err := store.Delete(agent, fact.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertRetentionRows(t, store, agent)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store = openForgetStore(t, dir)
	assertRetentionRows(t, store, agent)
}

func assertRetentionRows(t *testing.T, store *Store, agent string, want ...Fact) {
	t.Helper()
	got, err := store.List(agent)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("stored rows = %v, want %v", got, want)
	}
	for _, expected := range want {
		if !slices.ContainsFunc(got, func(row Fact) bool {
			return row.ID == expected.ID && row.Pinned == expected.Pinned && row.SupersededBy == expected.SupersededBy
		}) {
			t.Errorf("expected %q (pinned=%t, superseded_by=%q) among %v", expected.Text, expected.Pinned, expected.SupersededBy, got)
		}
	}
}
