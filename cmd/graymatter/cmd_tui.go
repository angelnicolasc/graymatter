package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type tabID int

const (
	tabMemory tabID = iota
	tabRecall
	tabSessions
	tabGraph
	tabUsage
	tabStats
)

var tabNames = []string{"Memory", "Recall", "Activity", "Graph", "Usage", "Status"}

type agentItem struct {
	id    string
	count int
}

func (a agentItem) Title() string       { return tuiPlain(a.id) }
func (a agentItem) Description() string { return fmt.Sprintf("%d records", a.count) }
func (a agentItem) FilterValue() string { return a.id }

type factItem struct{ fact memory.Fact }

func (f factItem) Title() string {
	s := truncateRunes(strings.ReplaceAll(tuiPlain(f.fact.Text), "\n", " "), 180)
	if f.fact.Pinned {
		return "★ " + s
	}
	return s
}
func (f factItem) Description() string {
	return fmt.Sprintf("%s · %s · %s", factState(f.fact), confidenceLabel(f.fact), f.fact.CreatedAt.Format("Jan 02"))
}
func (f factItem) FilterValue() string { return f.fact.Text }
func factState(f memory.Fact) string {
	if f.IsSuperseded() {
		if f.SupersededBy == memory.SupersededByAgent {
			return "retired"
		}
		return "replaced"
	}
	if f.IsAlias() {
		return "alias"
	}
	if f.Pinned {
		return "pinned"
	}
	return "active"
}
func confidenceLabel(f memory.Fact) string {
	return memory.EffectiveConfidence(f.Confidence)
}

func confidenceDetail(raw string) string {
	if raw == "" {
		return "not declared (effective: inferred)"
	}
	if memory.ValidateConfidence(raw) == nil {
		return raw + " (writer-declared)"
	}
	return fmt.Sprintf("%q (unknown label; effective: unverified)", tuiPlain(raw))
}
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if n <= 0 {
		return ""
	}
	if len(r) <= n {
		return s
	}
	if n < 4 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

type sessionItem struct{ s harness.HarnessSession }

func (s sessionItem) Title() string { return tuiPlain(s.s.AgentFile) }
func (s sessionItem) Description() string {
	return statusStyle(s.s.Status) + " · " + s.s.StartedAt.Format("Jan 02 15:04")
}
func (s sessionItem) FilterValue() string { return s.s.ID + " " + s.s.AgentID + " " + s.s.AgentFile }
func statusStyle(s string) string {
	switch s {
	case "done", "success":
		return "✓ " + s
	case "failed", "killed":
		return "✗ " + s
	case "running":
		return "● running"
	default:
		return "○ " + tuiPlain(s)
	}
}

type nodeItem struct{ n kg.Node }

func (n nodeItem) Title() string { return tuiPlain(n.n.Label) }
func (n nodeItem) Description() string {
	return tuiPlain(n.n.EntityType) + " · " + n.n.LastSeen.Format("Jan 02")
}
func (n nodeItem) FilterValue() string { return n.n.Label + " " + n.n.EntityType }

type receiptItem struct {
	receipt memory.RecallReceipt
	rank    int
}

func (r receiptItem) Title() string {
	return fmt.Sprintf("%d  %s", r.rank, strings.ReplaceAll(tuiPlain(r.receipt.Text), "\n", " "))
}
func (r receiptItem) Description() string {
	if r.receipt.Ranking != nil {
		return fmt.Sprintf("Final %.5f · %s", r.receipt.Ranking.FinalScore, shortID(r.receipt.Provenance.FactID))
	}
	return fmt.Sprintf("RRF %.5f · %s", r.receipt.Ranks.FusedScore, shortID(r.receipt.Provenance.FactID))
}
func (r receiptItem) FilterValue() string { return r.receipt.Text }

type checkpointItem struct{ cp session.Checkpoint }

func (c checkpointItem) Title() string {
	return c.cp.CreatedAt.Format("Jan 02 15:04:05") + " · " + shortID(c.cp.ID)
}
func (c checkpointItem) Description() string {
	return fmt.Sprintf("%s · %d messages", tuiPlain(c.cp.AgentID), len(c.cp.Messages))
}
func (c checkpointItem) FilterValue() string { return c.cp.ID }

type memPane int

const (
	memPaneAgents memPane = iota
	memPaneFacts
	memPaneDetail
)

type resourceState struct {
	Loading    bool
	Updated    time.Time
	Err        error
	Generation uint64
}
type agentsLoadedMsg struct {
	agents     []agentItem
	generation uint64
}
type factsLoadedMsg struct {
	facts      []factItem
	agent      string
	generation uint64
	next       string
	total      int
	appendPage bool
}
type sessionsLoadedMsg struct {
	sessions   []sessionItem
	generation uint64
}
type nodesLoadedMsg struct {
	nodes      []nodeItem
	edges      int
	orphans    int
	relations  []kg.Edge
	generation uint64
}
type checkpointsLoadedMsg struct {
	agent       string
	generation  uint64
	checkpoints []session.Checkpoint
	err         error
}
type recallLoadedMsg struct {
	agent, query string
	generation   uint64
	receipts     []memory.RecallReceipt
	err          error
}
type mutationMsg struct {
	action, agent string
	fact          memory.Fact
	err           error
}
type errMsg struct{ err error }
type resourceErrMsg struct {
	resource   string
	generation uint64
	err        error
}
type statusMsg struct{ text string }
type healthLoadedMsg struct {
	health     memory.InspectionHealth
	err        error
	generation uint64
}
type sourceFactMsg struct {
	fact   memory.Fact
	err    error
	origin string
}

type inspectRecallStore interface {
	InspectRecall(context.Context, string, string, int) ([]memory.RecallReceipt, error)
}
type curateFactStore interface {
	CurateFact(context.Context, string, string, string, string, memory.Fact) (memory.Fact, error)
}
type factPageStore interface {
	ListFacts(context.Context, string, string, string, string, int) (memory.FactPage, error)
}

type tuiModel struct {
	ctx                                        context.Context
	startup                                    tea.Cmd
	demo                                       bool
	store                                      cliStore
	dataDir                                    string
	readOnly                                   bool
	activeTab                                  tabID
	width, height                              int
	layoutWidth, layoutHeight                  int
	layoutTab                                  tabID
	err                                        error // legacy/general connection error, independent resource errors live below
	status                                     string
	memPane                                    memPane
	agentList, factList, sessionList, nodeList list.Model
	detail                                     viewport.Model
	kgEdgeCount, kgOrphans                     int
	dashboard                                  dashboardData

	theme                      tuiTheme
	themeName                  string
	mouse                      bool
	resources                  map[string]resourceState
	namespace                  string
	factMode                   string
	factQuery                  string
	nextFactCursor             string
	factTotal                  int
	factsGeneration            uint64
	recallGeneration           uint64
	checkpointGeneration       uint64
	recallCancel               context.CancelFunc
	recallList, checkpointList list.Model
	recallQuery                string
	input                      textinput.Model
	editor                     textarea.Model
	inputMode                  string // palette, filter, recall, namespace, graph-source
	modal                      string // add, revise, retire, kill, help, lineage
	lineage                    tuiLineage
	modalFact                  memory.Fact
	modalAgent, modalSession   string
	busy                       bool
	unknownMutation            string // memory or activity; cleared only by a successful refresh
	unknownAgent               string
	restoreFactID              string
	paletteIndex               int
	focus                      int
	activityCheckpoints        bool
	graphEdges                 []kg.Edge
	previewID                  string
	previewText                string
	usage                      tuiUsageState
	health                     memory.InspectionHealth
}

func newTUIModel(store cliStore, dir string, readOnly bool, theme string, mouse bool) tuiModel {
	nl := func(title string) list.Model {
		d := list.NewDefaultDelegate()
		d.SetSpacing(0)
		if title == "Namespaces" {
			d.SetHeight(1)
		}
		l := list.New(nil, d, 40, 20)
		l.Title = title
		l.SetShowTitle(false)
		l.SetShowFilter(false)
		l.SetShowStatusBar(false)
		l.SetShowHelp(false)
		l.SetShowPagination(false)
		l.SetFilteringEnabled(false)
		l.Filter = func(q string, targets []string) []list.Rank {
			var ranks []list.Rank
			for i, s := range targets {
				if strings.Contains(strings.ToLower(s), strings.ToLower(q)) {
					ranks = append(ranks, list.Rank{Index: i})
				}
			}
			return ranks
		}
		l.DisableQuitKeybindings()
		return l
	}
	m := tuiModel{store: store, dataDir: dir, readOnly: readOnly, themeName: theme, mouse: mouse, width: 100, height: 30, factMode: "active", resources: map[string]resourceState{}, agentList: nl("Namespaces"), factList: nl("Memory"), sessionList: nl("Runs"), nodeList: nl("Entities"), recallList: nl("Results"), checkpointList: nl("Checkpoints"), detail: viewport.New(), input: textinput.New(), editor: textarea.New()}
	m.detail.SoftWrap = true
	m.memPane = memPaneFacts
	m.input.Prompt = "› "
	m.input.CharLimit = 4096
	m.editor.CharLimit = 200000
	m.editor.ShowLineNumbers = false
	m.setTheme(theme)
	m.initUsage()
	if d, ok := store.(interface{ Demo() bool }); ok && d.Demo() {
		m.demo = true
		m.usage.stateDir = dir
	}
	m.updateSizes()
	return m
}
func (m tuiModel) Init() tea.Cmd {
	if m.startup != nil {
		return m.startup
	}
	return tea.Batch(m.loadAgents(), dashboardTick())
}
func (m tuiModel) commandContext() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}
func (m *tuiModel) loadHealth() tea.Cmd {
	g := m.startRequest("provider")
	st, parent := m.store, m.commandContext()
	return func() tea.Msg {
		p, ok := st.(interface {
			InspectHealth(context.Context) (memory.InspectionHealth, error)
		})
		if !ok {
			return healthLoadedMsg{err: memory.ErrInspectionUnsupported, generation: g}
		}
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		defer cancel()
		h, err := p.InspectHealth(ctx)
		return healthLoadedMsg{h, err, g}
	}
}
func (m *tuiModel) state(name string) resourceState {
	if m.resources == nil {
		m.resources = map[string]resourceState{}
	}
	return m.resources[name]
}
func (m *tuiModel) loading(name string) { r := m.state(name); r.Loading = true; m.resources[name] = r }
func (m *tuiModel) startRequest(name string) uint64 {
	r := m.state(name)
	r.Loading = true
	r.Generation++
	m.resources[name] = r
	return r.Generation
}
func (m *tuiModel) loaded(name string, err error) {
	r := m.state(name)
	r.Loading = false
	r.Err = err
	if err == nil {
		r.Updated = time.Now()
	}
	m.resources[name] = r
}

func (m *tuiModel) loadAgents() tea.Cmd {
	g := m.startRequest("namespaces")
	st := m.store
	return func() tea.Msg {
		if st == nil {
			return agentsLoadedMsg{generation: g}
		}
		ids, err := st.ListAgents()
		if err != nil {
			return resourceErrMsg{"namespaces", g, err}
		}
		items := make([]agentItem, 0, len(ids))
		for _, id := range ids {
			s, e := st.Stats(id)
			if e != nil {
				return resourceErrMsg{"namespaces", g, e}
			}
			items = append(items, agentItem{id, s.FactCount})
		}
		return agentsLoadedMsg{items, g}
	}
}
func (m *tuiModel) loadFacts(agent string) tea.Cmd { return m.loadFactPage(agent, "") }
func (m *tuiModel) loadFactPage(agent, cursor string) tea.Cmd {
	m.factsGeneration++
	g := m.factsGeneration
	m.loading("memory")
	st, parent := m.store, m.commandContext()
	mode, query := m.factMode, m.factQuery
	if mode == "" {
		mode = "active"
	}
	return func() tea.Msg {
		var facts []memory.Fact
		var err error
		var next string
		total := 0
		if st == nil {
			return factsLoadedMsg{agent: agent, generation: g}
		}
		if p, ok := st.(factPageStore); ok {
			ctx, cancel := context.WithTimeout(parent, 15*time.Second)
			defer cancel()
			var page memory.FactPage
			page, err = p.ListFacts(ctx, agent, mode, query, cursor, 100)
			facts, next, total = page.Facts, page.NextCursor, page.Total
		} else {
			facts, err = st.List(agent)
			if err == nil {
				filtered := facts[:0]
				for _, f := range facts {
					if matchFact(f, mode, query) {
						filtered = append(filtered, f)
					}
				}
				facts = filtered
				total = len(facts)
			}
		}
		if err != nil {
			return resourceErrMsg{"memory", g, err}
		}
		items := make([]factItem, len(facts))
		for i, f := range facts {
			items[i] = factItem{f}
		}
		return factsLoadedMsg{facts: items, agent: agent, generation: g, next: next, total: total, appendPage: cursor != ""}
	}
}
func matchFact(f memory.Fact, mode, query string) bool {
	switch mode {
	case "active":
		if f.IsSuperseded() || f.IsAlias() {
			return false
		}
	case "retired":
		if !f.IsSuperseded() {
			return false
		}
	case "alias":
		if !f.IsAlias() || f.IsSuperseded() {
			return false
		}
	}
	return strings.Contains(strings.ToLower(f.Text), strings.ToLower(query))
}
func (m *tuiModel) loadSessions() tea.Cmd {
	g := m.startRequest("activity")
	st := m.store
	return func() tea.Msg {
		if st == nil {
			return sessionsLoadedMsg{generation: g}
		}
		ss, err := st.SessionsList()
		if err != nil {
			return resourceErrMsg{"activity", g, err}
		}
		items := make([]sessionItem, len(ss))
		for i, s := range ss {
			items[i] = sessionItem{s}
		}
		return sessionsLoadedMsg{items, g}
	}
}
func (m *tuiModel) loadNodes() tea.Cmd {
	g := m.startRequest("graph")
	st := m.store
	return func() tea.Msg {
		if st == nil {
			return nodesLoadedMsg{generation: g}
		}
		ns, e := st.KGNodes()
		if e != nil {
			return resourceErrMsg{"graph", g, e}
		}
		es, e := st.KGEdges()
		if e != nil {
			return resourceErrMsg{"graph", g, e}
		}
		degree := map[string]int{}
		for _, e := range es {
			degree[e.From]++
			degree[e.To]++
		}
		items := make([]nodeItem, len(ns))
		orphans := 0
		for i, n := range ns {
			items[i] = nodeItem{n}
			if degree[n.ID] == 0 {
				orphans++
			}
		}
		return nodesLoadedMsg{nodes: items, edges: len(es), orphans: orphans, relations: es, generation: g}
	}
}
func (m *tuiModel) loadCheckpoints() tea.Cmd {
	m.checkpointGeneration++
	g, agent, st := m.checkpointGeneration, m.namespace, m.store
	m.loading("checkpoints")
	return func() tea.Msg {
		if st == nil || agent == "" {
			return checkpointsLoadedMsg{agent: agent, generation: g}
		}
		c, e := st.CheckpointList(agent)
		return checkpointsLoadedMsg{agent, g, c, e}
	}
}
func (m *tuiModel) inspectRecall(query string) tea.Cmd {
	if m.namespace == "" {
		m.status = "Choose a namespace first (n)."
		return nil
	}
	if m.recallCancel != nil {
		m.recallCancel()
	}
	ctx, cancel := context.WithTimeout(m.commandContext(), 30*time.Second)
	m.recallCancel = cancel
	m.recallGeneration++
	g, agent, st := m.recallGeneration, m.namespace, m.store
	m.recallQuery = query
	m.loading("recall")
	return func() tea.Msg {
		defer cancel()
		p, ok := st.(inspectRecallStore)
		if !ok {
			return recallLoadedMsg{agent: agent, query: query, generation: g, err: fmt.Errorf("safe inspection unavailable; update the store daemon")}
		}
		rs, e := p.InspectRecall(ctx, agent, query, 8)
		return recallLoadedMsg{agent, query, g, rs, e}
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if _, loaded := msg.(tuiUsageLoadedMsg); loaded {
		cmds = append(cmds, m.updateUsage(msg))
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		changed := m.width != msg.Width || m.height != msg.Height
		m.width, m.height = msg.Width, msg.Height
		if m.modal == "lineage" {
			m.sizeLineageView()
		}
		m.updateSizes()
		m.syncPreview(false)
		if changed {
			// Some terminal bridges retain cells from the old dimensions.
			// Invalidate the renderer once per resize, never on regular frames.
			cmds = append(cmds, tea.ClearScreen)
		}
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		if m.modal == "add" || m.modal == "revise" {
			var c tea.Cmd
			m.editor, c = m.editor.Update(msg)
			cmds = append(cmds, c)
		} else if m.inputMode != "" {
			var c tea.Cmd
			m.input, c = m.input.Update(msg)
			cmds = append(cmds, c)
		}
	case tea.MouseClickMsg:
		if m.mouse && m.modal == "" && m.inputMode == "" {
			cmds = append(cmds, m.handleMouse(msg))
		}
	case tea.MouseWheelMsg:
		if m.mouse && m.modal == "lineage" {
			var cmd tea.Cmd
			m.lineage.view, cmd = m.lineage.view.Update(msg)
			return m, cmd
		}
		if m.mouse && m.modal == "" && m.inputMode == "" {
			if m.activeTab == tabUsage {
				return m, m.updateUsage(msg)
			}
			if m.focus == 1 || m.memPane == memPaneDetail {
				var c tea.Cmd
				m.detail, c = m.detail.Update(msg)
				cmds = append(cmds, c)
			} else {
				cmds = append(cmds, m.moveList(msg))
				if m.activeTab == tabMemory && m.memPane == memPaneAgents {
					if s, ok := m.agentList.SelectedItem().(agentItem); ok && s.id != m.namespace {
						cmds = append(cmds, m.chooseNamespace(s.id))
					}
				}
			}
			m.syncPreview(false)
		}
	case agentsLoadedMsg:
		if msg.generation < m.state("namespaces").Generation {
			break
		}
		m.err = nil
		m.loaded("namespaces", nil)
		old := m.namespace
		items := make([]list.Item, len(msg.agents))
		for i, a := range msg.agents {
			items[i] = a
		}
		setListItems(&m.agentList, items)
		for i, a := range msg.agents {
			if a.id == old {
				m.agentList.Select(i)
			}
		}
		if sel, ok := m.agentList.SelectedItem().(agentItem); ok {
			if m.namespace == "" {
				// Prefer a project namespace over shared defaults on first open.
				for i, a := range msg.agents {
					if a.id != "__shared__" {
						m.agentList.Select(i)
						sel = a
						break
					}
				}
				cmds = append(cmds, m.chooseNamespace(sel.id))
			}
		}
		if old != "" && !m.state("memory").Loading && m.state("memory").Updated.IsZero() {
			cmds = append(cmds, m.chooseNamespace(old))
		}
	case factsLoadedMsg:
		if msg.generation < m.factsGeneration || (m.namespace != "" && msg.agent != "" && msg.agent != m.namespace) {
			break
		}
		m.factsGeneration = msg.generation
		m.loaded("memory", nil)
		old := m.selectedFactID()
		if old == "" {
			old = m.restoreFactID
		}
		items := []list.Item{}
		if msg.appendPage {
			items = append(items, m.factList.Items()...)
		}
		for _, f := range msg.facts {
			items = append(items, f)
		}
		setListItems(&m.factList, items)
		for i, item := range items {
			if item.(factItem).fact.ID == old {
				m.factList.Select(i)
			}
		}
		m.nextFactCursor = msg.next
		m.factTotal = msg.total
		m.restoreFactID = ""
		if m.unknownMutation == "memory" && msg.agent == m.unknownAgent {
			m.unknownMutation = ""
			m.status = "Refreshed. Inspect the result before deciding whether to repeat the action."
		}
		m.syncPreview(false)
	case sessionsLoadedMsg:
		if msg.generation < m.state("activity").Generation {
			break
		}
		m.loaded("activity", nil)
		if m.unknownMutation == "activity" {
			m.unknownMutation = ""
			m.status = "Refreshed. Inspect the run before deciding whether to stop it again."
		}
		old := ""
		if s, ok := m.sessionList.SelectedItem().(sessionItem); ok {
			old = s.s.ID
		}
		items := make([]list.Item, len(msg.sessions))
		for i, s := range msg.sessions {
			items[i] = s
		}
		setListItems(&m.sessionList, items)
		selectVisible(&m.sessionList, func(it list.Item) bool { return it.(sessionItem).s.ID == old })
		m.syncPreview(false)
	case nodesLoadedMsg:
		if msg.generation < m.state("graph").Generation {
			break
		}
		m.loaded("graph", nil)
		old := ""
		if n, ok := m.nodeList.SelectedItem().(nodeItem); ok {
			old = n.n.ID
		}
		items := make([]list.Item, len(msg.nodes))
		for i, n := range msg.nodes {
			items[i] = n
		}
		setListItems(&m.nodeList, items)
		selectVisible(&m.nodeList, func(it list.Item) bool { return it.(nodeItem).n.ID == old })
		m.graphEdges = msg.relations
		m.kgEdgeCount, m.kgOrphans = msg.edges, msg.orphans
		m.syncPreview(false)
	case checkpointsLoadedMsg:
		if msg.generation != m.checkpointGeneration || msg.agent != m.namespace {
			break
		}
		m.loaded("checkpoints", msg.err)
		if msg.err == nil {
			items := make([]list.Item, len(msg.checkpoints))
			for i, c := range msg.checkpoints {
				items[i] = checkpointItem{c}
			}
			setListItems(&m.checkpointList, items)
			m.syncPreview(false)
		}
	case lineageLoadedMsg:
		m.lineageLoaded(msg)
	case recallLoadedMsg:
		if msg.generation != m.recallGeneration || msg.agent != m.namespace {
			break
		}
		m.loaded("recall", msg.err)
		if msg.err == nil {
			items := make([]list.Item, len(msg.receipts))
			for i, r := range msg.receipts {
				items[i] = receiptItem{r, i + 1}
			}
			setListItems(&m.recallList, items)
			m.focus = 0
			m.syncPreview(true)
		}
	case resourceErrMsg:
		if msg.resource == "memory" && msg.generation < m.factsGeneration {
			break
		}
		if msg.generation < m.state(msg.resource).Generation {
			break
		}
		m.loaded(msg.resource, msg.err)
		if msg.resource == "namespaces" {
			m.err = msg.err
		}
	case healthLoadedMsg:
		if msg.generation < m.state("provider").Generation {
			break
		}
		m.loaded("provider", msg.err)
		if msg.err == nil {
			m.health = msg.health
		}
		m.syncPreview(false)
	case errMsg:
		m.err = msg.err
		m.loaded("namespaces", msg.err)
	case statusMsg:
		m.status = msg.text
	case copyResultMsg:
		if msg.err == nil {
			m.status = "Copied to system clipboard"
		} else {
			m.status = "Terminal copy requested (OSC52); use the inspector if unsupported"
			cmds = append(cmds, tea.SetClipboard(msg.text))
		}
	case mutationMsg:
		m.busy = false
		knownRevision := msg.action == "revise" && msg.fact.ID != ""
		if msg.err != nil && !knownRevision {
			m.status = "Action failed: " + tuiPlain(msg.err.Error())
			if errors.Is(msg.err, memory.ErrMutationOutcomeUnknown) {
				m.modal = ""
				m.unknownMutation, m.unknownAgent = "memory", msg.agent
				if msg.action == "Stop run" {
					m.unknownMutation = "activity"
				}
				m.status = "Outcome unknown. Refresh and inspect the result before another action."
				if msg.agent != "" {
					m.status = "Outcome unknown in " + tuiPlain(msg.agent) + ". Refresh that namespace and inspect before another action."
				}
			}
		} else {
			m.modal = ""
			m.status = msg.action + " confirmed"
			if msg.err != nil {
				m.status = "Revision committed as " + tuiPlain(msg.fact.ID) + ". Follow-up error: " + tuiPlain(msg.err.Error())
			}
			if msg.agent == m.namespace {
				cmds = append(cmds, m.loadFacts(m.namespace))
			}
			cmds = append(cmds, m.loadAgents())
			if msg.action == "Stop run" {
				cmds = append(cmds, m.loadSessions())
			}
		}
	case sourceFactMsg:
		if msg.origin != "" {
			n, ok := m.nodeList.SelectedItem().(nodeItem)
			if m.activeTab != tabGraph || !ok || n.n.ID != msg.origin {
				break
			}
		}
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			if m.activeTab != tabMemory {
				cmds = append(cmds, tea.ClearScreen)
			}
			m.activeTab = tabMemory
			m.recallGeneration++
			m.checkpointGeneration++
			setListItems(&m.recallList, nil)
			setListItems(&m.checkpointList, nil)
			m.namespace = msg.fact.AgentID
			m.factMode = "all"
			m.factQuery = ""
			setListItems(&m.factList, []list.Item{factItem{msg.fact}})
			m.factsGeneration++
			m.factTotal, m.nextFactCursor = 1, ""
			m.loaded("memory", nil)
			selectVisible(&m.agentList, func(it list.Item) bool { return it.(agentItem).id == msg.fact.AgentID })
			m.memPane = memPaneDetail
			m.focus = 1
			m.syncPreview(true)
			m.status = "Source fact · " + msg.fact.ID
		}
	case dashboardLoadedMsg:
		if msg.generation < m.state("status").Generation {
			break
		}
		if msg.data.Err != nil && m.dashboard.Loaded && m.dashboard.Err == nil {
			m.loaded("status", msg.data.Err)
		} else {
			m.dashboard = msg.data
			m.loaded("status", msg.data.Err)
		}
		m.syncPreview(false)
	case dashboardTickMsg:
		if m.activeTab == tabStats && !m.state("status").Loading {
			m.loading("status")
			cmds = append(cmds, m.loadDashboard())
		}
		cmds = append(cmds, dashboardTick())
	default:
		if m.modal == "add" || m.modal == "revise" {
			var c tea.Cmd
			m.editor, c = m.editor.Update(msg)
			cmds = append(cmds, c)
		} else if m.inputMode != "" {
			var c tea.Cmd
			m.input, c = m.input.Update(msg)
			cmds = append(cmds, c)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *tuiModel) chooseNamespace(agent string) tea.Cmd {
	if m.namespace != agent {
		setListItems(&m.factList, nil)
		setListItems(&m.recallList, nil)
		setListItems(&m.checkpointList, nil)
		m.detail.SetContent("")
		m.previewText = ""
		m.nextFactCursor, m.factTotal = "", 0
		m.restoreFactID = ""
		for _, resource := range []string{"memory", "recall", "checkpoints"} {
			m.resources[resource] = resourceState{}
		}
		m.recallGeneration++
		if m.recallCancel != nil {
			m.recallCancel()
		}
	}
	m.namespace = agent
	selectVisible(&m.agentList, func(it list.Item) bool { return it.(agentItem).id == agent })
	m.previewID = ""
	if m.activeTab == tabSessions {
		return tea.Batch(m.loadFacts(agent), m.loadCheckpoints())
	}
	return m.loadFacts(agent)
}
func (m *tuiModel) switchTab(t tabID) tea.Cmd {
	changed := m.activeTab != t
	m.activeTab = t
	m.focus = 0
	m.memPane = memPaneFacts
	m.previewID = ""
	m.syncPreview(true)
	var load tea.Cmd
	switch t {
	case tabMemory:
		if m.namespace != "" && !m.state("memory").Loading {
			load = m.loadFacts(m.namespace)
		}
	case tabSessions:
		load = tea.Batch(m.loadSessions(), m.loadCheckpoints())
	case tabGraph:
		load = m.loadNodes()
	case tabUsage:
		load = m.loadUsage()
	case tabStats:
		m.loading("status")
		load = tea.Batch(m.loadDashboard(), m.loadHealth())
	}
	if changed {
		// Different views use different panel geometry. Clear retained cells
		// once before dispatching loads, including their concurrent batches.
		return tea.Sequence(tea.ClearScreen, load)
	}
	return load
}
func (m *tuiModel) refresh() tea.Cmd {
	switch m.activeTab {
	case tabRecall:
		if strings.TrimSpace(m.recallQuery) == "" {
			m.status = "Enter a query first (Enter)."
			return nil
		}
		return m.inspectRecall(m.recallQuery)
	case tabMemory:
		return tea.Batch(m.loadAgents(), m.loadFacts(m.namespace))
	case tabSessions:
		return tea.Batch(m.loadSessions(), m.loadCheckpoints())
	case tabGraph:
		return m.loadNodes()
	case tabStats:
		m.loading("status")
		return tea.Batch(m.loadDashboard(), m.loadHealth())
	case tabUsage:
		return m.refreshUsage()
	}
	return nil
}
func (m tuiModel) selectedFactID() string {
	if f, ok := m.factList.SelectedItem().(factItem); ok {
		return f.fact.ID
	}
	return ""
}
func shortID(s string) string { return truncateRunes(s, 14) }
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func tuiCmd() *cobra.Command {
	var readOnly, mouse bool
	var theme string
	cmd := &cobra.Command{Use: "tui", Short: "Explore, inspect and curate agent memory", Long: "Interactive memory workbench: Memory, Recall, Activity, Graph, Usage and Status.\nUse / or Ctrl+K for commands, f to filter, n to choose a namespace.\nRecall inspection does not touch access counts. Retirement leaves a tombstone that ordinary pruning can remove.", RunE: func(cmd *cobra.Command, args []string) error {
		if theme != "" && theme != "dark" && theme != "light" && theme != "terminal" {
			return fmt.Errorf("theme must be dark, light, or terminal")
		}
		store, err := openTUIStore(readOnly)
		if err != nil {
			return err
		}
		defer store.Close()
		return runTUI(cmd.Context(), store, dataDir, readOnly || store.IsReadOnly(), theme, mouse)
	}}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "disable mutations for this TUI session")
	cmd.Flags().BoolVar(&mouse, "mouse", false, "enable mouse navigation (off preserves terminal selection)")
	cmd.Flags().StringVar(&theme, "theme", "", "dark, light, or terminal (default: saved preference, then dark)")
	return cmd
}

// runTUI borrows the store; the caller retains ownership and closes it.
func runTUI(parent context.Context, store cliStore, dir string, readOnly bool, theme string, mouse bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	m := newTUIModel(store, dir, readOnly, theme, mouse)
	m.ctx = ctx
	prefsPath := ""
	if !m.demo {
		prefsPath = tuiPreferencesPath(dir)
		m.restorePreferences(prefsPath, theme)
	}
	m.startup = tea.Batch(m.loadAgents(), m.loadUsage(), dashboardTick())
	result, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err == nil && prefsPath != "" {
		if final, ok := result.(tuiModel); ok {
			err = final.savePreferences(prefsPath)
		}
	}
	return err
}
