package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/charmbracelet/x/ansi"
)

type workbenchStore struct {
	cliStore
	calls        []string
	lastExpected memory.Fact
	err          error
}

func (s *workbenchStore) CurateFact(_ context.Context, a, id, action, text string, f memory.Fact) (memory.Fact, error) {
	s.calls = append(s.calls, action+":"+a+":"+id)
	s.lastExpected = f
	if s.err != nil {
		return memory.Fact{}, s.err
	}
	return f, nil
}
func (s *workbenchStore) Remember(_ context.Context, a, text string) error {
	s.calls = append(s.calls, "add:"+a+":"+text)
	return s.err
}
func (s *workbenchStore) InspectRecall(_ context.Context, a, q string, k int) ([]memory.RecallReceipt, error) {
	s.calls = append(s.calls, "inspect:"+a+":"+q)
	return nil, s.err
}
func (s *workbenchStore) SessionKill(id string) error {
	s.calls = append(s.calls, "kill:"+id)
	return s.err
}
func workbenchModel(t *testing.T, s cliStore) tuiModel {
	t.Helper()
	m := newTUIModel(s, t.TempDir(), false, "dark", false)
	m.namespace = "project"
	f := memory.Fact{ID: "fact-1", AgentID: "project", Text: "A carefully selected fact", Weight: 1, CreatedAt: time.Now()}
	m.factList.SetItems([]list.Item{factItem{f}})
	m.syncPreview(true)
	return m
}
func press(m tuiModel, k tea.KeyPressMsg) (tuiModel, tea.Cmd) {
	next, c := m.Update(k)
	return next.(tuiModel), c
}

func TestWorkbenchInputOwnsAllPrintableKeys(t *testing.T) {
	s := &workbenchStore{}
	for _, mode := range []string{"palette", "filter", "namespace", "recall"} {
		t.Run(mode, func(t *testing.T) {
			m := workbenchModel(t, s)
			m.openInput(mode, "")
			for _, r := range "qdk123uy" {
				m, _ = press(m, keyMsg(r))
			}
			if m.input.Value() != "qdk123uy" || m.activeTab != tabMemory || m.modal != "" {
				t.Fatalf("input leaked to actions: %q tab=%d modal=%q", m.input.Value(), m.activeTab, m.modal)
			}
		})
	}
	m := workbenchModel(t, s)
	m, _ = press(m, keyMsg('a'))
	for _, r := range "qdk123uy" {
		m, _ = press(m, keyMsg(r))
	}
	if m.editor.Value() != "qdk123uy" || m.modal != "add" {
		t.Fatalf("editor leaked keys: %q %q", m.editor.Value(), m.modal)
	}
	if len(s.calls) != 0 {
		t.Fatalf("typing performed store work: %v", s.calls)
	}
}

func TestWorkbenchRetireRequiresConfirmationAndSnapshot(t *testing.T) {
	s := &workbenchStore{}
	m := workbenchModel(t, s)
	m, c := press(m, keyMsg('d'))
	if m.modal != "retire" || c != nil || len(s.calls) != 0 {
		t.Fatal("retire did not wait for confirmation")
	}
	m, c = press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if c == nil || !m.busy {
		t.Fatal("confirmation did not dispatch")
	}
	msg := c()
	if len(s.calls) != 1 || s.calls[0] != "retire:project:fact-1" || s.lastExpected.Text != "A carefully selected fact" {
		t.Fatalf("wrong selected fact: %v %+v", s.calls, s.lastExpected)
	}
	m2, _ := m.Update(msg)
	if m2.(tuiModel).modal != "" || !strings.Contains(m2.(tuiModel).status, "confirmed") {
		t.Fatal("successful action not acknowledged")
	}
}

func TestWorkbenchMutationFailurePreservesEditor(t *testing.T) {
	s := &workbenchStore{err: errors.New("write denied")}
	m := workbenchModel(t, s)
	m, _ = press(m, keyMsg('e'))
	m.editor.SetValue("Revised text")
	m, c := press(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if c == nil {
		t.Fatal("save missing")
	}
	next, _ := m.Update(c())
	m = next.(tuiModel)
	if m.modal != "revise" || m.editor.Value() != "Revised text" || m.busy || !strings.Contains(m.status, "write denied") {
		t.Fatalf("lost failure state: %+v", m.status)
	}
	next, _ = m.Update(mutationMsg{err: memory.ErrMutationOutcomeUnknown})
	m = next.(tuiModel)
	if m.modal != "" || !strings.Contains(m.status, "Outcome unknown") {
		t.Fatal("ambiguous mutation left replayable modal")
	}
}

func TestWorkbenchRevisionReportsKnownCommitDespiteFollowUpError(t *testing.T) {
	m := workbenchModel(t, &workbenchStore{})
	m.modal, m.busy = "revise", true
	next, refresh := m.Update(mutationMsg{
		action: "revise",
		agent:  "project",
		fact:   memory.Fact{ID: "replacement-id"},
		err:    errors.New("follow-up failed"),
	})
	m = next.(tuiModel)
	if m.modal != "" || m.busy || m.unknownMutation != "" || refresh == nil {
		t.Fatalf("known commit was not closed and refreshed: %q", m.status)
	}
	if !strings.Contains(m.status, "Revision committed as replacement-id") || !strings.Contains(m.status, "follow-up failed") {
		t.Fatalf("known commit receipt hidden: %q", m.status)
	}
}

func TestWorkbenchKOnlyNavigatesAndStopRequiresX(t *testing.T) {
	s := &workbenchStore{}
	m := workbenchModel(t, s)
	m.activeTab = tabSessions
	m.sessionList.SetItems([]list.Item{sessionItem{harness.HarnessSession{ID: "run1", Status: "running"}}, sessionItem{harness.HarnessSession{ID: "run2", Status: "running"}}})
	m.sessionList.Select(1)
	m, _ = press(m, keyMsg('k'))
	if m.sessionList.Index() != 0 || m.modal != "" || len(s.calls) != 0 {
		t.Fatal("k executed destructive action")
	}
	m, _ = press(m, keyMsg('x'))
	if m.modal != "kill" || len(s.calls) != 0 {
		t.Fatal("x did not require confirmation")
	}
	_, c := press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	c()
	if len(s.calls) != 1 || s.calls[0] != "kill:run1" {
		t.Fatalf("wrong run: %v", s.calls)
	}
}

func TestWorkbenchRejectsStaleFactsRecallAndResourceLoads(t *testing.T) {
	m := workbenchModel(t, nil)
	m.factsGeneration = 5
	m.recallGeneration = 7
	m.resources["graph"] = resourceState{Generation: 4, Loading: true}
	for _, msg := range []tea.Msg{
		factsLoadedMsg{agent: "project", generation: 4, facts: []factItem{{memory.Fact{ID: "old"}}}},
		factsLoadedMsg{agent: "other", generation: 6, facts: []factItem{{memory.Fact{ID: "wrong"}}}},
		recallLoadedMsg{agent: "project", generation: 6, err: errors.New("stale")},
		nodesLoadedMsg{generation: 3, nodes: []nodeItem{{kg.Node{ID: "stale"}}}},
		resourceErrMsg{resource: "graph", generation: 3, err: errors.New("stale error")},
	} {
		next, _ := m.Update(msg)
		m = next.(tuiModel)
	}
	if m.selectedFactID() != "fact-1" || len(m.nodeList.Items()) != 0 || m.resources["graph"].Err != nil || m.resources["recall"].Err != nil {
		t.Fatal("stale load replaced current scope")
	}
}

func TestWorkbenchRecallExplicitAndSafe(t *testing.T) {
	s := &workbenchStore{}
	m := workbenchModel(t, s)
	m, _ = press(m, keyMsg('2'))
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	for _, r := range "question" {
		m, _ = press(m, keyMsg(r))
	}
	if len(s.calls) != 0 {
		t.Fatal("typing queried provider")
	}
	m, c := press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if c == nil {
		t.Fatal("query missing")
	}
	msg := c()
	if len(s.calls) != 1 || s.calls[0] != "inspect:project:question" {
		t.Fatalf("unsafe or wrong query: %v", s.calls)
	}
	next, _ := m.Update(msg)
	if next.(tuiModel).resources["recall"].Err != nil {
		t.Fatal("safe result rejected")
	}
	unsupported := workbenchModel(t, nil)
	msg = unsupported.inspectRecall("query")()
	if msg.(recallLoadedMsg).err == nil {
		t.Fatal("missing safe capability silently fell back")
	}
}

func TestWorkbenchGraphInspectorScrollsAndPreservesSelection(t *testing.T) {
	m := workbenchModel(t, nil)
	m.activeTab = tabGraph
	m.width, m.height = 80, 24
	m.focus = 1
	m.nodeList.SetItems([]list.Item{nodeItem{kg.Node{ID: "one", Label: "One"}}})
	for i := 0; i < 40; i++ {
		m.graphEdges = append(m.graphEdges, kg.Edge{From: "one", To: fmt.Sprint(i), Relation: "depends on", Sources: []string{"source"}})
	}
	m.syncPreview(true)
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	offset := m.detail.YOffset()
	if offset == 0 {
		t.Fatal("inspector did not scroll")
	}
	m.syncPreview(false)
	if m.detail.YOffset() != offset {
		t.Fatal("refresh reset inspection position")
	}
}

func TestWorkbenchViewsBoundedInCellsAndSanitizeStoredText(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, size := range [][2]int{{80, 24}, {80, 29}, {80, 30}, {100, 30}, {120, 36}, {160, 48}} {
		for tab := tabMemory; tab <= tabStats; tab++ {
			for _, focus := range []int{0, 1} {
				t.Run(fmt.Sprintf("%dx%d/%d/%d", size[0], size[1], tab, focus), func(t *testing.T) {
					m := workbenchModel(t, nil)
					m.width, m.height = size[0], size[1]
					m.activeTab = tab
					m.focus = focus
					m.factList.SetItems([]list.Item{factItem{memory.Fact{ID: "f", Text: strings.Repeat("界👩‍💻 ", 200) + "\x1b]52;c;ZWZmaW5n\a\x1b[31mBAD\x1b[0m\u202e", AgentID: "project"}}})
					m.dashboard.Loaded = true
					m.syncPreview(true)
					for _, mode := range []string{"", "help", "retire", "add", "revise"} {
						m.modal = mode
						out := m.View().Content
						lines := strings.Split(out, "\n")
						if len(lines) != m.height {
							t.Fatalf("height %d", len(lines))
						}
						for _, line := range lines {
							if ansi.StringWidth(line) != m.width {
								t.Fatalf("width %d != %d", ansi.StringWidth(line), m.width)
							}
						}
						if strings.ContainsAny(out, "\x1b\a\u202e") {
							t.Fatal("terminal control leaked")
						}
						if !strings.Contains(out, "GRAYMATTER") {
							t.Fatal("header missing")
						}
					}
				})
			}
		}
	}
}

func TestWorkbenchUsageAndPaletteRouteWithoutMemoryKeys(t *testing.T) {
	m := workbenchModel(t, nil)
	m, _ = press(m, keyMsg('/'))
	for _, r := range "usage" {
		m, _ = press(m, keyMsg(r))
	}
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.activeTab != tabUsage {
		t.Fatal("palette did not switch view")
	}
	m, _ = press(m, keyMsg('s'))
	if m.usage.mode != "spend" {
		t.Fatal("Usage s captured by another view")
	}
	m, _ = press(m, keyMsg('l'))
	if m.usage.mode != "limits" {
		t.Fatal("Usage l captured by pane navigation")
	}
}

func TestWorkbenchFilterSurvivesRefresh(t *testing.T) {
	m := workbenchModel(t, nil)
	m.activeTab = tabGraph
	m.nodeList.SetItems([]list.Item{nodeItem{kg.Node{ID: "1", Label: "Alpha"}}, nodeItem{kg.Node{ID: "2", Label: "Other"}}})
	m.filterLocal("alpha")
	if len(m.nodeList.VisibleItems()) != 1 {
		t.Fatal("filter not applied")
	}
	next, _ := m.Update(nodesLoadedMsg{nodes: []nodeItem{{kg.Node{ID: "3", Label: "Alpha new"}}, {kg.Node{ID: "4", Label: "Unrelated"}}}})
	m = next.(tuiModel)
	if len(m.nodeList.VisibleItems()) != 1 || m.nodeList.VisibleItems()[0].(nodeItem).n.ID != "3" {
		t.Fatal("refresh lost filter or kept stale filtered items")
	}
}

func TestWorkbenchInitialNarrowLayoutShowsMemory(t *testing.T) {
	m := workbenchModel(t, nil)
	m.width, m.height = 80, 24
	m.syncPreview(true)
	if m.memPane != memPaneFacts || !strings.Contains(m.View().Content, "carefully selected") {
		t.Fatal("narrow layout starts in namespace picker")
	}
}

func TestWorkbenchMouseVisiblePageAndFilteredSelection(t *testing.T) {
	m := workbenchModel(t, nil)
	m.width, m.height = 120, 24
	var items []list.Item
	for i := 0; i < 20; i++ {
		items = append(items, agentItem{id: fmt.Sprintf("namespace-%02d", i)})
	}
	m.agentList.SetItems(items)
	m.updateSizes()
	m.agentList.Select(16)
	m.handleMouse(tea.MouseClickMsg{X: 2, Y: 7, Button: tea.MouseLeft})
	if m.agentList.Index() != 17 || m.namespace != "namespace-17" {
		t.Fatalf("namespace click lost page: %d %s", m.agentList.Index(), m.namespace)
	}
	m.activeTab = tabGraph
	m.nodeList.SetItems([]list.Item{nodeItem{kg.Node{ID: "skip", Label: "Other"}}, nodeItem{kg.Node{ID: "a", Label: "Match A"}}, nodeItem{kg.Node{ID: "b", Label: "Match B"}}})
	m.filterLocal("Match")
	m.nodeList.Select(1)
	m.handleMouse(tea.MouseClickMsg{X: 2, Y: 6, Button: tea.MouseLeft})
	if n, ok := m.nodeList.SelectedItem().(nodeItem); !ok || n.n.ID != "a" {
		t.Fatalf("click used unfiltered index: %+v", n)
	}
	for i, name := range tabNames {
		// Click the visible text, independent of the tabs' padding.
		line := strings.Split(ansi.Strip(m.renderHeader()), "\n")[2]
		pos := strings.Index(line, name)
		m.handleMouse(tea.MouseClickMsg{X: pos + 1, Y: 2, Button: tea.MouseLeft})
		if int(m.activeTab) != i {
			t.Fatalf("tab hit for %s selected %d", name, m.activeTab)
		}
	}
}

func TestWorkbenchSpacedMemoryPaginationMouseAndResize(t *testing.T) {
	for _, tc := range []struct{ height, perPage, firstRow, rowHeight int }{{24, 8, 6, 2}, {29, 10, 6, 2}, {30, 7, 7, 3}, {36, 9, 7, 3}, {48, 13, 7, 3}} {
		t.Run(fmt.Sprint(tc.height), func(t *testing.T) {
			m := workbenchModel(t, nil)
			m.width, m.height = 120, tc.height
			var items []list.Item
			for i := 0; i < 50; i++ {
				items = append(items, factItem{memory.Fact{ID: fmt.Sprint(i), Text: fmt.Sprintf("record %02d", i)}})
			}
			m.factList.SetItems(items)
			m.syncPreview(true)
			if got := m.factList.Paginator.PerPage; got != tc.perPage {
				t.Fatalf("keyboard page size %d, want %d", got, tc.perPage)
			}
			m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
			if m.factList.Index() != tc.perPage {
				t.Fatalf("PageDown selected %d, want %d", m.factList.Index(), tc.perPage)
			}
			_, listWidth, _ := m.columnWidths()
			visible := ansi.Strip(m.renderList(m.factList, listWidth-2, m.contentHeight(), ""))
			if !strings.Contains(visible, fmt.Sprintf("record %02d", 2*tc.perPage-1)) || strings.Contains(visible, fmt.Sprintf("record %02d", 2*tc.perPage)) {
				t.Fatal("rendered page differs from keyboard page")
			}
			m.handleMouse(tea.MouseClickMsg{X: 22, Y: tc.firstRow + tc.rowHeight, Button: tea.MouseLeft})
			want := tc.perPage + 1
			if m.factList.Index() != want {
				t.Fatalf("mouse selected %d, want %d", m.factList.Index(), want)
			}
			if tc.rowHeight == 3 {
				m.handleMouse(tea.MouseClickMsg{X: 22, Y: tc.firstRow + 2, Button: tea.MouseLeft})
				if m.factList.Index() != want {
					t.Fatal("inter-item whitespace changed selection")
				}
			}
			if unused := tc.firstRow + tc.perPage*tc.rowHeight; unused < m.height-2 {
				m.handleMouse(tea.MouseClickMsg{X: 22, Y: unused, Button: tea.MouseLeft})
				if m.factList.Index() != want {
					t.Fatal("unused bottom row selected an offscreen item")
				}
			}
			m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyTab})
			for _, height := range []int{29, 30} {
				next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: height})
				m = next.(tuiModel)
				if m.factList.Index() != want || m.focus != 1 || m.memPane != memPaneDetail {
					t.Fatal("spacing breakpoint lost selected fact or inspector focus")
				}
			}
		})
	}
}

func TestWorkbenchPreferencesScopedAndExplicitThemeWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbench", "prefs.json")
	m := workbenchModel(t, nil)
	m.namespace = "saved"
	m.factMode = "retired"
	m.factQuery = "needle"
	m.setTheme("light")
	if err := m.savePreferences(path); err != nil {
		t.Fatal(err)
	}
	reopened := workbenchModel(t, nil)
	reopened.restorePreferences(path, "")
	if reopened.namespace != "saved" || reopened.factMode != "retired" || reopened.factQuery != "needle" || reopened.themeName != "light" || reopened.restoreFactID != "fact-1" {
		t.Fatal("preferences not restored")
	}
	reopened.setTheme("terminal")
	reopened.restorePreferences(path, "terminal")
	if reopened.themeName != "terminal" {
		t.Fatal("saved theme overrode explicit flag")
	}
	if tuiPreferencesPath("store-a") == tuiPreferencesPath("store-b") {
		t.Fatal("store preferences collide")
	}
	m.demo = true
	demoPath := filepath.Join(t.TempDir(), "demo.json")
	if err := m.savePreferences(demoPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(demoPath); !os.IsNotExist(err) {
		t.Fatal("demo persisted preferences")
	}
}

func TestWorkbenchUnknownMutationBlocksUntilRefresh(t *testing.T) {
	m := workbenchModel(t, &workbenchStore{})
	next, _ := m.Update(mutationMsg{agent: "project", action: "Add", err: memory.ErrMutationOutcomeUnknown})
	m = next.(tuiModel)
	m, _ = press(m, keyMsg('a'))
	if m.modal != "" || m.unknownMutation == "" {
		t.Fatal("uncertain add can be repeated without refresh")
	}
	next, _ = m.Update(factsLoadedMsg{agent: "project", facts: []factItem{{memory.Fact{ID: "checked", AgentID: "project"}}}})
	m = next.(tuiModel)
	m, _ = press(m, keyMsg('a'))
	if m.modal != "add" || m.unknownMutation != "" {
		t.Fatal("refresh did not unblock inspected mutation")
	}
}

func TestWorkbenchUsageMouseAndSpaceAreScoped(t *testing.T) {
	m := workbenchModel(t, nil)
	m.usage.loaded = true
	m.usage.snapshot = usageFixture()
	m.width, m.height = 80, 24
	for _, tab := range []tabID{tabMemory, tabUsage} {
		for _, enabled := range []bool{false, true} {
			m.activeTab = tab
			m.mouse = enabled
			m.usage.offset = 0
			next, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
			out := next.(tuiModel)
			if (out.usage.offset > 0) != (tab == tabUsage && enabled) {
				t.Fatalf("wheel routing tab%d enabled%t offset%d", tab, enabled, out.usage.offset)
			}
		}
	}
	m.activeTab = tabUsage
	m.usage.offset = 0
	m, _ = press(m, keyMsg(' '))
	if m.usage.offset == 0 {
		t.Fatalf("space (%q) did not page Usage", keyMsg(' ').String())
	}
	m.mouse = true
	m.activeTab = tabMemory
	next, _ := m.Update(tea.MouseClickMsg{X: 2, Y: m.height - 1, Button: tea.MouseLeft})
	if next.(tuiModel).activeTab != tabUsage {
		t.Fatal("footer link did not open Usage")
	}
}

func TestWorkbenchConfidenceProvenanceAndFinalRanking(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		declared  bool
	}{
		{"", "not declared (effective: inferred)", false},
		{"verified", "verified (writer-declared)", true},
		{"inferred", "inferred (writer-declared)", true},
		{"unverified", "unverified (writer-declared)", true},
		{"legacy-high", `"legacy-high" (unknown label; effective: unverified)`, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			m := workbenchModel(t, nil)
			m.activeTab = tabRecall
			m.recallList.SetItems([]list.Item{receiptItem{receipt: memory.RecallReceipt{Text: "Fact", Provenance: memory.RecallProvenance{Confidence: tc.raw}}}})
			for _, out := range []string{formatFactDetail(memory.Fact{Confidence: tc.raw}), m.previewContent()} {
				if !strings.Contains(out, "Confidence: "+tc.want) || strings.Contains(out, "writer-declared") != tc.declared {
					t.Fatalf("misleading confidence provenance: %s", out)
				}
			}
		})
	}
	r := memory.RecallReceipt{Text: "Fact", Ranks: memory.RecallRanks{FusedScore: 0.025, K: 60}, Ranking: &memory.ConfidenceRanking{BaseScore: 0.025, FinalScore: 0.03, Factor: 1.2, EffectiveConfidence: "verified", ConfidenceWeight: 0.2, Policy: memory.ConfidencePolicy}}
	m := workbenchModel(t, nil)
	m.activeTab = tabRecall
	m.recallList.SetItems([]list.Item{receiptItem{receipt: r, rank: 1}})
	out := m.previewContent()
	for _, want := range []string{"Base RRF score: 0.025000", "Final score: 0.030000", "Confidence factor: 1.2000", "Policy: confidence-v1", "confidence weight 0.2000", "Effective confidence: verified"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing ranking receipt %q: %s", want, out)
		}
	}
	if got := (receiptItem{receipt: r}).Description(); !strings.Contains(got, "Final 0.03000") {
		t.Fatalf("list implies base RRF governed final order: %s", got)
	}
	r.Ranking = nil
	if strings.Contains(receiptScoreDetail(r), "Final score") || strings.Contains(receiptScoreDetail(r), "Policy:") {
		t.Fatal("invented missing ranking metadata")
	}
}

func TestWorkbenchResizeAndRedrawPreserveEditingState(t *testing.T) {
	m := workbenchModel(t, nil)
	m.modal = "revise"
	m.editor.SetValue("Unsaved edit survives resize")
	selected := m.selectedFactID()
	clearType := reflect.TypeOf(tea.ClearScreen())
	for _, size := range [][2]int{{119, 36}, {80, 23}, {160, 48}} {
		next, cmd := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = next.(tuiModel)
		if cmd == nil || reflect.TypeOf(cmd()) != clearType {
			t.Fatal("actual resize did not invalidate terminal screen")
		}
		if m.modal != "revise" || m.editor.Value() != "Unsaved edit survives resize" || m.selectedFactID() != selected {
			t.Fatal("resize changed editing state")
		}
		_, cmd = m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if cmd != nil {
			t.Fatal("unchanged size causes repeated full redraw")
		}
	}
	m, cmd := press(m, tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	if cmd == nil || reflect.TypeOf(cmd()) != clearType || m.editor.Value() != "Unsaved edit survives resize" {
		t.Fatal("Ctrl+L did not redraw cleanly while editing")
	}
	m.modal = ""
	m.openInput("filter", "draft query")
	m, cmd = press(m, tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	if cmd == nil || reflect.TypeOf(cmd()) != clearType || m.input.Value() != "draft query" || m.inputMode != "filter" {
		t.Fatal("Ctrl+L changed focused input")
	}
}

func TestWorkbenchRecallRefreshDispatchesOnceAndRejectsPreviousResult(t *testing.T) {
	s := &workbenchStore{}
	m := workbenchModel(t, s)
	m.activeTab = tabRecall
	m, cmd := press(m, keyMsg('r'))
	if cmd != nil || len(s.calls) != 0 || !strings.Contains(m.status, "Enter a query first") {
		t.Fatal("empty Recall refresh dispatched a query")
	}
	first := m.inspectRecall("original query")()
	next, _ := m.Update(first)
	m = next.(tuiModel)
	if len(s.calls) != 1 {
		t.Fatal("initial inspection did not dispatch once")
	}
	previousGeneration := m.recallGeneration
	m, cmd = press(m, keyMsg('r'))
	if cmd == nil || len(s.calls) != 1 || m.recallGeneration != previousGeneration+1 {
		t.Fatal("refresh is not a single asynchronous new request")
	}
	next, _ = m.Update(first)
	m = next.(tuiModel)
	if !m.resources["recall"].Loading {
		t.Fatal("previous result completed the newer refresh")
	}
	result := cmd()
	if len(s.calls) != 2 || s.calls[1] != "inspect:project:original query" {
		t.Fatalf("refresh dispatched wrong or duplicate query: %v", s.calls)
	}
	next, _ = m.Update(result)
	m = next.(tuiModel)
	if m.resources["recall"].Loading || m.resources["recall"].Err != nil {
		t.Fatal("fresh result was not accepted")
	}
}
