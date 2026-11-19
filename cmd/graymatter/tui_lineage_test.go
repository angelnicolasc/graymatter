package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/charmbracelet/x/ansi"
)

type workbenchLineageStore struct {
	workbenchStore
	versions map[string]memory.LineageInspection
}

func (s *workbenchLineageStore) InspectLineage(_ context.Context, ns, id, hash string) (memory.LineageInspection, error) {
	s.calls = append(s.calls, "lineage:"+ns+":"+id+":"+hash)
	if version, ok := s.versions[hash]; ok {
		return version, nil
	}
	return memory.LineageInspection{}, memory.ErrFactNotFound
}

func TestWorkbenchLineageFollowsExactHistoricalEvidence(t *testing.T) {
	source := memory.LineageReference{FactID: "source", ContentHash: "source-v1"}
	s := &workbenchLineageStore{versions: map[string]memory.LineageInspection{
		"old-head": {
			Snapshot: &memory.EvidenceSnapshot{ContentHash: "old-head", Text: "historical head text"},
			LineageProjection: memory.LineageProjection{
				ContentHash: "old-head", LineageStatus: memory.LineageVerified,
				Replaces:      []memory.LineageReference{source},
				LegacySources: []memory.LineageReference{{FactID: "ambiguous-source"}},
			},
		},
		"source-v1": {
			Snapshot: &memory.EvidenceSnapshot{ContentHash: "source-v1", Text: "exact retained source text"},
		},
	}}
	m := workbenchModel(t, &readOnlyTUIStore{cliStore: s})
	m.activeTab = tabRecall
	m.recallList.SetItems([]list.Item{receiptItem{receipt: memory.RecallReceipt{
		Text: "live canonical text",
		Provenance: memory.RecallProvenance{
			FactID: "head",
			LineageProjection: memory.LineageProjection{
				ContentHash: "current-head", HistoricalHash: "old-head", LineageStatus: memory.LineageContentChanged,
			},
		},
	}}})
	if preview := ansi.Strip(m.previewContent()); !strings.Contains(preview, "content_changed") || !strings.Contains(preview, "old-head") {
		t.Fatalf("receipt preview hides divergence: %s", preview)
	}
	m, cmd := press(m, keyMsg('v'))
	if m.modal != "lineage" || cmd == nil {
		t.Fatal("lineage inspector did not open")
	}
	loaded, _ := m.Update(cmd())
	m = loaded.(tuiModel)
	if len(m.lineage.links) != 1 {
		t.Fatal("unversioned legacy ID was offered as an exact source")
	}
	m, cmd = press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("following the source did not load a version")
	}
	loaded, _ = m.Update(cmd())
	m = loaded.(tuiModel)
	if view := ansi.Strip(m.renderLineage(m.width, m.bodyHeight())); !strings.Contains(view, "exact retained source text") {
		t.Fatalf("lineage view lost source evidence: %s", view)
	}
	m, cmd = press(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if cmd == nil {
		t.Fatal("back navigation did not load the prior version")
	}
	message := cmd()
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	loaded, _ = m.Update(message)
	if next := loaded.(tuiModel); next.modal != "" || next.activeTab != tabRecall {
		t.Fatal("stale lineage response reopened the inspector")
	}
	want := []string{"lineage:project:head:old-head", "lineage:project:source:source-v1", "lineage:project:head:old-head"}
	if !reflect.DeepEqual(s.calls, want) {
		t.Fatalf("unexpected store operations: %v", s.calls)
	}
}
