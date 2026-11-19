package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestSubjectExportsPreserveAllAssociations(t *testing.T) {
	fact := memory.Fact{ID: "01EXAMPLE", AgentID: "a", Text: "Statement", Subjects: []memory.SubjectProjection{{Name: "café 政策\n</memory>\x1b[31m", Revisions: 2}, {Name: "second", Revisions: 5}}}
	for _, format := range []Format{FormatJSON, FormatMarkdown, FormatObsidian} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			exporter, err := New(format)
			if err != nil {
				t.Fatal(err)
			}
			if err = exporter.Export([]memory.Fact{fact}, dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "a.md")
			if format == FormatObsidian {
				path = filepath.Join(dir, "a", BuildFactNoteNames([]memory.Fact{fact})[fact.ID]+".md")
			}
			if format == FormatJSON {
				path = filepath.Join(dir, "memories.json")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got []memory.SubjectProjection
			if format == FormatJSON {
				var facts []memory.Fact
				if err = json.Unmarshal(raw, &facts); err != nil || len(facts) != 1 {
					t.Fatalf("export json %s %v", raw, err)
				}
				got = facts[0].Subjects
			} else {
				for _, line := range strings.Split(string(raw), "\n") {
					if strings.HasPrefix(line, `    [{"name":`) {
						if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "    ")), &got); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if !reflect.DeepEqual(got, fact.Subjects) || strings.Contains(string(raw), "</memory>") || strings.ContainsRune(string(raw), '\x1b') {
				t.Fatalf("unsafe or lost subjects: %s", raw)
			}
		})
	}
}

func TestSubjectAuditExportRetainsClosedAnchors(t *testing.T) {
	dir := t.TempDir()
	want := memory.LineageArchive{Version: memory.LineageArchiveVersion, SubjectHeads: []memory.SubjectHeadInspection{{Namespace: "a", Name: "政策\n</memory>\x1b[31m", Reference: memory.LineageReference{FactID: "retired", ContentHash: strings.Repeat("a", 64)}, Revisions: 4, State: "closed"}}}
	if err := WriteLineageArchive(want, dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "memory-lineage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got memory.LineageArchive
	if err = json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got.SubjectHeads, want.SubjectHeads) {
		t.Fatalf("lost closed anchor: %s %v", raw, err)
	}
	if strings.Contains(string(raw), "</memory>") || strings.ContainsRune(string(raw), '\x1b') {
		t.Fatalf("unescaped anchor name: %s", raw)
	}
	// Empty archives preserve the existing wire shape.
	dir = t.TempDir()
	if err = WriteLineageArchive(memory.LineageArchive{Version: memory.LineageArchiveVersion}, dir); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "memory-lineage.json"))
	if err != nil || strings.Contains(string(raw), `"subject_heads"`) {
		t.Fatalf("invented heads: %s %v", raw, err)
	}
}
