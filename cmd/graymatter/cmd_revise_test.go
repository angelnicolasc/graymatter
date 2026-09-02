package main

import (
	"context"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// seedStore builds a throwaway store in a temp dir and returns a reopener, so
// each case here writes through the same CLI surface an operator would use.
func seedRevise(t *testing.T, agent string, facts ...string) {
	t.Helper()
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	oldDir := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = oldDir })

	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if err := mem.Remember(context.Background(), agent, f); err != nil {
			t.Fatal(err)
		}
	}
	_ = mem.Close()
}

func liveFacts(t *testing.T, agent string) []memory.Fact {
	t.Helper()
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	all, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	var live []memory.Fact
	for _, f := range all {
		if !f.IsSuperseded() {
			live = append(live, f)
		}
	}
	return live
}

func recallTexts(t *testing.T, agent, query string) []string {
	t.Helper()
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	out, err := store.Recall(context.Background(), agent, query, 10)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The point of the command: after a revision, Recall answers with one value
// instead of ranking three versions as independent facts.
func TestReviseRemovesTheStaleValueFromRecall(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent,
		"the session timeout is 30 minutes",
		"the session timeout was shortened to 22 minutes",
		"the session timeout is 10 minutes after the security review",
	)

	before := recallTexts(t, agent, "how long until a session times out?")
	stale := 0
	for _, txt := range before {
		if strings.Contains(txt, "30 minutes") || strings.Contains(txt, "22 minutes") {
			stale++
		}
	}
	if stale != 2 {
		t.Fatalf("precondition: want both stale values recallable, got %d\n%v", stale, before)
	}

	if err := runRevise(context.Background(), agent,
		"the session timeout is 30 minutes",
		"the session timeout is 10 minutes after the security review", ""); err != nil {
		t.Fatalf("revise 30 -> 10: %v", err)
	}
	if err := runRevise(context.Background(), agent,
		"the session timeout was shortened to 22 minutes",
		"the session timeout is 10 minutes after the security review", ""); err != nil {
		t.Fatalf("revise 22 -> 10: %v", err)
	}

	after := recallTexts(t, agent, "how long until a session times out?")
	for _, txt := range after {
		if strings.Contains(txt, "30 minutes") || strings.Contains(txt, "22 minutes") {
			t.Errorf("stale value still recallable after revise: %q", txt)
		}
	}
	found := false
	for _, txt := range after {
		if strings.Contains(txt, "10 minutes") {
			found = true
		}
	}
	if !found {
		t.Errorf("current value missing from recall after revise\n%v", after)
	}
}

// The tombstone points at the replacement, so the correction can be followed
// rather than only showing that something was retired (ADR-007).
func TestReviseTombstonePointsAtTheReplacement(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent, "the export cap is 10000 rows")

	if err := runRevise(context.Background(), agent,
		"the export cap is 10000 rows", "the export cap was raised to 50000 rows", ""); err != nil {
		t.Fatal(err)
	}

	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	all, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]memory.Fact, len(all))
	for _, f := range all {
		byID[f.ID] = f
	}
	var victim memory.Fact
	for _, f := range all {
		if strings.Contains(f.Text, "10000 rows") {
			victim = f
		}
	}
	if victim.ID == "" {
		t.Fatal("the retired fact was deleted; storage must stay append-only")
	}
	if !victim.IsSuperseded() {
		t.Fatal("the old fact was not tombstoned")
	}
	if victim.SupersededBy == memory.SupersededByAgent {
		t.Fatal("tombstone is generic; it should name the replacement")
	}
	replacement, ok := byID[victim.SupersededBy]
	if !ok {
		t.Fatalf("superseded_by %q resolves to no fact", victim.SupersededBy)
	}
	if !strings.Contains(replacement.Text, "50000 rows") {
		t.Errorf("tombstone points at %q, want the corrected fact", replacement.Text)
	}
}

// forget retires with no replacement and keeps the receipt.
func TestForgetRetiresWithoutReplacement(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent, "deploys are frozen on Fridays", "CI caches Go modules between runs")

	if err := runForget(agent, "deploys are frozen on Fridays", ""); err != nil {
		t.Fatal(err)
	}
	if got := len(liveFacts(t, agent)); got != 1 {
		t.Errorf("live facts = %d, want 1", got)
	}
	for _, txt := range recallTexts(t, agent, "can we deploy on a Friday?") {
		if strings.Contains(txt, "frozen on Fridays") {
			t.Errorf("forgotten fact still recallable: %q", txt)
		}
	}
}

// A unique substring is enough; an ambiguous one is an error that names the
// candidates instead of retiring an arbitrary fact.
func TestMatchingIsPermissiveButNeverAmbiguous(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent,
		"the primary database lives in eu-west-1",
		"the primary database runs PostgreSQL 16",
	)

	if err := runForget(agent, "PostgreSQL", ""); err != nil {
		t.Fatalf("unique substring should match: %v", err)
	}

	err := runForget(agent, "the primary database", "")
	if err == nil {
		t.Fatal("ambiguous substring must not retire a fact silently")
	}
	// One of the two is already retired, so "the primary database" now matches
	// a single live fact... but List returns tombstones too, so the guard has
	// to hold on the full set.
	if !strings.Contains(err.Error(), "matches") && !strings.Contains(err.Error(), "already superseded") {
		t.Errorf("unhelpful error for the ambiguous case: %v", err)
	}
}

// Reviving a superseded fact by revising it again would resurrect a retired
// belief; the command refuses instead.
func TestReviseRefusesAnAlreadySupersededFact(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent, "clients target API version v2")

	if err := runRevise(context.Background(), agent,
		"clients target API version v2", "all clients were moved to API version v3", ""); err != nil {
		t.Fatal(err)
	}
	err := runRevise(context.Background(), agent,
		"clients target API version v2", "clients target API version v4", "")
	if err == nil || !strings.Contains(err.Error(), "already superseded") {
		t.Errorf("revising a tombstone should be refused, got %v", err)
	}
}

// An empty replacement is a mistake, not a way to retire a fact — forget is.
func TestReviseRequiresAReplacement(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent, "the cache TTL is 60 seconds")
	if err := runRevise(context.Background(), agent, "the cache TTL is 60 seconds", "   ", ""); err == nil {
		t.Error("revise with a blank replacement must fail")
	}
	if got := len(liveFacts(t, agent)); got != 1 {
		t.Errorf("failed revise changed the store: live = %d, want 1", got)
	}
}

// Targeting by ID is the unambiguous path, needed when several facts share
// wording.
func TestReviseByID(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent, "the coverage gate is 60 percent", "the coverage gate rose to 75 percent")

	live := liveFacts(t, agent)
	var target memory.Fact
	for _, f := range live {
		if strings.Contains(f.Text, "60 percent") {
			target = f
		}
	}
	if target.ID == "" {
		t.Fatal("seed fact missing")
	}
	if err := runRevise(context.Background(), agent, "", "the coverage gate is 85 percent", target.ID); err != nil {
		t.Fatalf("revise by id: %v", err)
	}
	for _, f := range liveFacts(t, agent) {
		if strings.Contains(f.Text, "60 percent") {
			t.Error("the fact named by --id was not retired")
		}
	}
}

// The same sentence stored twice is one belief stored twice. Retiring only one
// copy leaves the other live, which is the exact failure revise exists to
// remove — so every exact-text match goes together.
func TestReviseRetiresEveryIdenticalCopy(t *testing.T) {
	const agent = "backend"
	const dup = "the session timeout was shortened to 22 minutes"
	seedRevise(t, agent, "the session timeout is 30 minutes", dup, dup)

	if err := runRevise(context.Background(), agent, dup, "the session timeout is now 10 minutes", ""); err != nil {
		t.Fatal(err)
	}
	for _, f := range liveFacts(t, agent) {
		if f.Text == dup {
			t.Errorf("an identical copy survived the revision: %s", f.ID)
		}
	}
	for _, txt := range recallTexts(t, agent, "how long until a session times out?") {
		if strings.Contains(txt, "22 minutes") {
			t.Errorf("stale value still recallable: %q", txt)
		}
	}
}

// forget applies the same rule.
func TestForgetRetiresEveryIdenticalCopy(t *testing.T) {
	const agent = "backend"
	const dup = "deploys are frozen on Fridays"
	seedRevise(t, agent, dup, dup, "CI caches Go modules between runs")

	if err := runForget(agent, dup, ""); err != nil {
		t.Fatal(err)
	}
	if got := len(liveFacts(t, agent)); got != 1 {
		t.Errorf("live facts = %d, want 1", got)
	}
}

// Distinct sentences sharing a substring are distinct beliefs; guessing is not
// allowed even now that identical copies are grouped.
func TestSubstringAcrossDifferentFactsStaysAnError(t *testing.T) {
	const agent = "backend"
	seedRevise(t, agent,
		"the primary database lives in eu-west-1",
		"the primary database runs PostgreSQL 16",
	)
	err := runForget(agent, "the primary database", "")
	if err == nil {
		t.Fatal("an ambiguous substring must not retire a fact")
	}
	if !strings.Contains(err.Error(), "different facts") {
		t.Errorf("error should name the ambiguity: %v", err)
	}
	if got := len(liveFacts(t, agent)); got != 2 {
		t.Errorf("the failed call changed the store: live = %d, want 2", got)
	}
}

// The CLI composes Remember + UpdateFact over the cliStore interface, because
// that path has to work through the daemon RPC as well as in-process, while
// pkg/memory.Store.Revise is the library home for the same semantics. Two
// implementations of one contract drift unless something holds them together:
// this asserts they leave the store in the same state.
func TestCLIReviseMatchesLibraryRevise(t *testing.T) {
	const agent = "backend"
	const oldText = "the export cap is 10000 rows"
	const newText = "the export cap was raised to 50000 rows"

	// Arm 1: the CLI command.
	seedRevise(t, agent, oldText, "CI caches Go modules between runs")
	if err := runRevise(context.Background(), agent, oldText, newText, ""); err != nil {
		t.Fatal(err)
	}
	viaCLI := factShape(t, agent)

	// Arm 2: the library call, on its own store.
	seedRevise(t, agent, oldText, "CI caches Go modules between runs")
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	var victim memory.Fact
	for _, f := range all {
		if f.Text == oldText {
			victim = f
		}
	}
	_ = store.Close()

	direct, err := memory.Open(memory.StoreConfig{
		DataDir:  dataDir,
		Embedder: embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Revise(context.Background(), agent, newText, victim); err != nil {
		t.Fatal(err)
	}
	_ = direct.Close()
	viaLib := factShape(t, agent)

	if len(viaCLI) != len(viaLib) {
		t.Fatalf("different fact counts: cli %d, library %d", len(viaCLI), len(viaLib))
	}
	for text, retired := range viaCLI {
		if viaLib[text] != retired {
			t.Errorf("%q: cli retired=%v, library retired=%v", text, retired, viaLib[text])
		}
	}
}

// factShape reduces the store to text -> "is it retired", which is the part of
// the state both paths must agree on. IDs and timestamps differ by
// construction and say nothing about the contract.
func factShape(t *testing.T, agent string) map[string]bool {
	t.Helper()
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	all, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bool, len(all))
	for _, f := range all {
		out[f.Text] = f.IsSuperseded()
	}
	return out
}
