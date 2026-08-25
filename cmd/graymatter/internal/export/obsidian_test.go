package export

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func obsidianTestFacts() []memory.Fact {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	return []memory.Fact{
		{ID: "01A", AgentID: "sales-closer", Text: "Maria prefers Slack over email", CreatedAt: base, AccessedAt: base, Weight: 1},
		{ID: "01B", AgentID: "sales-closer", Text: "Maria prefers Slack over email", CreatedAt: base, AccessedAt: base, Weight: 1},
		{ID: "01C", AgentID: "backend", Text: "API rate limit is 100 req/min", CreatedAt: base, AccessedAt: base, Weight: 1},
		{ID: "01D", AgentID: "backend", Text: "   ", CreatedAt: base, AccessedAt: base, Weight: 1},
		{ID: "01E", AgentID: "docs", Text: "El corazón de la configuración es la calibración de pesos y señales de recall para cada agente del sistema", CreatedAt: base, AccessedAt: base, Weight: 1},
	}
}

func exportObsidian(t *testing.T, facts []memory.Fact) string {
	t.Helper()
	out := t.TempDir()
	if err := (&ObsidianExporter{}).Export(facts, out); err != nil {
		t.Fatalf("export: %v", err)
	}
	return out
}

// Fact notes must be named after the fact text, not the raw ULID: Obsidian's
// graph view and quick switcher render the filename, and a wall of ULIDs is
// unreadable.
func TestObsidian_FactNotesUseReadableNames(t *testing.T) {
	out := exportObsidian(t, obsidianTestFacts())

	if _, err := os.Stat(filepath.Join(out, "sales-closer", "Maria-prefers-Slack-over-email.md")); err != nil {
		t.Errorf("readable fact note missing: %v", err)
	}
	// The duplicate text is disambiguated with a short-ID suffix, not overwritten.
	if _, err := os.Stat(filepath.Join(out, "sales-closer", "Maria-prefers-Slack-over-email-01b.md")); err != nil {
		t.Errorf("disambiguated duplicate note missing: %v", err)
	}
	// A blank slug falls back to "fact" plus suffix.
	if _, err := os.Stat(filepath.Join(out, "backend", "fact-01d.md")); err != nil {
		t.Errorf("blank-slug note missing: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(out, "sales-closer"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 notes in sales-closer, got %d", len(entries))
	}
}

// Every wikilink the index emits must resolve to a file on disk — this is the
// property Obsidian needs to draw the fact layer of the graph at all.
func TestObsidian_IndexLinksResolve(t *testing.T) {
	out := exportObsidian(t, obsidianTestFacts())

	data, err := os.ReadFile(filepath.Join(out, "_index.md"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\[\[([^\]|]+)\|`)
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		target := filepath.Join(out, filepath.FromSlash(m[1])+".md")
		if _, err := os.Stat(target); err != nil {
			t.Errorf("index link %q does not resolve: %v", m[1], err)
		}
	}
	if !strings.Contains(string(data), "sales-closer/Maria-prefers-Slack-over-email") {
		t.Errorf("index does not use readable note names:\n%s", data)
	}
}

// Two exports of the same store must produce byte-identical indexes; map
// iteration order used to scramble the agent sections between runs.
func TestObsidian_IndexDeterministic(t *testing.T) {
	facts := obsidianTestFacts()
	out1 := exportObsidian(t, facts)
	out2 := exportObsidian(t, facts)

	b1, err := os.ReadFile(filepath.Join(out1, "_index.md"))
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(filepath.Join(out2, "_index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("index is not deterministic between runs:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", b1, b2)
	}
}

// Previews truncate by rune, not by byte: multi-byte text must never split
// mid-character and emit invalid UTF-8 into the vault.
func TestObsidian_PreviewIsRuneSafe(t *testing.T) {
	out := exportObsidian(t, obsidianTestFacts())

	data, err := os.ReadFile(filepath.Join(out, "_index.md"))
	if err != nil {
		t.Fatal(err)
	}
	index := string(data)
	if !utf8.ValidString(index) {
		t.Error("index is not valid UTF-8")
	}
	if strings.Contains(index, "\uFFFD") {
		t.Error("index contains replacement characters from a split rune")
	}
	if !strings.Contains(index, "El corazón de la configuración") {
		t.Errorf("multi-byte preview garbled:\n%s", index)
	}
}

func TestBuildFactNoteNames_CollisionFreeAndDeterministic(t *testing.T) {
	facts := []memory.Fact{
		{ID: "aaaaaaaa-1", Text: "Same text"},
		{ID: "bbbbbbbb-2", Text: "Same text"},
		{ID: "cccccccc-3", Text: "Same text"},
		{ID: "dddddddd-4", Text: "   "},
	}
	names := BuildFactNoteNames(facts)

	seen := map[string]bool{}
	for _, f := range facts {
		n := names[f.ID]
		if n == "" {
			t.Fatalf("empty name for fact %s", f.ID)
		}
		if seen[n] {
			t.Errorf("filename collision on %q", n)
		}
		seen[n] = true
	}
	if names["aaaaaaaa-1"] != "Same-text" {
		t.Errorf("first occurrence should keep the clean name, got %q", names["aaaaaaaa-1"])
	}

	again := BuildFactNoteNames(facts)
	for _, f := range facts {
		if again[f.ID] != names[f.ID] {
			t.Errorf("naming is not deterministic for %s: %q vs %q", f.ID, names[f.ID], again[f.ID])
		}
	}
}

// Pinned facts carry the flag into the vault frontmatter (ADR-010).
func TestObsidian_PinnedFrontmatter(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	facts := []memory.Fact{
		{ID: "01P", AgentID: "arch", Text: "Single-writer write path.", CreatedAt: base, AccessedAt: base, Weight: 1, Pinned: true},
	}
	out := exportObsidian(t, facts)
	data, err := os.ReadFile(filepath.Join(out, "arch", "Single-writer-write-path.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "pinned: true") {
		t.Errorf("pinned flag missing from frontmatter:\n%s", data)
	}
}
