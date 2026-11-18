package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/charmbracelet/x/ansi"
)

const (
	a1Subject = "deployment window"
	a1Retired = "Deployment window was Tuesday"
	a1Current = "Deployment window is Thursday"
)

// Recreate a retirement without a replacement edge, including the retrieval
// index update. The native matrix separately exercises the published writer.
func seedUnresolvedSubjectSurface(t *testing.T) string {
	t.Helper()
	store, err := openDirectStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	subject := a1Subject
	rows, err := store.PutManyReturningReceipts(context.Background(), "project", []memory.WriteFact{
		{Text: a1Retired, Options: memory.WriteOptions{Subject: &subject}},
		{Text: a1Current},
	})
	if err != nil || len(rows) != 2 {
		t.Fatalf("seed store: %+v %v", rows, err)
	}
	retired := rows[0].Fact
	retired.SupersededBy = rows[1].Fact.ID
	if err := store.UpdateFact("project", retired); err != nil {
		t.Fatal(err)
	}
	return rows[1].Fact.ID
}

func a1SurfaceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldQuiet := dataDir, quiet
	dataDir, quiet = dir, true
	t.Cleanup(func() { dataDir, quiet = oldDir, oldQuiet })
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://subject-surface")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	return dir
}

func assertA1SubjectWarning(t *testing.T, text string) {
	t.Helper()
	if strings.Count(text, "Unresolved subjects:") != 1 ||
		!strings.Contains(text, a1Subject) || !strings.Contains(text, "(1 total)") {
		t.Fatalf("expected a single bounded subject warning: %q", text)
	}
}

func TestA108SubjectIssuesHooks(t *testing.T) {
	a1SurfaceDir(t)
	seedUnresolvedSubjectSurface(t)
	store, err := openDirectStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	native, err := hookRecallBlock(store, "project", "deployment", 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	lexical, _, err := hookRecallLexicalBlock(context.Background(), store, "project", "deployment")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{native, lexical} {
		assertA1SubjectWarning(t, text)
		if !strings.Contains(text, a1Current) || strings.Contains(text, a1Retired) || strings.Contains(text, "[subjects=") {
			t.Fatalf("unexpected hook facts or subject projection: %q", text)
		}
	}
}

func TestA108SubjectIssuesExport(t *testing.T) {
	a1SurfaceDir(t)
	id := seedUnresolvedSubjectSurface(t)
	out := t.TempDir()
	cmd := exportCmd()
	var warning bytes.Buffer
	cmd.SetErr(&warning)
	cmd.SetArgs([]string{"--format", "json", "--agent", "project", "--out", out})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	assertA1SubjectWarning(t, warning.String())
	raw, err := os.ReadFile(filepath.Join(out, "memories.json"))
	if err != nil {
		t.Fatal(err)
	}
	var facts []memory.Fact
	if err := json.Unmarshal(raw, &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].ID != id || facts[0].Text != a1Current || len(facts[0].Subjects) != 0 {
		t.Fatalf("unexpected export facts: %s", raw)
	}
}

func TestPU07SubjectLineageExportThroughDaemon(t *testing.T) {
	dir := a1SurfaceDir(t)
	oldNoDaemon := noDaemon
	noDaemon = false
	t.Cleanup(func() { noDaemon = oldNoDaemon })
	seedUnresolvedSubjectSurface(t)

	const namespace = "project"
	ctx := t.Context()
	store, err := openDirectStore()
	if err != nil {
		t.Fatal(err)
	}
	liveSubject, prunedSubject := "current backup policy", "obsolete archive policy"
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	source, quote := "policy.log", "Keep a verified backup"
	receipts, err := store.PutManyReturningReceipts(ctx, namespace, []memory.WriteFact{
		{Text: "Back up daily", Options: memory.WriteOptions{Subject: &liveSubject, Source: &source, Quote: &quote}},
		{Text: "Use the obsolete archive", Options: memory.WriteOptions{Subject: &prunedSubject}},
	})
	if err != nil || len(receipts) != 2 {
		t.Fatalf("seed export subjects: %v, %v", receipts, err)
	}
	live, pruned := receipts[0].Fact, receipts[1].Fact
	pruned.AccessedAt = time.Now().UTC().Add(-201 * 24 * time.Hour)
	if err := store.UpdateFact(namespace, pruned); err != nil {
		t.Fatal(err)
	}
	if err := store.Consolidate(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	closeErr := store.Close()
	store = nil
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	runExport := func() ([]byte, string) {
		t.Helper()
		out := t.TempDir()
		cmd := exportCmd()
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"--format", "json", "--include-lineage", "--agent", namespace, "--out", out})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(out, "memory-lineage.json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw, stderr.String()
	}
	direct, directWarning := runExport()
	assertA1SubjectWarning(t, directWarning)
	var archive memory.LineageArchive
	if err := json.Unmarshal(direct, &archive); err != nil {
		t.Fatal(err)
	}
	if archive.Namespace != namespace || len(archive.SubjectHeads) != 3 || archive.SubjectIssues == nil ||
		archive.SubjectIssues.Total != 1 || len(archive.SubjectIssues.Subjects) != 1 {
		t.Fatalf("incomplete fixture archive: %s", direct)
	}
	heads := make(map[string]memory.SubjectHeadInspection)
	for _, head := range archive.SubjectHeads {
		heads[head.Name] = head
	}
	if head := heads[liveSubject]; head.State != "active" || head.Reference.FactID != live.ID {
		t.Fatalf("live subject missing from archive: %+v", head)
	}
	if head := heads[prunedSubject]; head.State != "closed" || head.Reference.FactID != pruned.ID {
		t.Fatalf("closed subject missing after prune: %+v", head)
	}
	issue := archive.SubjectIssues.Subjects[0]
	if issue.Namespace != namespace || issue.Name != a1Subject || issue.HeadFactID != heads[a1Subject].Reference.FactID {
		t.Fatalf("unresolved diagnostic disagrees with recorded head: %+v", issue)
	}
	for _, fact := range archive.Facts {
		if fact.ID == pruned.ID {
			t.Fatalf("fixture retained the pruned canonical row: %+v", fact)
		}
	}
	var citedLive, retainedPruned bool
	for _, row := range archive.Evidence {
		if row.Snapshot == nil {
			continue
		}
		switch row.Snapshot.FactID {
		case live.ID:
			citedLive = row.Evidence != nil && row.Evidence.Source == source && row.Evidence.Quote == quote
		case pruned.ID:
			retainedPruned = row.Snapshot.Text == pruned.Text && row.Snapshot.ContentHash == heads[prunedSubject].Reference.ContentHash
		}
	}
	if !citedLive || !retainedPruned {
		t.Fatalf("fixture did not retain exact archive evidence: %s", direct)
	}

	t.Setenv("GRAYMATTER_NO_DAEMON", "0")
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(daemon.RunOptions{
			DataDir:  dir,
			IdleExit: 20 * time.Second,
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
	viaDaemon, daemonWarning := runExport()
	if !bytes.Equal(direct, viaDaemon) || directWarning != daemonWarning {
		t.Fatalf("daemon export or warning differs from direct: archive=%s, warning=%q, expected warning=%q",
			viaDaemon, daemonWarning, directWarning)
	}
}

func TestA108SubjectIssuesTUI(t *testing.T) {
	a1SurfaceDir(t)
	id := seedUnresolvedSubjectSurface(t)
	store, err := openTUIStore(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	model := workbenchModel(t, store)
	model.width = 180
	next, _ := model.Update(model.loadFacts("project")())
	got := next.(tuiModel)
	facts := got.factList.Items()
	if len(facts) != 1 || facts[0].(factItem).fact.ID != id || len(facts[0].(factItem).fact.Subjects) != 0 {
		t.Fatalf("unexpected TUI facts: %+v", facts)
	}
	assertA1SubjectWarning(t, ansi.Strip(got.View().Content))
}

func TestA103SubjectIssuesDoctorIsReadOnly(t *testing.T) {
	dir := a1SurfaceDir(t)
	seedUnresolvedSubjectSurface(t)
	before := snapshotDoctorTree(t, dir)
	check := checkStore(dir)
	if check.Status != "warn" || !strings.Contains(check.Detail, `"project" (1)`) ||
		!strings.Contains(check.Hint, "replaces") {
		t.Fatalf("unexpected doctor observation: %+v", check)
	}
	assertA1SubjectWarning(t, check.Detail)
	assertDoctorTreeUnchanged(t, dir, before)
}
