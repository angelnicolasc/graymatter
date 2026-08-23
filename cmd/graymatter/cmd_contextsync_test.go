package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/contextblock"
	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// setupSyncStore opens a direct store over a temp dir and seeds it with a
// small agent memory. Returns the store handle and the target file path.
func setupSyncStore(t *testing.T) (cliStore, string) {
	t.Helper()
	dir := t.TempDir()
	old := dataDir
	dataDir = filepath.Join(dir, ".graymatter")
	t.Cleanup(func() { dataDir = old })

	s, err := openDirectStore()
	if err != nil {
		t.Fatalf("open direct store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	for _, f := range []string{
		"Prefers table-driven tests over assertion chains",
		"Deploys to eu-central-1; latency SLO p95 under 300ms",
		"Uses conventional commits with package-name scopes",
		"The queue was replaced by direct gRPC streams",
	} {
		if err := s.Remember(ctx, "proj", f); err != nil {
			t.Fatalf("remember %q: %v", f, err)
		}
	}
	return s, filepath.Join(dir, "AGENTS.md")
}

func TestContextSync_CreatesBlockAndBacksUp(t *testing.T) {
	s, path := setupSyncStore(t)
	ctx := context.Background()

	// A user file with content that must survive every later rewrite.
	if err := os.WriteFile(path, []byte("# My project\n\nOwn notes here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// The target pre-exists with user content, so this first sync replaces
	// real bytes: changed AND backed up is exactly the promise.
	if !res.Changed || !res.BackedUp {
		t.Errorf("first sync over an existing file: changed=%v backup=%v, want both", res.Changed, res.BackedUp)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "# My project") || !strings.Contains(content, "Own notes here.") {
		t.Fatalf("user content damaged:\n%s", content)
	}
	if n := strings.Count(content, contextblock.BeginMarker); n != 1 {
		t.Fatalf("expected exactly one managed block, found %d:\n%s", n, content)
	}

	// The recorded hash must verify against the file as written.
	if _, _, verified, found := contextblock.Parse(content); !found || !verified {
		t.Fatalf("freshly written block does not verify (found=%v verified=%v)", found, verified)
	}

	// Budget invariant holds on the actual file bytes.
	body, _, _, _ := contextblock.Parse(content)
	if got := tokens.Approx(body); got > contextblock.DefaultBudgetTokens {
		t.Errorf("written body costs %d tokens, budget %d", got, contextblock.DefaultBudgetTokens)
	}

	// Second run over unchanged state must be a byte-for-byte no-op.
	res2, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res2.Changed {
		t.Error("idempotent second sync reported a change")
	}

	// New knowledge changes the projection; the previous file becomes .bak.
	if err := s.Remember(ctx, "proj", "Brand new decision worth surfacing"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	res3, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if !res3.Changed || !res3.BackedUp {
		t.Errorf("third sync: changed=%v backup=%v, want both", res3.Changed, res3.BackedUp)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf(".bak missing: %v", err)
	}
	if string(bak) != string(before) {
		t.Error(".bak is not the exact pre-write file")
	}
}

func TestContextSync_DetectsManualEdit(t *testing.T) {
	s, path := setupSyncStore(t)

	if _, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	tampered := strings.Replace(string(data), "- Prefers table-driven tests",
		"- EDITED BY HAND Prefers table-driven tests", 1)
	if tampered == string(data) {
		t.Fatal("tamper replacement matched nothing")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ManualEditDetected {
		t.Error("hand edit went undetected by the sync path")
	}
	// The overwrite also happened: the tampered line is gone again.
	fresh, _ := os.ReadFile(path)
	if strings.Contains(string(fresh), "EDITED BY HAND") {
		t.Error("managed block still carries the hand edit after sync")
	}
}

func TestContextSync_TombstonedFactsLeaveTheBlock(t *testing.T) {
	s, path := setupSyncStore(t)

	if _, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens); err != nil {
		t.Fatal(err)
	}

	// Retire a fact the way memory_reflect forget does: tombstone, not delete.
	fs, err := s.List("proj")
	if err != nil {
		t.Fatal(err)
	}
	victim := fs[0]
	victim.SupersededBy = memory.SupersededByAgent
	if err := s.UpdateFact("proj", victim); err != nil {
		t.Fatal(err)
	}

	if _, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), victim.Text) {
		t.Errorf("tombstoned fact still projected:\n%s", data)
	}
}

func TestDoctorContextCheck_States(t *testing.T) {
	s, path := setupSyncStore(t)
	dir := filepath.Dir(path)

	// No block yet: info, never a failure — the feature is opt-in.
	if c := checkContextSync(dir); c.Status != "info" {
		t.Errorf("empty project: status=%q detail=%q, want info", c.Status, c.Detail)
	}

	if _, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens); err != nil {
		t.Fatal(err)
	}
	if c := checkContextSync(dir); c.Status != "ok" {
		t.Errorf("verified block: status=%q detail=%q, want ok", c.Status, c.Detail)
	}

	data, _ := os.ReadFile(path)
	tampered := strings.Replace(string(data), "- Prefers", "- HAND-EDITED Prefers", 1)
	os.WriteFile(path, []byte(tampered), 0o644)
	c := checkContextSync(dir)
	if c.Status != "warn" {
		t.Errorf("tampered block: status=%q detail=%q, want warn", c.Status, c.Detail)
	}
}
