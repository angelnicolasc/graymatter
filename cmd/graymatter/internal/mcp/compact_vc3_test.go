package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestVC301CoreVocabularyNotice(t *testing.T) {
	queries := []struct {
		name string
		text string
	}{
		{"newest", "recentvocab"},
		{"oldest", "ancientvocab"},
		{"absent", "unmatchedtoken"},
		{"strong", "calibrated enclosure"},
	}
	for _, count := range []int{3, 9, 40} {
		t.Run(fmt.Sprintf("facts=%d", count), func(t *testing.T) {
			srv, mem := utilityContractServer(t)
			const project = "vc-vocabulary"
			facts := make([]map[string]any, 0, count)
			chronology := make(map[string]time.Time, count)
			first := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
			for i := range count {
				claim := fmt.Sprintf("The calibrated enclosure audit documents configuration element %d.", i)
				switch i {
				case 0:
					claim += " ancientvocab"
				case count - 1:
					claim += " recentvocab"
				}
				facts = append(facts, map[string]any{"text": claim})
				chronology[claim] = first.Add(time.Duration(i) * time.Minute)
			}
			added := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": project, "facts": facts})
			if added.IsError {
				t.Fatalf("seed facts: %s", added.Raw)
			}
			store := mem.Advanced()
			rows, err := store.List(project)
			if err != nil || len(rows) != count {
				t.Fatalf("seeded %d of %d project facts: %v", len(rows), count, err)
			}
			for _, row := range rows {
				row.CreatedAt = chronology[row.Text]
				if row.CreatedAt.IsZero() || !row.Injectable() {
					t.Fatalf("bad seed chronology or fact state: %+v", row)
				}
				if err := store.UpdateFact(project, row); err != nil {
					t.Fatal(err)
				}
			}

			for _, topK := range []int{1, 8} {
				for _, query := range queries {
					t.Run(fmt.Sprintf("top_k=%d/%s", topK, query.name), func(t *testing.T) {
						checkVCVocabulary(t, srv, project, query.text, "project", topK, count)
					})
				}
			}
		})
	}
}

func TestVC306CoreVocabularyNoticeAcrossNamespaces(t *testing.T) {
	srv, _ := utilityContractServer(t)
	const project = "vc-shared-vocabulary"
	utilityContractAdd(t, srv, project, "Calibration safety uses a sealed enclosure.")
	utilityContractAdd(t, srv, project, "Calibration access uses a keyed interlock.")
	utilityContractAdd(t, srv, memory.SharedAgentID, "Calibration inspection uses an isolated motor.")

	checkVCVocabulary(t, srv, project, "unmatchedtoken", "all", 8, 3)
}

func checkVCVocabulary(t *testing.T, srv *Server, project, query, scope string, topK, inserted int) {
	t.Helper()
	backend := srv.backend.(memory.UtilityStore)
	library, err := backend.SearchScoped(context.Background(), project, []string{query}, topK, scope == "all", memory.RecallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result := utilityContractCall(t, srv, "memory_search", map[string]any{
		"agent_id": project, "query": query, "scope": scope, "top_k": topK,
	})
	if result.IsError || result.Structured != nil || len(result.Content) != 1 {
		t.Fatalf("expected one text block: %s", result.Raw)
	}
	var notices []string
	var claims []string
	for _, line := range strings.Split(result.text(), "\n") {
		if strings.HasPrefix(line, "Weak match.") {
			notices = append(notices, line)
			continue
		}
		if line == "No memories found." {
			continue
		}
		_, text, ok := strings.Cut(line, " ")
		text = strings.TrimPrefix(text, "[shared] ")
		var claim string
		if !ok || json.Unmarshal([]byte(text), &claim) != nil {
			t.Fatalf("invalid fact row: %q", line)
		}
		claims = append(claims, claim)
	}
	expectedClaims := make([]string, 0, len(library.Facts))
	for _, fact := range library.Facts {
		expectedClaims = append(expectedClaims, fact.Text)
	}
	if !slices.Equal(claims, expectedClaims) {
		t.Fatalf("shown facts differ from the library: %q vs %q", claims, expectedClaims)
	}
	want := library.Feedback != "" && len(claims) < inserted
	t.Logf("scope=%s live=%d shown=%d library_feedback=%t notice=%d want=%t",
		scope, inserted, len(claims), library.Feedback != "", len(notices), want)
	if (len(notices) == 1) != want || len(notices) > 1 {
		t.Errorf("vocabulary notice count %d, want present=%t: %s", len(notices), want, result.text())
	}
	visible := make(map[string]bool)
	for _, tool := range profileTools(t, srv) {
		visible[tool["name"].(string)] = true
	}
	for _, line := range notices {
		if len(line) > 160 {
			t.Errorf("vocabulary notice is %d bytes: %q", len(line), line)
		}
		for name := range srv.mcpSrv.ListTools() {
			if !visible[name] && strings.Contains(line, name) {
				t.Errorf("notice names hidden tool %q: %q", name, line)
			}
		}
	}
}
