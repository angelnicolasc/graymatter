package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/lineageview"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type tuiLineageLink struct {
	label string
	ref   memory.LineageReference
}

type tuiLineage struct {
	generation uint64
	namespace  string
	ref        memory.LineageReference
	ancestors  []memory.LineageReference
	links      []tuiLineageLink
	selected   int
	view       viewport.Model
}

type lineageLoadedMsg struct {
	generation uint64
	inspection memory.LineageInspection
	err        error
}

func (m *tuiModel) openLineage() tea.Cmd {
	var namespace, id string
	var projection memory.LineageProjection
	switch m.activeTab {
	case tabMemory:
		if item, ok := m.factList.SelectedItem().(factItem); ok {
			namespace, id = item.fact.AgentID, item.fact.ID
			projection = item.fact.LineageProjection
		}
	case tabRecall:
		if item, ok := m.recallList.SelectedItem().(receiptItem); ok {
			namespace, id = m.namespace, item.receipt.Provenance.FactID
			projection = item.receipt.Provenance.LineageProjection
		}
	}
	if id == "" {
		m.status = "Select a fact or recall receipt to inspect its lineage"
		return nil
	}
	if _, ok := m.store.(lineageInspector); !ok {
		m.status = "Lineage inspection unavailable on this store"
		return nil
	}
	m.status = ""
	m.modal = "lineage"
	m.lineage = tuiLineage{generation: m.lineage.generation, namespace: namespace, view: viewport.New()}
	m.lineage.view.SoftWrap = true
	m.sizeLineageView()
	hash := projection.ContentHash
	if projection.LineageStatus == memory.LineageContentChanged && projection.HistoricalHash != "" {
		hash = projection.HistoricalHash
	}
	return m.loadLineage(memory.LineageReference{FactID: id, ContentHash: hash})
}

func (m *tuiModel) sizeLineageView() {
	m.lineage.view.SetWidth(max(1, m.width-2))
	m.lineage.view.SetHeight(max(1, m.bodyHeight()-3))
}

func (m *tuiModel) loadLineage(ref memory.LineageReference) tea.Cmd {
	s := &m.lineage
	s.generation++
	s.ref, s.links, s.selected = ref, nil, 0
	s.view.SetContent("Loading version evidence…")
	s.view.GotoTop()

	store := m.store.(lineageInspector)
	generation, namespace, parent := s.generation, s.namespace, m.commandContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		inspection, err := store.InspectLineage(ctx, namespace, ref.FactID, ref.ContentHash)
		return lineageLoadedMsg{generation: generation, inspection: inspection, err: err}
	}
}

func (m *tuiModel) lineageLoaded(msg lineageLoadedMsg) {
	s := &m.lineage
	if m.modal != "lineage" || msg.generation != s.generation {
		return
	}
	if errors.Is(msg.err, memory.ErrFactNotFound) {
		s.view.SetContent("Retained snapshot unavailable for this selection.")
		return
	}
	if msg.err != nil {
		s.view.SetContent("Lineage inspection failed · " + tuiPlain(msg.err.Error()))
		return
	}
	p := msg.inspection.LineageProjection
	for _, ref := range p.DerivedFrom {
		s.links = append(s.links, tuiLineageLink{"Derived from", ref})
	}
	for _, ref := range p.Replaces {
		s.links = append(s.links, tuiLineageLink{"Replaces", ref})
	}
	for _, ref := range p.LegacySources {
		if ref.ContentHash != "" {
			s.links = append(s.links, tuiLineageLink{"Legacy source (relationship ambiguous)", ref})
		}
	}

	lines := []string{"Namespace · " + s.namespace, "Fact · " + s.ref.FactID}
	lines = append(lines, lineageview.Lines(p)...)
	if msg.inspection.CapturedAt != nil {
		lines = append(lines, "Legacy evidence captured · "+msg.inspection.CapturedAt.Format(time.RFC3339))
	}
	if snapshot := msg.inspection.Snapshot; snapshot != nil {
		// Use the canonical hash for breadcrumbs after an empty-hash lookup.
		s.ref.ContentHash = snapshot.ContentHash
		lines = append(lines, "Written · "+snapshot.CreatedAt.Format(time.RFC3339),
			"Confidence · "+confidenceDetail(snapshot.Confidence), "", snapshot.Text)
	} else {
		lines = append(lines, "", "Retained snapshot unavailable for this selection.")
	}
	lines = append(lines, "", "Evidence inspection is read-only. Retained snapshots stay outside recall.")
	s.view.SetContent(tuiPlain(strings.Join(lines, "\n")))
}

func (m tuiModel) handleLineageKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := &m.lineage
	switch msg.String() {
	case "esc":
		m.modal = ""
		m.lineage = tuiLineage{generation: s.generation}
	case "tab", "shift+tab":
		if n := len(s.links); n > 0 {
			step := 1
			if msg.String() == "shift+tab" {
				step = -1
			}
			s.selected = (s.selected + step + n) % n
		}
	case "enter":
		if len(s.links) > 0 {
			s.ancestors = append(s.ancestors, s.ref)
			cmd := m.loadLineage(s.links[s.selected].ref)
			return m, cmd
		}
	case "backspace":
		if n := len(s.ancestors); n > 0 {
			ref := s.ancestors[n-1]
			s.ancestors = s.ancestors[:n-1]
			cmd := m.loadLineage(ref)
			return m, cmd
		}
	default:
		var cmd tea.Cmd
		s.view, cmd = s.view.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m tuiModel) renderLineage(w, h int) string {
	s := m.lineage
	selected := "Version evidence · read-only"
	if len(s.links) > 0 {
		link := s.links[s.selected]
		selected = fmt.Sprintf("%d/%d  %s", s.selected+1, len(s.links), lineageview.Reference(link.label, link.ref))
	}
	return m.panel("Memory lineage", m.muted(tuiPlain(selected))+"\n"+s.view.View(), w, h, true)
}
