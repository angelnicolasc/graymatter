package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The demo must be exactly what it claims: a local, deterministic, re-runnable
// seeding of a multi-agent corpus that never duplicates on re-run and never
// touches anything that is not a GrayMatter demo dir.

func newDemoStore(t *testing.T) (cliStore, string) {
	t.Helper()
	// Direct store, like the hooks tests: the demo machinery is exercised
	// without a daemon in the loop.
	oldData, oldNoDaemon := dataDir, noDaemon
	dataDir = t.TempDir()
	noDaemon = true
	t.Cleanup(func() { dataDir, noDaemon = oldData, oldNoDaemon })

	dir := dataDir
	s, err := openStoreIn(dir)
	if err != nil {
		t.Fatalf("open demo store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestDemoSeed_PlantsCorpusAndIsIdempotent(t *testing.T) {
	s, _ := newDemoStore(t)

	planted, err := seedDemoCorpus(s)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	want := 0
	for _, a := range demoAgents() {
		want += len(a.facts)
	}
	if planted != want {
		t.Fatalf("first seed planted %d facts, want %d", planted, want)
	}

	// Re-run: append-only store, so a duplicate plant would be visible garbage.
	again, err := seedDemoCorpus(s)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if again != 0 {
		t.Errorf("second seed planted %d facts, want 0 (no duplicates)", again)
	}

	agents, err := s.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, a := range agents {
		found[a] = true
	}
	for _, a := range demoAgents() {
		if !found[a.id] {
			t.Errorf("agent %q missing after seeding", a.id)
		}
	}
}

func TestDemoSeed_RecallFindsDemoFacts(t *testing.T) {
	s, _ := newDemoStore(t)
	if _, err := seedDemoCorpus(s); err != nil {
		t.Fatal(err)
	}
	// The sales corpus must be retrievable — this is the story the demo tells.
	facts, err := s.Recall(context.Background(), "sales-closer", "Maria Acme renewal", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) == 0 {
		t.Fatal("demo corpus is not retrievable; the demo would show an empty store")
	}
	if !strings.Contains(facts[0], "Maria") && !strings.Contains(facts[0], "Acme") {
		t.Errorf("top recall = %q, want a Maria/Acme fact", facts[0])
	}
}

func TestDemoRemoveDir_RefusesNonDemoContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thesis.pdf"), []byte("years of work"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := removeDemoDir(dir)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("removeDemoDir must refuse non-demo content, got %v", err)
	}
	// The refused file must still be there.
	if _, statErr := os.Stat(filepath.Join(dir, "thesis.pdf")); statErr != nil {
		t.Error("refused directory must be left untouched")
	}
}

func TestDemoRemoveDir_AcceptsDemoContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("# GrayMatter Memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gray.db"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeDemoDir(dir); err != nil {
		t.Fatalf("removeDemoDir on a demo dir: %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("demo dir should be gone after --fresh")
	}
}

func TestDemoAgents_CorpusShape(t *testing.T) {
	agents := demoAgents()
	if len(agents) != 4 {
		t.Fatalf("demo agents = %d, want 4 (three agents + shared)", len(agents))
	}
	ids := map[string]bool{}
	for _, a := range agents {
		if a.id == "" {
			t.Error("demo agent with empty id")
		}
		if len(a.facts) == 0 {
			t.Errorf("agent %q has no facts; the demo would be empty", a.id)
		}
		ids[a.id] = true
	}
	if !ids["sales-closer"] || !ids["support-lead"] || !ids["infra-bot"] || !ids["__shared__"] {
		t.Errorf("demo agents missing the documented cast: %v", ids)
	}
}
