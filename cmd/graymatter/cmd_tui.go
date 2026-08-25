package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Styles (palette, border styles, dashboard helpers) live in tui_styles.go.
// Dashboard data loading + panel rendering live in tui_dashboard.go.

// ── Tab definitions ───────────────────────────────────────────────────────────

type tabID int

const (
	tabMemory   tabID = iota // 3-pane memory browser
	tabSessions              // harness sessions list
	tabGraph                 // knowledge graph nodes
	tabStats                 // observability dashboard
)

var tabNames = []string{"Memory", "Sessions", "Graph", "Stats"}

// ── List item types ───────────────────────────────────────────────────────────

// --- Memory tab ---

type agentItem struct {
	id    string
	count int
}

func (a agentItem) Title() string       { return a.id }
func (a agentItem) Description() string { return fmt.Sprintf("%d facts", a.count) }
func (a agentItem) FilterValue() string { return a.id }

type factItem struct{ fact memory.Fact }

func (f factItem) Title() string {
	preview := truncateRunes(f.fact.Text, 72)
	if f.fact.Pinned {
		return "★ " + preview
	}
	return preview
}
func (f factItem) Description() string {
	if f.fact.Pinned {
		return fmt.Sprintf("pinned · weight %.3f · %s", f.fact.Weight, f.fact.CreatedAt.Format("2006-01-02"))
	}
	return fmt.Sprintf("weight %.3f · %s", f.fact.Weight, f.fact.CreatedAt.Format("2006-01-02"))
}
func (f factItem) FilterValue() string { return f.fact.Text }

// truncateRunes shortens s to at most max runes without splitting multi-byte
// characters.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}

// --- Sessions tab ---

type sessionItem struct{ s harness.HarnessSession }

func (s sessionItem) Title() string {
	age := ""
	if !s.s.StartedAt.IsZero() {
		age = " · " + time.Since(s.s.StartedAt).Truncate(time.Second).String() + " ago"
	}
	return s.s.AgentFile + age
}
func (s sessionItem) Description() string {
	st := statusStyle(s.s.Status)
	return fmt.Sprintf("%s  %s", st, s.s.ID)
}
func (s sessionItem) FilterValue() string { return s.s.ID + s.s.AgentFile }

func statusStyle(status string) string {
	switch status {
	case "running":
		return styleStatusOK.Render("● running")
	case "success", "done": // the runner writes "done" for completed runs
		return styleStatusOK.Render("✓ " + status)
	case "failed", "killed":
		return styleStatusFail.Render("✗ " + status)
	default:
		return styleStatusPending.Render("○ " + status)
	}
}

// --- Graph tab ---

type nodeItem struct{ n kg.Node }

func (n nodeItem) Title() string { return n.n.Label + " [" + n.n.EntityType + "]" }
func (n nodeItem) Description() string {
	return fmt.Sprintf("id: %s · weight %.3f · seen %s",
		n.n.ID[:min(12, len(n.n.ID))], n.n.Weight,
		n.n.LastSeen.Format("2006-01-02"))
}
func (n nodeItem) FilterValue() string { return n.n.Label + " " + n.n.EntityType }

// ── Messages ──────────────────────────────────────────────────────────────────

type agentsLoadedMsg struct{ agents []agentItem }
type factsLoadedMsg struct{ facts []factItem }
type sessionsLoadedMsg struct{ sessions []sessionItem }
type nodesLoadedMsg struct {
	nodes   []nodeItem
	edges   int
	orphans int
}
type errMsg struct{ err error }
type statusMsg struct{ text string }

// ── Pane within memory tab ────────────────────────────────────────────────────

type memPane int

const (
	memPaneAgents memPane = iota
	memPaneFacts
	memPaneDetail
)

// ── Main model ────────────────────────────────────────────────────────────────

type tuiModel struct {
	store    cliStore
	dataDir  string
	readOnly bool // true when the store is in read-only mode (--no-daemon only)

	// layout
	activeTab tabID
	width     int
	height    int
	err       error
	status    string

	// memory tab
	memPane   memPane
	agentList list.Model
	factList  list.Model
	detail    viewport.Model

	// sessions tab
	sessionList list.Model

	// graph tab
	nodeList    list.Model
	nodeDetail  viewport.Model
	kgEdgeCount int
	kgOrphans   int

	// stats tab (observability dashboard)
	dashboard dashboardData
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		m.loadAgents(),
		m.loadSessions(),
		m.loadNodes(),
		m.loadDashboard(),
		dashboardTick(),
	)
}

// ── Loaders ───────────────────────────────────────────────────────────────────

func (m tuiModel) loadAgents() tea.Cmd {
	return func() tea.Msg {
		agents, err := m.store.ListAgents()
		if err != nil {
			return errMsg{err}
		}
		items := make([]agentItem, 0, len(agents))
		for _, a := range agents {
			// The agent came from ListAgents, so a failure here is the store
			// going away rather than a missing agent. Dropping the error left
			// the agent listed with a count of 0, which reads as "this agent
			// remembers nothing".
			st, err := m.store.Stats(a)
			if err != nil {
				return errMsg{err}
			}
			items = append(items, agentItem{id: a, count: st.FactCount})
		}
		return agentsLoadedMsg{items}
	}
}

func (m tuiModel) loadFacts(agentID string) tea.Cmd {
	return func() tea.Msg {
		facts, err := m.store.List(agentID)
		if err != nil {
			return errMsg{err}
		}
		items := make([]factItem, len(facts))
		for i, f := range facts {
			items[i] = factItem{f}
		}
		return factsLoadedMsg{items}
	}
}

func (m tuiModel) loadSessions() tea.Cmd {
	return func() tea.Msg {
		sessions, err := m.store.SessionsList()
		if err != nil {
			// Was swallowed as "non-fatal", which rendered an empty list: the
			// same picture a project with no sessions gives. An empty store
			// does not error here, so reaching this really is a failure.
			return errMsg{err}
		}
		items := make([]sessionItem, len(sessions))
		for i, s := range sessions {
			items[i] = sessionItem{s}
		}
		return sessionsLoadedMsg{items}
	}
}

func (m tuiModel) loadNodes() tea.Cmd {
	return func() tea.Msg {
		nodes, err := m.store.KGNodes()
		if err != nil {
			return errMsg{err}
		}
		edges, _ := m.store.KGEdges()
		degree := map[string]int{}
		for _, e := range edges {
			degree[e.From]++
			degree[e.To]++
		}
		orphans := 0
		for _, n := range nodes {
			if degree[n.ID] == 0 {
				orphans++
			}
		}
		items := make([]nodeItem, len(nodes))
		for i, n := range nodes {
			items[i] = nodeItem{n}
		}
		return nodesLoadedMsg{nodes: items, edges: len(edges), orphans: orphans}
	}
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		// Tab switching.
		case "1":
			m.activeTab = tabMemory
		case "2":
			m.activeTab = tabSessions
		case "3":
			m.activeTab = tabGraph
		case "4":
			m.activeTab = tabStats
		case "tab":
			m.activeTab = (m.activeTab + 1) % tabID(len(tabNames))
		case "shift+tab":
			m.activeTab = (m.activeTab + tabID(len(tabNames)) - 1) % tabID(len(tabNames))

		default:
			// Per-tab key handling.
			switch m.activeTab {
			case tabMemory:
				cmds = append(cmds, m.updateMemoryKey(msg)...)
			case tabSessions:
				cmds = append(cmds, m.updateSessionsKey(msg)...)
			case tabStats:
				cmds = append(cmds, m.updateStatsKey(msg)...)
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateSizes()

	case agentsLoadedMsg:
		// A load that got through means whatever went wrong before is over.
		// Without this the error would outlive its cause: nothing else clears
		// it, so one hiccup used to leave the UI stuck on an error screen for
		// the rest of the session.
		m.err = nil
		items := make([]list.Item, len(msg.agents))
		for i, a := range msg.agents {
			items[i] = a
		}
		m.agentList.SetItems(items)

	case factsLoadedMsg:
		m.err = nil
		items := make([]list.Item, len(msg.facts))
		for i, f := range msg.facts {
			items[i] = f
		}
		m.factList.SetItems(items)

	case sessionsLoadedMsg:
		items := make([]list.Item, len(msg.sessions))
		for i, s := range msg.sessions {
			items[i] = s
		}
		m.sessionList.SetItems(items)

	case nodesLoadedMsg:
		items := make([]list.Item, len(msg.nodes))
		for i, n := range msg.nodes {
			items[i] = n
		}
		m.nodeList.SetItems(items)
		m.kgEdgeCount = msg.edges
		m.kgOrphans = msg.orphans
		if sel, ok := m.nodeList.SelectedItem().(nodeItem); ok {
			m.nodeDetail.SetContent(formatNodeDetail(sel.n))
		}

	case dashboardLoadedMsg:
		m.dashboard = msg.data
		if msg.data.Err == nil {
			m.err = nil
		}

	case dashboardTickMsg:
		// Refresh the dashboard periodically and re-arm the ticker.
		cmds = append(cmds, m.loadDashboard(), dashboardTick())

	case errMsg:
		m.err = msg.err

	case statusMsg:
		m.status = msg.text
	}

	// Delegate remaining events to active tab's focused widget.
	switch m.activeTab {
	case tabMemory:
		var cmd tea.Cmd
		switch m.memPane {
		case memPaneAgents:
			m.agentList, cmd = m.agentList.Update(msg)
		case memPaneFacts:
			m.factList, cmd = m.factList.Update(msg)
		case memPaneDetail:
			m.detail, cmd = m.detail.Update(msg)
		}
		cmds = append(cmds, cmd)
	case tabSessions:
		var cmd tea.Cmd
		m.sessionList, cmd = m.sessionList.Update(msg)
		cmds = append(cmds, cmd)
	case tabGraph:
		var cmd1, cmd2 tea.Cmd
		m.nodeList, cmd1 = m.nodeList.Update(msg)
		m.nodeDetail, cmd2 = m.nodeDetail.Update(msg)
		// Keep the detail pane in sync with the highlighted node.
		if _, isKey := msg.(tea.KeyMsg); isKey {
			if sel, ok := m.nodeList.SelectedItem().(nodeItem); ok {
				m.nodeDetail.SetContent(formatNodeDetail(sel.n))
			}
		}
		cmds = append(cmds, cmd1, cmd2)
	}

	return m, tea.Batch(cmds...)
}

func (m *tuiModel) updateMemoryKey(msg tea.KeyMsg) []tea.Cmd {
	switch msg.String() {
	case "right", "l":
		if m.memPane < memPaneDetail {
			m.memPane++
		}
	case "left", "h":
		if m.memPane > memPaneAgents {
			m.memPane--
		}
	case "enter":
		if m.memPane == memPaneAgents {
			if sel, ok := m.agentList.SelectedItem().(agentItem); ok {
				m.memPane = memPaneFacts
				return []tea.Cmd{m.loadFacts(sel.id)}
			}
		} else if m.memPane == memPaneFacts {
			m.memPane = memPaneDetail
			if sel, ok := m.factList.SelectedItem().(factItem); ok {
				m.detail.SetContent(formatFactDetail(sel.fact))
			}
		}
	case "d":
		if m.readOnly {
			m.status = "read-only: cannot delete facts while another process holds the store"
			return nil
		}
		if m.memPane == memPaneFacts {
			if sel, ok := m.factList.SelectedItem().(factItem); ok {
				if agSel, ok2 := m.agentList.SelectedItem().(agentItem); ok2 {
					_ = m.store.Delete(agSel.id, sel.fact.ID)
					m.status = "Deleted fact " + sel.fact.ID[:8]
					return []tea.Cmd{m.loadFacts(agSel.id), m.loadDashboard()}
				}
			}
		}
	case "r":
		return []tea.Cmd{m.loadAgents(), m.loadDashboard()}
	}
	return nil
}

func (m *tuiModel) updateSessionsKey(msg tea.KeyMsg) []tea.Cmd {
	switch msg.String() {
	case "r":
		return []tea.Cmd{m.loadSessions()}
	case "k":
		if m.readOnly {
			m.status = "read-only: cannot kill sessions while another process holds the store"
			return nil
		}
		if sel, ok := m.sessionList.SelectedItem().(sessionItem); ok {
			if err := m.store.SessionKill(sel.s.ID); err != nil {
				m.status = "kill: " + err.Error()
			} else {
				m.status = "Killed " + sel.s.ID[:8]
				return []tea.Cmd{m.loadSessions()}
			}
		}
	}
	return nil
}

func (m *tuiModel) updateStatsKey(msg tea.KeyMsg) []tea.Cmd {
	if msg.String() == "r" {
		return []tea.Cmd{m.loadDashboard()}
	}
	return nil
}

// ── View ──────────────────────────────────────────────────────────────────────

// View renders the dashboard. An error no longer replaces it.
//
// The old behaviour swapped the entire UI for "Error: ... Press q to quit." and
// nothing ever cleared it, so a single transient failure ended the session even
// though the store recovered a moment later. Errors now ride in the header
// while the rest of the UI stays usable and `r` keeps working.
func (m tuiModel) View() string {
	header := m.renderHeader()
	body := m.renderBody()
	footer := m.renderFooter()

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// truncateErr keeps an error readable inside the header chip. The full text is
// not lost: the failing view says what is unavailable, and the daemon logs the
// underlying cause.
func truncateErr(err error) string {
	const max = 48
	s := err.Error()
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func (m tuiModel) renderHeader() string {
	logo := styleLogo.Render(" GRAYMATTER ")

	tabs := make([]string, len(tabNames))
	for i, name := range tabNames {
		label := fmt.Sprintf("[%d] %s", i+1, name)
		if tabID(i) == m.activeTab {
			tabs[i] = styleTabActive.Render("▸ " + name + " ")
			_ = label
		} else {
			tabs[i] = styleTabInactive.Render(label)
		}
	}
	tabBar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)

	// Right side: version + clock + optional status.
	ver := styleVersion.Render("v" + version)
	clock := styleDimText.Render("· " + time.Now().Format("15:04"))
	right := lipgloss.JoinHorizontal(lipgloss.Top, ver, clock)

	// Optional status chip (e.g. after delete/kill).
	statusChip := ""
	// An error outranks a status message: it is why the numbers below may be
	// stale, and hiding it is what made a dead connection look like an empty
	// store.
	if m.err != nil {
		statusChip = "  " + styleStatusFail.Render("⚠ "+truncateErr(m.err))
	} else if m.status != "" {
		statusChip = "  " + styleSubText.Render("· "+m.status)
	}

	// Read-only badge: shown when the store is locked by another process.
	roBadge := ""
	if m.readOnly {
		roBadge = "  " + styleStatusFail.Render("⊘ read-only")
	}

	left := lipgloss.JoinHorizontal(lipgloss.Top, logo, " ", tabBar, statusChip, roBadge)

	// Justify left/right across full width.
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	gap := m.width - leftW - rightW
	if gap < 1 {
		gap = 1
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, left, strings.Repeat(" ", gap), right)

	// Thin separator line below the header.
	sep := lipgloss.NewStyle().Foreground(colorSlate).Render(
		strings.Repeat("─", max(1, m.width)))

	return lipgloss.JoinVertical(lipgloss.Left, bar, sep)
}

func (m tuiModel) renderBody() string {
	bodyH := m.height - 4 // header (2) + footer (1) + spacing
	if bodyH < 5 {
		bodyH = 5
	}
	switch m.activeTab {
	case tabMemory:
		return m.renderMemory(bodyH)
	case tabSessions:
		return m.renderSessions(bodyH)
	case tabGraph:
		return m.renderGraph(bodyH)
	case tabStats:
		return m.renderDashboard(bodyH)
	}
	return ""
}

func (m tuiModel) renderFooter() string {
	k := styleHelpKey.Render
	sep := "  " + styleHelpSep.Render("│") + "  "

	var groups []string
	switch m.activeTab {
	case tabMemory:
		mutKeys := k("d") + " " + styleDimText.Render("delete") + "  "
		if m.readOnly {
			mutKeys = styleStatusFail.Render("d") + " " + styleDimText.Render("(read-only)") + "  "
		}
		groups = []string{
			k("j/k") + " " + styleDimText.Render("nav") + "  " +
				k("←/→") + " " + styleDimText.Render("pane"),
			k("enter") + " " + styleDimText.Render("select") + "  " +
				mutKeys +
				k("r") + " " + styleDimText.Render("refresh"),
			k("1-4") + " " + styleDimText.Render("tabs") + "  " +
				k("q") + " " + styleDimText.Render("quit"),
		}
	case tabSessions:
		killKey := k("k") + " " + styleDimText.Render("kill") + "  "
		if m.readOnly {
			killKey = styleStatusFail.Render("k") + " " + styleDimText.Render("(read-only)") + "  "
		}
		groups = []string{
			k("j/k") + " " + styleDimText.Render("nav"),
			killKey +
				k("r") + " " + styleDimText.Render("refresh"),
			k("1-4") + " " + styleDimText.Render("tabs") + "  " +
				k("q") + " " + styleDimText.Render("quit"),
		}
	case tabGraph:
		groups = []string{
			k("j/k") + " " + styleDimText.Render("nav"),
			k("1-4") + " " + styleDimText.Render("tabs") + "  " +
				k("q") + " " + styleDimText.Render("quit"),
		}
	case tabStats:
		groups = []string{
			k("r") + " " + styleDimText.Render("refresh") + "  " +
				styleDimText.Render("(auto every 5s)"),
			k("1-4") + " " + styleDimText.Render("tabs") + "  " +
				k("q") + " " + styleDimText.Render("quit"),
		}
	}
	return styleHelp.Render(strings.Join(groups, sep))
}

// ── Memory tab ────────────────────────────────────────────────────────────────

func (m tuiModel) renderMemory(h int) string {
	colW := m.width / 3

	agentPane := border(m.memPane == memPaneAgents).
		Width(colW - 2).Height(h - 2).Render(m.agentList.View())
	factPane := border(m.memPane == memPaneFacts).
		Width(colW - 2).Height(h - 2).Render(m.factList.View())
	detailPane := border(m.memPane == memPaneDetail).
		Width(m.width - colW*2 - 6).Height(h - 2).Render(m.detail.View())

	return lipgloss.JoinHorizontal(lipgloss.Top, agentPane, factPane, detailPane)
}

// ── Sessions tab ──────────────────────────────────────────────────────────────

func (m tuiModel) renderSessions(h int) string {
	return styleBorderInactive.Width(m.width - 4).Height(h - 2).Render(m.sessionList.View())
}

// ── Graph tab ─────────────────────────────────────────────────────────────────

func (m tuiModel) renderGraph(h int) string {
	if len(m.nodeList.Items()) == 0 {
		return styleBorderInactive.Width(m.width - 4).Height(h - 2).
			Render(styleDimText.Render("\n  Knowledge graph empty.\n  Run `graymatter init`, store some memories, and enable auto-population with `graymatter daemon run --kg`\n  (or add explicit edges via `memory_reflect action=link`)."))
	}
	leftW := m.width * 2 / 5
	stats := styleDimText.Render(fmt.Sprintf(
		"  entities: %d · edges: %d · orphans: %d",
		len(m.nodeList.Items()), m.kgEdgeCount, m.kgOrphans))
	// The stats block costs 5 screen rows (3 content + 2 border). The panes
	// must discount them or the tab overflows the viewport and pushes the
	// header off screen — h-7 content + 2 border + 5 = h, an exact fit.
	header := styleBorderInactive.Width(m.width - 4).Height(3).Render(stats)
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		styleBorderInactive.Width(leftW-2).Height(h-7).Render(m.nodeList.View()),
		styleBorderInactive.Width(m.width-leftW-4).Height(h-7).Render(m.nodeDetail.View()))
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

// ── Size management ───────────────────────────────────────────────────────────

func (m *tuiModel) updateSizes() {
	bodyH := m.height - 4
	if bodyH < 5 {
		bodyH = 5
	}
	listH := bodyH - 4
	if listH < 3 {
		listH = 3
	}

	colW := m.width / 3
	m.agentList.SetSize(colW-4, listH)
	m.factList.SetSize(colW-4, listH)
	m.detail.Width = m.width - colW*2 - 8
	m.detail.Height = listH

	m.sessionList.SetSize(m.width-6, listH)

	leftW := m.width * 2 / 5
	// Graph-tab panes sit under a 5-row stats header the other tabs don't
	// have, so their lists get 3 rows less than the shared listH (see
	// renderGraph for the arithmetic).
	m.nodeList.SetSize(leftW-4, listH-3)
	m.nodeDetail.Width = m.width - leftW - 6
	m.nodeDetail.Height = listH - 3
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func formatNodeDetail(n kg.Node) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Entity:  %s\n", n.Label))
	sb.WriteString(fmt.Sprintf("Type:    %s\n", n.EntityType))
	sb.WriteString(fmt.Sprintf("ID:      %s\n", n.ID))
	sb.WriteString(fmt.Sprintf("Weight:  %.4f\n", n.Weight))
	sb.WriteString(fmt.Sprintf("First:   %s\n", n.FirstSeen.Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("Last:    %s\n", n.LastSeen.Format("2006-01-02 15:04")))
	return sb.String()
}
func formatFactDetail(f memory.Fact) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ID:      %s\n", f.ID))
	sb.WriteString(fmt.Sprintf("Agent:   %s\n", f.AgentID))
	sb.WriteString(fmt.Sprintf("Created: %s\n", f.CreatedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Weight:  %.4f\n", f.Weight))
	sb.WriteString(fmt.Sprintf("Access:  %d times\n", f.AccessCount))
	if f.Pinned {
		sb.WriteString(fmt.Sprintf("Pinned:  yes (exempt from decay, pruning, summarisation)\n"))
	}
	if f.Confidence != "" {
		sb.WriteString(fmt.Sprintf("Confidence: %s\n", f.Confidence))
	}
	sb.WriteString("\n─── Text ────────────────────────────\n\n")
	sb.WriteString(f.Text)
	sb.WriteString("\n")
	return sb.String()
}

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

// ── Command wiring ─────────────────────────────────────────────────────────────

func tuiCmd() *cobra.Command {
	var forceReadOnly bool

	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Observability dashboard for GrayMatter",
		Long: `Interactive 4-view terminal UI for GrayMatter.

Views (switch with 1-4 or tab/shift+tab):
  1. Memory   — browse agents, facts, and full fact detail
  2. Sessions — view and kill managed agent sessions
  3. Graph    — browse knowledge graph nodes and edges
  4. Stats    — observability dashboard (KPIs, agent activity, weight distribution, 30d sparkline)

By default the TUI connects to the store daemon, so it runs fine alongside an
MCP server, opencode, or another TUI. --read-only applies only with --no-daemon
(direct in-process open), where it forces a non-mutating open.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --read-only only makes sense for the direct (--no-daemon) path;
			// through the daemon every client is a full read/write peer.
			if forceReadOnly {
				noDaemon = true
			}

			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			newList := func(title string) list.Model {
				l := list.New(nil, list.NewDefaultDelegate(), 40, 20)
				l.Title = title
				l.SetShowStatusBar(false)
				l.SetFilteringEnabled(true)
				return l
			}

			m := tuiModel{
				store:       store,
				dataDir:     dataDir,
				readOnly:    store.IsReadOnly(),
				agentList:   newList("Agents"),
				factList:    newList("Facts"),
				sessionList: newList("Sessions"),
				nodeList:    newList("KG Nodes"),
				detail:      viewport.New(40, 20),
				nodeDetail:  viewport.New(40, 20),
			}

			p := tea.NewProgram(m, tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}

	cmd.Flags().BoolVar(&forceReadOnly, "read-only", false,
		"with --no-daemon: open the store read-only (skips all mutating operations)")
	return cmd
}
