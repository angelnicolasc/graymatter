package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
)

type copyResultMsg struct {
	text string
	err  error
}
type paletteCommand struct{ name, description, key string }

var tuiCommands = []paletteCommand{
	{"memory", "Browse and curate facts", "1"}, {"recall", "Inspect retrieval without access touches", "2"}, {"activity", "Harness runs and checkpoints", "3"}, {"graph", "Relationships and supporting facts", "4"}, {"usage", "Subscription limits and API spend", "u"}, {"status", "Store status and diagnostics", "6"},
	{"namespace", "Choose memory namespace", "n"}, {"filter", "Filter text in current list", "f"}, {"add", "Add a fact to the current namespace", "a"}, {"revise", "Replace selected fact with traceable revision", "e"}, {"retire", "Retire selected fact as a tombstone", "d"}, {"pin", "Toggle protection from automatic decay and pruning", "p"}, {"copy", "Copy selected content", "y"}, {"refresh", "Refresh the current resource", "r"},
	{"theme dark", "Use the dark theme", ""}, {"theme light", "Use the light theme", ""}, {"theme terminal", "Use terminal foreground and background", ""}, {"mouse", "Toggle mouse navigation", ""}, {"help", "Keyboard reference and data scope", "?"}, {"quit", "Close the workbench", "q"},
	{"redraw", "Clear and redraw the terminal", "Ctrl+L"},
}

func (m tuiModel) matchingCommands() []paletteCommand {
	q := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(m.input.Value(), "/")))
	var out []paletteCommand
	for _, c := range tuiCommands {
		if strings.Contains(c.name, q) || strings.Contains(strings.ToLower(c.description), q) {
			out = append(out, c)
		}
	}
	return out
}
func (m *tuiModel) openInput(mode, value string) tea.Cmd {
	m.inputMode = mode
	m.paletteIndex = 0
	m.input.SetValue(value)
	m.input.Placeholder = map[string]string{"palette": "Type a command, e.g. /usage", "filter": "Filter current list", "recall": "Ask what the agent should remember", "namespace": "Choose a namespace", "graph-source": "Choose a supporting fact", "graph-neighbor": "Choose a connected entity"}[mode]
	return m.input.Focus()
}
func (m *tuiModel) closeInput() { m.inputMode = ""; m.input.Blur(); m.paletteIndex = 0 }

func (m tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+l" {
		return m, tea.ClearScreen
	}
	if key == "ctrl+c" {
		if m.recallCancel != nil {
			m.recallCancel()
		}
		return m, tea.Quit
	}
	if m.modal == "lineage" {
		return m.handleLineageKey(msg)
	}
	if m.modal != "" {
		return m.handleModalKey(msg)
	}
	if m.inputMode != "" {
		return m.handleInputKey(msg)
	}
	if m.activeTab == tabUsage {
		switch key {
		case "a", "l", "s", "c", "v", "g", "up", "down", "j", "k", "pgup", "pgdown", "home", "end", "r", "space", " ":
			return m, m.updateUsage(msg)
		}
	}
	switch key {
	case "q":
		if m.recallCancel != nil {
			m.recallCancel()
		}
		return m, tea.Quit
	case "1", "2", "3", "4", "5", "6":
		return m, m.switchTab(tabID(key[0] - '1'))
	case "u":
		return m, m.switchTab(tabUsage)
	case "/", ":", "ctrl+k":
		return m, m.openInput("palette", "")
	case "?":
		m.modal = "help"
		m.detail.GotoTop()
		return m, nil
	case "n":
		return m, m.openInput("namespace", "")
	case "f", "ctrl+f":
		return m, m.openFilter()
	case "r":
		return m, m.refresh()
	case "tab", "shift+tab":
		step := 1
		if key == "shift+tab" {
			step = -1
		}
		if m.activeTab == tabMemory && m.width >= 120 {
			m.memPane = memPane((int(m.memPane) + step + 3) % 3)
			if m.memPane == memPaneDetail {
				m.focus = 1
			} else {
				m.focus = 0
			}
		} else {
			m.focus = 1 - m.focus
			if m.focus == 1 {
				m.memPane = memPaneDetail
			} else {
				m.memPane = memPaneFacts
			}
		}
		m.updateSizes()
		m.syncPreview(false)
		return m, nil
	case "esc":
		m.focus = 0
		m.memPane = memPaneFacts
		return m, nil
	case "y":
		return m, m.copySelection()
	case "v":
		if m.activeTab == tabMemory || m.activeTab == tabRecall {
			cmd := m.openLineage()
			return m, cmd
		}
	}
	switch m.activeTab {
	case tabMemory:
		switch key {
		case "a", "e", "d", "p":
			return m, tea.Batch(m.updateMemoryKey(msg)...)
		case "[", "]":
			modes := []string{"active", "retired", "alias", "all"}
			idx := 0
			for i, v := range modes {
				if v == m.factMode {
					idx = i
				}
			}
			step := 1
			if key == "[" {
				step = -1
			}
			m.factMode = modes[(idx+step+4)%4]
			if id := m.selectedFactID(); id != "" {
				m.restoreFactID = id
			}
			setListItems(&m.factList, nil)
			m.previewID = ""
			return m, m.loadFacts(m.namespace)
		case "enter":
			if m.memPane == memPaneAgents {
				if s, ok := m.agentList.SelectedItem().(agentItem); ok {
					m.memPane = memPaneFacts
					return m, m.chooseNamespace(s.id)
				}
			}
			m.memPane = memPaneDetail
			m.focus = 1
			m.updateSizes()
			m.syncPreview(false)
			return m, nil
		case "right", "l":
			if m.memPane == memPaneAgents {
				m.memPane = memPaneFacts
			} else {
				m.memPane = memPaneDetail
				m.focus = 1
			}
			return m, nil
		case "left", "h":
			if m.memPane == memPaneDetail {
				m.memPane = memPaneFacts
				m.focus = 0
			} else if m.width >= 120 {
				m.memPane = memPaneAgents
			}
			return m, nil
		case "more", "ctrl+n":
			if m.nextFactCursor != "" && !m.state("memory").Loading {
				return m, m.loadFactPage(m.namespace, m.nextFactCursor)
			}
		}
	case tabRecall:
		if key == "enter" || key == "i" {
			return m, m.openInput("recall", m.recallQuery)
		}
	case tabSessions:
		if key == "c" {
			m.activityCheckpoints = !m.activityCheckpoints
			m.previewID = ""
			m.syncPreview(true)
			return m, nil
		}
		if key == "x" {
			return m, tea.Batch(m.updateSessionsKey(msg)...)
		}
		if key == "enter" {
			m.focus = 1
			return m, nil
		}
	case tabGraph:
		if key == "o" {
			return m, m.openInput("graph-neighbor", "")
		}
		if key == "s" {
			return m, m.openInput("graph-source", "")
		}
		if key == "enter" {
			m.focus = 1
			return m, nil
		}
	}
	if m.focus == 1 || m.memPane == memPaneDetail || m.activeTab == tabStats {
		var c tea.Cmd
		m.detail, c = m.detail.Update(msg)
		return m, c
	}
	c := m.moveList(msg)
	m.syncPreview(false)
	if m.activeTab == tabMemory && m.memPane == memPaneAgents {
		if s, ok := m.agentList.SelectedItem().(agentItem); ok && s.id != m.namespace {
			return m, tea.Batch(c, m.chooseNamespace(s.id))
		}
	}
	return m, c
}

func (m *tuiModel) moveList(msg tea.Msg) tea.Cmd {
	var c tea.Cmd
	switch m.activeTab {
	case tabMemory:
		if m.memPane == memPaneAgents {
			m.agentList, c = m.agentList.Update(msg)
		} else {
			m.factList, c = m.factList.Update(msg)
		}
	case tabRecall:
		m.recallList, c = m.recallList.Update(msg)
	case tabSessions:
		if m.activityCheckpoints {
			m.checkpointList, c = m.checkpointList.Update(msg)
		} else {
			m.sessionList, c = m.sessionList.Update(msg)
		}
	case tabGraph:
		m.nodeList, c = m.nodeList.Update(msg)
	}
	return c
}

func (m tuiModel) handleInputKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "esc" {
		m.closeInput()
		return m, nil
	}
	if key == "up" || key == "down" {
		if key == "up" {
			m.paletteIndex = max(0, m.paletteIndex-1)
		} else {
			m.paletteIndex++
		}
		return m, nil
	}
	if key == "enter" {
		value := strings.TrimSpace(m.input.Value())
		mode := m.inputMode
		switch mode {
		case "palette":
			cs := m.matchingCommands()
			if len(cs) == 0 {
				m.status = "No matching command"
				return m, nil
			}
			c := cs[min(m.paletteIndex, len(cs)-1)]
			m.closeInput()
			return m, m.executeCommand(c.name)
		case "namespace":
			ns := m.namespaceMatches(value)
			if len(ns) == 0 {
				if value == "" || m.readOnly {
					m.status = "No matching namespace"
					return m, nil
				}
				m.closeInput()
				return m, m.chooseNamespace(value)
			}
			a := ns[min(m.paletteIndex, len(ns)-1)]
			m.closeInput()
			return m, m.chooseNamespace(a.id)
		case "recall":
			if value == "" {
				return m, nil
			}
			m.closeInput()
			return m, m.inspectRecall(value)
		case "filter":
			m.closeInput()
			if m.activeTab == tabMemory {
				m.factQuery = value
				if id := m.selectedFactID(); id != "" {
					m.restoreFactID = id
				}
				setListItems(&m.factList, nil)
				m.previewID = ""
				return m, m.loadFacts(m.namespace)
			}
			m.filterLocal(value)
			m.syncPreview(true)
			return m, nil
		case "graph-neighbor":
			ns := m.neighborMatches(value)
			if len(ns) == 0 {
				m.status = "No matching connected entity"
				return m, nil
			}
			id := ns[min(m.paletteIndex, len(ns)-1)].n.ID
			m.closeInput()
			m.nodeList.ResetFilter()
			for i, it := range m.nodeList.Items() {
				if it.(nodeItem).n.ID == id {
					m.nodeList.Select(i)
				}
			}
			m.focus = 0
			m.syncPreview(true)
			return m, nil
		case "graph-source":
			ids := m.sourceMatches(value)
			if len(ids) == 0 {
				m.status = "This entity has no recorded supporting fact IDs"
				return m, nil
			}
			id := ids[min(m.paletteIndex, len(ids)-1)]
			m.closeInput()
			m.status = "Resolving supporting fact…"
			return m, m.resolveSource(id)
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.paletteIndex = 0
	return m, cmd
}
func (m *tuiModel) executeCommand(name string) tea.Cmd {
	for i, t := range []string{"memory", "recall", "activity", "graph", "usage", "status"} {
		if name == t {
			return m.switchTab(tabID(i))
		}
	}
	switch name {
	case "namespace":
		return m.openInput("namespace", "")
	case "filter":
		return m.openFilter()
	case "refresh":
		return m.refresh()
	case "redraw":
		return tea.ClearScreen
	case "copy":
		return m.copySelection()
	case "help":
		m.modal = "help"
	case "quit":
		return tea.Quit
	case "mouse":
		m.mouse = !m.mouse
		m.status = fmt.Sprintf("Mouse navigation: %t", m.mouse)
	case "theme dark", "theme light", "theme terminal":
		m.setTheme(strings.TrimPrefix(name, "theme "))
		m.syncPreview(false)
		m.status = "Theme: " + m.themeName
		return tea.ClearScreen
	case "add", "revise", "retire", "pin":
		if m.activeTab != tabMemory {
			m.status = "Open Memory to curate a fact"
			return nil
		}
		keys := map[string]string{"add": "a", "revise": "e", "retire": "d", "pin": "p"}
		return tea.Batch(m.updateMemoryKey(tea.KeyPressMsg{Code: []rune(keys[name])[0], Text: keys[name]})...)
	}
	return nil
}

func (m *tuiModel) openFilter() tea.Cmd {
	if m.activeTab == tabRecall {
		return m.openInput("recall", m.recallQuery)
	}
	if m.activeTab == tabMemory {
		return m.openInput("filter", m.factQuery)
	}
	if l := m.activeList(); l != nil {
		return m.openInput("filter", l.FilterValue())
	}
	m.status = "This view has no list filter. Use / to search commands."
	return nil
}

func (m *tuiModel) updateMemoryKey(msg tea.KeyPressMsg) []tea.Cmd {
	key := msg.String()
	if key == "r" {
		return []tea.Cmd{m.refresh()}
	}
	if key != "a" && key != "e" && key != "d" && key != "p" {
		return nil
	}
	if m.readOnly {
		m.status = "read-only: this session cannot change memory"
		return nil
	}
	if m.busy {
		m.status = "Wait for the current action to finish"
		return nil
	}
	if m.unknownMutation != "" {
		m.status = "Refresh and inspect the uncertain action before changing memory again."
		return nil
	}
	if m.namespace == "" {
		m.status = "Choose a namespace first (n)"
		return nil
	}
	if key == "a" {
		m.modal = "add"
		m.modalAgent = m.namespace
		m.editor.SetValue("")
		return []tea.Cmd{m.editor.Focus()}
	}
	sel, ok := m.factList.SelectedItem().(factItem)
	if !ok {
		return nil
	}
	m.modalFact = sel.fact
	m.modalAgent = sel.fact.AgentID
	if sel.fact.IsSuperseded() {
		m.status = "Select a live fact to revise or retire"
		return nil
	}
	switch key {
	case "d":
		m.modal = "retire"
	case "e":
		m.modal = "revise"
		m.editor.SetValue(sel.fact.Text)
		return []tea.Cmd{m.editor.Focus()}
	case "p":
		action := "pin"
		if sel.fact.Pinned {
			action = "unpin"
		}
		return []tea.Cmd{m.curate(action, "")}
	}
	return nil
}
func (m *tuiModel) updateSessionsKey(msg tea.KeyPressMsg) []tea.Cmd {
	if msg.String() == "r" {
		return []tea.Cmd{m.loadSessions()}
	}
	if msg.String() != "x" {
		return nil
	}
	if m.readOnly {
		m.status = "read-only: this session cannot stop runs"
		return nil
	}
	if m.busy {
		return nil
	}
	if m.unknownMutation != "" {
		m.status = "Refresh and inspect the uncertain action before stopping a run again."
		return nil
	}
	if s, ok := m.sessionList.SelectedItem().(sessionItem); ok {
		if s.s.Status != "running" {
			m.status = "Only running harness sessions can be stopped"
			return nil
		}
		m.modal = "kill"
		m.modalSession = s.s.ID
	}
	return nil
}
func (m *tuiModel) updateStatsKey(msg tea.KeyPressMsg) []tea.Cmd {
	if msg.String() == "r" {
		return []tea.Cmd{m.loadDashboard()}
	}
	return nil
}
func (m tuiModel) handleModalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "esc" && !m.busy {
		m.modal = ""
		m.editor.Blur()
		return m, nil
	}
	if m.modal == "help" {
		if k == "?" || k == "enter" {
			m.modal = ""
		}
		return m, nil
	}
	if m.busy {
		return m, nil
	}
	if m.modal == "add" || m.modal == "revise" {
		if k == "ctrl+s" {
			text := strings.TrimSpace(m.editor.Value())
			if text == "" {
				m.status = "A fact cannot be empty"
				return m, nil
			}
			if m.modal == "revise" {
				return m, m.curate("revise", text)
			}
			if m.readOnly {
				m.status = "read-only: cannot add memory"
				return m, nil
			}
			m.busy = true
			st, agent, parent := m.store, m.modalAgent, m.commandContext()
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(parent, 30*time.Second)
				defer cancel()
				err := st.Remember(ctx, agent, text)
				return mutationMsg{action: "Add", agent: agent, err: err}
			}
		}
		var c tea.Cmd
		m.editor, c = m.editor.Update(msg)
		return m, c
	}
	if k == "enter" {
		if m.modal == "retire" {
			return m, m.curate("retire", "")
		}
		if m.modal == "kill" {
			if m.readOnly {
				m.status = "read-only: cannot stop run"
				return m, nil
			}
			m.busy = true
			id, st := m.modalSession, m.store
			return m, func() tea.Msg { return mutationMsg{action: "Stop run", err: st.SessionKill(id)} }
		}
	}
	return m, nil
}
func (m *tuiModel) curate(action, text string) tea.Cmd {
	if m.readOnly {
		m.status = "read-only: cannot curate memory"
		return nil
	}
	if m.busy {
		return nil
	}
	p, ok := m.store.(curateFactStore)
	if !ok {
		m.status = "Safe curation unavailable; update the store daemon"
		return nil
	}
	m.busy = true
	f, parent := m.modalFact, m.commandContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		next, err := p.CurateFact(ctx, f.AgentID, f.ID, action, text, f)
		return mutationMsg{action: action, agent: f.AgentID, fact: next, err: err}
	}
}
func (m tuiModel) selectedText() string {
	switch m.activeTab {
	case tabMemory:
		if f, ok := m.factList.SelectedItem().(factItem); ok {
			return f.fact.Text
		}
	case tabRecall:
		if r, ok := m.recallList.SelectedItem().(receiptItem); ok {
			return r.receipt.Text
		}
	case tabGraph:
		if n, ok := m.nodeList.SelectedItem().(nodeItem); ok {
			return n.n.ID
		}
	}
	return m.previewContent()
}
func (m *tuiModel) copySelection() tea.Cmd {
	value := m.selectedText()
	if value == "" {
		m.status = "Nothing selected"
		return nil
	}
	return func() tea.Msg { return copyResultMsg{value, clipboard.WriteAll(value)} }
}
func (m tuiModel) namespaceMatches(q string) []agentItem {
	var out []agentItem
	for _, it := range m.agentList.Items() {
		a := it.(agentItem)
		if strings.Contains(strings.ToLower(a.id), strings.ToLower(q)) {
			out = append(out, a)
		}
	}
	return out
}
func (m *tuiModel) filterLocal(q string) {
	var l *list.Model
	switch m.activeTab {
	case tabSessions:
		if m.activityCheckpoints {
			l = &m.checkpointList
		} else {
			l = &m.sessionList
		}
	case tabGraph:
		l = &m.nodeList
	default:
		return
	}
	old := listIdentity(l.SelectedItem())
	l.SetFilteringEnabled(true)
	l.SetFilterText(q)
	l.SetFilterState(list.FilterApplied)
	selectVisible(l, func(item list.Item) bool { return old != "" && listIdentity(item) == old })
}
func setListItems(l *list.Model, items []list.Item) {
	old := listIdentity(l.SelectedItem())
	q := l.FilterValue()
	filtered := l.FilterState() != list.Unfiltered
	l.SetItems(items)
	// SetItems retains the old row cursor and schedules filtering. Reapply
	// even an empty active filter synchronously, then select by durable ID.
	if filtered {
		l.SetFilterText(q)
	}
	selectVisible(l, func(item list.Item) bool { return old != "" && listIdentity(item) == old })
}

func listIdentity(item list.Item) string {
	switch item := item.(type) {
	case agentItem:
		return item.id
	case factItem:
		return item.fact.ID
	case sessionItem:
		return item.s.ID
	case nodeItem:
		return item.n.ID
	case checkpointItem:
		return item.cp.ID
	case receiptItem:
		return item.receipt.Provenance.FactID
	}
	return ""
}

func (m tuiModel) neighborMatches(q string) []nodeItem {
	sel, ok := m.nodeList.SelectedItem().(nodeItem)
	if !ok {
		return nil
	}
	ids := map[string]bool{}
	for _, e := range m.graphEdges {
		if e.From == sel.n.ID {
			ids[e.To] = true
		}
		if e.To == sel.n.ID {
			ids[e.From] = true
		}
	}
	var out []nodeItem
	for _, it := range m.nodeList.Items() {
		n := it.(nodeItem)
		if ids[n.n.ID] && strings.Contains(strings.ToLower(n.n.Label), strings.ToLower(q)) {
			out = append(out, n)
		}
	}
	return out
}
func (m tuiModel) sourceMatches(q string) []string {
	sel, ok := m.nodeList.SelectedItem().(nodeItem)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, e := range m.graphEdges {
		if e.From == sel.n.ID || e.To == sel.n.ID {
			for _, id := range e.Sources {
				if !seen[id] && strings.Contains(strings.ToLower(id), strings.ToLower(q)) {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}
func (m tuiModel) resolveSource(id string) tea.Cmd {
	st, parent := m.store, m.commandContext()
	origin := ""
	if n, ok := m.nodeList.SelectedItem().(nodeItem); ok {
		origin = n.n.ID
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		defer cancel()
		agents, err := st.ListAgents()
		if err != nil {
			return sourceFactMsg{err: err, origin: origin}
		}
		for _, a := range agents {
			if p, ok := st.(factPageStore); ok {
				cursor := ""
				for {
					page, err := p.ListFacts(ctx, a, "all", "", cursor, 100)
					if err != nil {
						return sourceFactMsg{err: err, origin: origin}
					}
					for _, f := range page.Facts {
						if f.ID == id {
							return sourceFactMsg{fact: f, origin: origin}
						}
					}
					if page.NextCursor == "" {
						break
					}
					cursor = page.NextCursor
				}
				continue
			}
			facts, e := st.List(a)
			if e != nil {
				return sourceFactMsg{err: e, origin: origin}
			}
			for _, f := range facts {
				if f.ID == id {
					return sourceFactMsg{fact: f, origin: origin}
				}
			}
		}
		return sourceFactMsg{err: fmt.Errorf("Source %s no longer exists in this store", id), origin: origin}
	}
}

func (m *tuiModel) handleMouse(msg tea.MouseClickMsg) tea.Cmd {
	x, y := msg.X, msg.Y
	if msg.Button != tea.MouseLeft {
		return nil
	}
	if y == m.height-1 {
		return m.switchTab(tabUsage)
	}
	if y == tuiTabRow {
		pos := tuiTabInset
		for i := range tabNames {
			w := len([]rune(m.tabLabel(i)))
			if x >= pos && x < pos+w {
				return m.switchTab(tabID(i))
			}
			pos += w
		}
		return nil
	}
	if y < m.contentTop() || y >= m.height-tuiFooterRows {
		return nil
	}
	if m.width < 100 && m.focus == 1 {
		return nil
	}
	ns, lw, _ := m.columnWidths()
	rowHeight := m.listRowHeight()
	row := (y - m.contentTop()) / rowHeight
	if m.activeTab == tabMemory && ((ns > 0 && x < ns) || (m.width < 100 && m.memPane == memPaneAgents)) {
		m.memPane = memPaneAgents
		m.focus = 0
		selectClickedRow(&m.agentList, y-m.contentTop(), m.contentHeight(), 1)
		if s, ok := m.agentList.SelectedItem().(agentItem); ok {
			return m.chooseNamespace(s.id)
		}
		return nil
	}
	if m.width >= 100 && x >= ns+lw {
		m.focus = 1
		m.memPane = memPaneDetail
		return nil
	}
	m.focus = 0
	m.memPane = memPaneFacts
	l := m.activeList()
	if l != nil {
		if (y-m.contentTop())%rowHeight >= 2 {
			return nil // Whitespace between memories is not a selectable row.
		}
		selectClickedRow(l, row, m.contentHeight(), rowHeight)
		m.syncPreview(false)
	}
	return nil
}
func selectClickedRow(l *list.Model, row, height, rowHeight int) {
	count := len(l.VisibleItems())
	if count == 0 {
		return
	}
	perPage := max(1, height/rowHeight)
	if row < 0 || row >= perPage {
		return
	}
	start := (l.Index() / perPage) * perPage
	if start+row < count {
		l.Select(start + row)
	}
}
func selectVisible(l *list.Model, matches func(list.Item) bool) {
	l.Select(0)
	for i, item := range l.VisibleItems() {
		if matches(item) {
			l.Select(i)
			return
		}
	}
}
func (m *tuiModel) activeList() *list.Model {
	switch m.activeTab {
	case tabMemory:
		return &m.factList
	case tabRecall:
		return &m.recallList
	case tabSessions:
		if m.activityCheckpoints {
			return &m.checkpointList
		}
		return &m.sessionList
	case tabGraph:
		return &m.nodeList
	}
	return nil
}
