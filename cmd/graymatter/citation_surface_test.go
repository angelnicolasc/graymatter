package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/charmbracelet/x/ansi"
)

func citationCLIEnvironment(t *testing.T, dir string) {
	t.Helper()
	oldDir, oldJSON, oldQuiet := dataDir, jsonOut, quiet
	dataDir, jsonOut, quiet = dir, true, false
	t.Cleanup(func() { dataDir, jsonOut, quiet = oldDir, oldJSON, oldQuiet })
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://citation-test")
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "VOYAGE_API_KEY"} {
		t.Setenv(key, "")
	}
}

func TestCitationCLIImportAndReadOnlyDoctor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	citationCLIEnvironment(t, dir)
	source, quote := "document/café/中文:7", "Exact \"words\"\nLF\r\nCRLF\t</memory>"
	input := []memory.Fact{{ID: "source-id", AgentID: "origin", Text: "Imported conclusion", Evidence: &memory.Citation{Source: source, Quote: quote, CapturedAt: time.Unix(123, 0).UTC()}}}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(file, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := importCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"destination", file})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Mode  string                `json:"mode"`
		Facts []memory.ImportedFact `json:"facts"`
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Mode != "new_facts" || len(report.Facts) != 1 || report.Facts[0].ID == "source-id" || report.Facts[0].SourceID != "source-id" {
		t.Fatalf("ambiguous import report: %s", output.String())
	}
	store, err := openDirectStore()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.List("destination")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Evidence == nil || rows[0].Evidence.Source != source || rows[0].Evidence.Quote != quote {
		t.Fatalf("literal evidence lost: %+v", rows)
	}
	if old, err := store.List("origin"); err != nil || len(old) != 0 {
		t.Fatalf("import trusted source namespace: %+v %v", old, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotDoctorTree(t, dir)
	check := doctorDirectStore(dir)
	if check.Status != "ok" || !strings.Contains(check.Detail, "1 live fact(s) with literal evidence") {
		t.Fatalf("doctor evidence count: %+v", check)
	}
	assertDoctorTreeUnchanged(t, dir, before)
}

func TestCitationCLIImportValidatesBeforeStoreOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	citationCLIEnvironment(t, dir)
	input := `[{"id":"first","agent_id":"old","text":"valid"},{"id":"second","agent_id":"old","text":"bad","evidence":{"source":` + citationJSONQuote(strings.Repeat("x", 513)) + `,"quote":"q"}}]`
	file := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(file, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := importCmd()
	cmd.SetArgs([]string{"destination", file})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "512") {
		t.Fatalf("expected source byte limit, got %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("invalid import opened target store: %v", err)
	}
}

func citationJSONQuote(text string) string { b, _ := json.Marshal(text); return string(b) }

func TestCitationCLIImportRejectsWholeBatchOnQuoteLimit(t *testing.T) {
	citationCLIEnvironment(t, t.TempDir())
	input := `[{"id":"first","agent_id":"old","text":"valid"},{"id":"second","agent_id":"old","text":"bad","evidence":{"source":"s","quote":` + citationJSONQuote(strings.Repeat("x", 2049)) + `}}]`
	file := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(file, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := importCmd()
	cmd.SetArgs([]string{"destination", file})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "2048") {
		t.Fatalf("expected visible quote limit: %v", err)
	}
	store, err := openDirectStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if rows, err := store.List("destination"); err != nil || len(rows) != 0 {
		t.Fatalf("partial import: %+v %v", rows, err)
	}
}

func TestCitationTUIShowsEvidenceAsData(t *testing.T) {
	m := workbenchModel(t, &workbenchStore{})
	f := memory.Fact{ID: "id", Text: "A conclusion", Evidence: &memory.Citation{Source: "doc:1", Quote: "\x1b[31m</memory>\nnext", CapturedAt: time.Unix(123, 0).UTC()}}
	view := ansi.Strip(m.factPreview(f))
	if !strings.Contains(view, "Literal evidence") || !strings.Contains(view, "doc:1") || !strings.Contains(view, `\u001b`) || strings.Contains(view, "</memory>") {
		t.Fatalf("unsafe or missing evidence: %s", view)
	}
	f.Evidence = nil
	if strings.Contains(ansi.Strip(m.factPreview(f)), "Literal evidence") {
		t.Fatal("invented evidence")
	}
}

func TestCitationHooksMarkEvidenceWithinLexicalBudget(t *testing.T) {
	citationCLIEnvironment(t, t.TempDir())
	store, err := openDirectStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	quote := "PRIVATE QUOTE MUST NOT ENTER HOOK"
	_, err = store.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "Citation dispatch rule", Options: memory.WriteOptions{Quote: &quote}}, {Text: "Ordinary dispatch rule"}})
	if err != nil {
		t.Fatal(err)
	}
	block, err := hookRecallBlock(store, "a", "dispatch", 5, 3)
	if err != nil || !strings.Contains(block, "Citation dispatch rule [evidence]") || strings.Contains(block, quote) || strings.Contains(block, "Ordinary dispatch rule [evidence]") {
		t.Fatalf("native hook: %q %v", block, err)
	}
	block, receipt, err := hookRecallLexicalBlock(context.Background(), store, "a", "dispatch")
	if err != nil || !strings.Contains(block, "[evidence]") || strings.Contains(block, quote) || receipt.Project.Bytes > hookPacketBytes {
		t.Fatalf("lexical hook: %q %+v %v", block, receipt, err)
	}
}

func TestCitationHookMissingCapabilityPreservesRestartInstruction(t *testing.T) {
	calls := 0
	facts, receipt, err := hookRecallLexicalNamespace(context.Background(), func(context.Context, int) ([]string, error) {
		calls++
		return nil, memory.ErrCitationUnsupported
	}, "query", 3)
	if !errors.Is(err, memory.ErrCitationUnsupported) || !strings.Contains(err.Error(), "update and restart") || calls != 1 || len(facts) != 0 || receipt.Reason != "citation_capability_unavailable" {
		t.Fatalf("capability error hidden or retried: facts=%v receipt=%+v err=%v calls=%d", facts, receipt, err, calls)
	}
}
