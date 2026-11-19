package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestC110ReviewCLIReadOnlyAndExplicitActions(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "0")
	t.Setenv("GRAYMATTER_KG", "0")
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://review-cli-fixture")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	oldDir, oldNoDaemon, oldJSON, oldQuiet := dataDir, noDaemon, jsonOut, quiet
	dataDir, noDaemon, jsonOut, quiet = t.TempDir(), false, true, false
	t.Cleanup(func() { dataDir, noDaemon, jsonOut, quiet = oldDir, oldNoDaemon, oldJSON, oldQuiet })
	path := filepath.Join(dataDir, "gray.db")

	// Diagnostics on a missing store remain local and do not create leaves.
	runC0Command(t, reviewCmd(), "list", "project")
	runC0Command(t, statusCmd())
	if got := checkStoreReadOnly(dataDir); !strings.Contains(got.Detail, "no database yet") {
		t.Fatalf("doctor report for missing database = %+v", got)
	}
	if entries, err := os.ReadDir(dataDir); err != nil || len(entries) != 0 {
		t.Fatalf("read-only diagnostics created entries: %v, %v", entries, err)
	}

	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	ctx := context.Background()
	s, err := memory.Open(memory.StoreConfig{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.PutReturningFact(ctx, "project", "Backups retain data for 45 days.")
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	second, err := s.PutReturningFact(ctx, "project", "Backups retain data for 30 days.")
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	snapshots := []memory.EvidenceSnapshot{memory.NewEvidenceSnapshot(first), memory.NewEvidenceSnapshot(second)}
	records, err := s.ReviewCommit(ctx, memory.ReviewSubmission{
		Namespace: "project", Provider: "fixture", Model: "offline", PromptVersion: "fixture-v1",
		Snapshots: snapshots,
		Findings: []memory.ReviewFinding{
			{Kind: memory.ReviewKindSummary, Summary: "Unverified candidate.", SourceIDs: []string{first.ID}},
			{Kind: memory.ReviewKindConflict, SourceIDs: []string{first.ID, second.ID}, Note: "Different retention periods."},
		},
	})
	closeErr := s.Close()
	if err != nil || closeErr != nil || len(records) != 2 {
		t.Fatalf("seed review journal = %v, %v, %v", records, err, closeErr)
	}
	var summaryID, conflictID string
	for _, record := range records {
		switch record.Kind {
		case memory.ReviewKindSummary:
			summaryID = record.ID
		case memory.ReviewKindConflict:
			conflictID = record.ID
		}
	}
	if summaryID == "" || conflictID == "" {
		t.Fatalf("missing summary or conflict: %v", records)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	out := runC0Command(t, reviewCmd(), "list", "project")
	var rows []reviewRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 2 ||
		strings.Contains(out, "Different retention periods.") || strings.Contains(out, first.Text) {
		t.Fatalf("lightweight list = %s, %v", out, err)
	}
	out = runC0Command(t, reviewCmd(), "inspect", "project", conflictID)
	var inspected memory.ReviewRecord
	if err := json.Unmarshal([]byte(out), &inspected); err != nil || inspected.Resolution != "unresolved" ||
		inspected.SourceState != "current" || len(inspected.Sources) != 2 {
		t.Fatalf("inspect = %s, %v", out, err)
	}
	out = runC0Command(t, statusCmd())
	var status struct {
		Store struct {
			Review *memory.ReviewOverview `json:"review"`
		} `json:"store"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil || status.Store.Review == nil ||
		status.Store.Review.Pending != 1 || status.Store.Review.Unresolved != 1 {
		t.Fatalf("status = %s, %v", out, err)
	}
	wantOverview := fmt.Sprintf("1 pending · 0 stale · 0 rejected · 1 unresolved · 0 resolved · 0 dismissed · %d logical bytes", status.Store.Review.LogicalBytes)
	jsonOut = false
	statusText := runStatusCmd(t)
	jsonOut = true
	if !slices.Contains(strings.Split(statusText, "\n"), "REVIEW     "+wantOverview) {
		t.Fatalf("status review line = %q, want %q", statusText, wantOverview)
	}
	check := checkStoreReadOnly(dataDir)
	if !strings.HasSuffix(check.Detail, " · review journal "+wantOverview) {
		t.Fatalf("doctor review line = %q, want %q", check.Detail, wantOverview)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("diagnostics changed database bytes: %v", err)
	}

	runC0Command(t, reviewCmd(), "reject", "project", summaryID, "--decision", "The summary lacks context.")
	runC0Command(t, reviewCmd(), "resolve", "project", conflictID, "--decision", "The runbook specifies 45 days.",
		"--reference", first.ID, "--reference", "runbook/retention")
	reopened, err := openDiagnosticMemory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rejected, err := reopened.ReviewInspect(ctx, "project", summaryID)
	if err != nil || rejected.Status != "rejected" {
		t.Fatalf("reopened rejected record = %+v, %v", rejected, err)
	}
	resolved, err := reopened.ReviewInspect(ctx, "project", conflictID)
	if err != nil || resolved.Resolution != "resolved" || resolved.Decision != "The runbook specifies 45 days." ||
		len(resolved.References) != 2 || resolved.SourceState != "current" {
		t.Fatalf("reopened resolved record = %+v, %v", resolved, err)
	}
	facts, err := reopened.List("project")
	if err != nil || len(facts) != 2 {
		t.Fatalf("review changed active facts: %v, %v", facts, err)
	}
	for _, f := range facts {
		if f.IsSuperseded() || (f.ID != first.ID && f.ID != second.ID) {
			t.Fatalf("review changed active fact %+v", f)
		}
	}
}

func TestC110ReviewCLIThroughDaemonUsesOwnerProvider(t *testing.T) {
	const namespace = "project"
	ctx := context.Background()
	oldDir, oldNoDaemon, oldJSON, oldQuiet := dataDir, noDaemon, jsonOut, quiet
	dataDir, noDaemon, jsonOut, quiet = t.TempDir(), false, true, false
	t.Cleanup(func() { dataDir, noDaemon, jsonOut, quiet = oldDir, oldNoDaemon, oldJSON, oldQuiet })
	t.Setenv("GRAYMATTER_NO_DAEMON", "0")
	t.Setenv("GRAYMATTER_KG", "0")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	store, err := memory.Open(memory.StoreConfig{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	var factIDs []string
	for _, days := range []int{45, 30, 60, 90} {
		fact, err := store.PutReturningFact(ctx, namespace, fmt.Sprintf("Backups retain data for %d days.", days))
		if err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
		factIDs = append(factIDs, fact.ID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	const privateNote = "The evidence records different retention periods."
	var conflicts []map[string]any
	for i, first := range factIDs {
		for _, second := range factIDs[i+1:] {
			conflicts = append(conflicts, map[string]any{"ids": []string{first, second}, "note": privateNote})
		}
	}
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		providerCalls.Add(1)
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "owner-fixture-model" {
			http.Error(w, "unexpected review model", http.StatusBadRequest)
			return
		}
		findings, _ := json.Marshal(map[string]any{"conflicts": conflicts})
		_ = json.NewEncoder(w).Encode(map[string]any{"response": string(findings), "done": true})
	}))
	t.Cleanup(provider.Close)
	t.Setenv("GRAYMATTER_OLLAMA_URL", provider.URL)
	t.Setenv("GRAYMATTER_OLLAMA_CONSOLIDATE_MODEL", "owner-fixture-model")

	ready := make(chan struct{})
	done := make(chan error, 1)
	dir := dataDir
	go func() {
		done <- daemon.Run(daemon.RunOptions{
			DataDir: dir,
			Logf: func(format string, _ ...any) {
				if strings.HasPrefix(format, "graymatter daemon ready:") {
					close(ready)
				}
			},
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before readiness: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not become ready")
	}

	client, err := daemon.ConnectNoSpawn(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Shutdown()
		_ = client.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	if !client.ReviewJournalSupported() {
		t.Fatal("daemon omitted review capability")
	}
	// Analyze must use the already running owner's endpoint and model.
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://wrong-client-endpoint")
	t.Setenv("GRAYMATTER_OLLAMA_CONSOLIDATE_MODEL", "wrong-client-model")
	out := runC0Command(t, reviewCmd(), "analyze", namespace, "--provider", "ollama")
	var rows []reviewRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != len(conflicts) || rows[0].Resolution != "unresolved" {
		t.Fatalf("analysis result = %s, %v", out, err)
	}
	for _, args := range [][]string{
		{namespace, "backups retention"},
		{namespace, "--query", "backups retention", "--query", "data days"},
	} {
		out := captureStdout(t, func() { runC0Command(t, recallCmd(), args...) })
		var recalled struct {
			Notices []memory.ReviewNotice `json:"conflict_notices"`
		}
		if err := json.Unmarshal([]byte(out), &recalled); err != nil || len(recalled.Notices) != 4 || strings.Contains(out, privateNote) {
			t.Fatalf("default recall notices for %v = %s, %v", args, out, err)
		}
		seen := make(map[string]bool)
		for _, notice := range recalled.Notices {
			if seen[notice.ID] || notice.SourceState != "current" || len(notice.SourceIDs) != 2 ||
				!slices.Contains(factIDs, notice.SourceIDs[0]) || !slices.Contains(factIDs, notice.SourceIDs[1]) {
				t.Fatalf("unexpected conflict notice %+v", notice)
			}
			seen[notice.ID] = true
		}
	}
	runC0Command(t, reviewCmd(), "list", namespace)
	runC0Command(t, reviewCmd(), "inspect", namespace, rows[0].ID)
	runC0Command(t, statusCmd())
	report := checkStoreReadOnly(dir)
	if !strings.Contains(report.Detail, "6 unresolved") || providerCalls.Load() != 1 {
		t.Fatalf("read-only diagnostics or provider calls: %+v, %d", report, providerCalls.Load())
	}
}

func TestC110ReviewStatusPreservesReadOnlyDatabaseSymlink(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	oldDir, oldJSON := dataDir, jsonOut
	dataDir, jsonOut = t.TempDir(), true
	t.Cleanup(func() { dataDir, jsonOut = oldDir, oldJSON })
	target := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: target})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutReturningFact(context.Background(), "project", "Read-only status control."); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target, "gray.db")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dataDir, "gray.db")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symlink requires host permission: %v", err)
		}
		t.Fatal(err)
	}
	out := runC0Command(t, statusCmd())
	var status struct {
		Store struct {
			TotalAgents int `json:"total_agents"`
		} `json:"store"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil || status.Store.TotalAgents != 1 {
		t.Fatalf("status for linked database = %s, %v", out, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("linked database bytes changed: %v", err)
	}
}
