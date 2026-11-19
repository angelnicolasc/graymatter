package main

import (
	"fmt"
	"image/color"
	"os"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type tuiTheme struct {
	Canvas, Surface, Text, Muted, Accent, Border color.Color
	Success, Warning, Info, Purple               color.Color
}

func (m *tuiModel) setTheme(name string) {
	if name == "" {
		name = "dark"
	}
	m.themeName = name
	c := lipgloss.Color
	switch name {
	case "light":
		m.theme = tuiTheme{Canvas: c("#F2F3ED"), Surface: c("#E2E8DF"), Text: c("#26332D"), Muted: c("#637268"), Accent: c("#147C70"), Border: c("#CCD5CA"), Success: c("#586E20"), Warning: c("#8A5A17"), Info: c("#286A8D"), Purple: c("#725389")}
	case "terminal":
		m.theme = tuiTheme{Accent: c("6"), Success: c("2"), Warning: c("3"), Info: c("4"), Purple: c("5")}
	default:
		m.theme = tuiTheme{Canvas: c("#171C1C"), Surface: c("#202828"), Text: c("#E6EAE3"), Muted: c("#85918B"), Accent: c("#66C5B8"), Border: c("#34413D"), Success: c("#B5C96A"), Warning: c("#D9AC65"), Info: c("#86BBD8"), Purple: c("#B9A1D6")}
	}
	input := textinput.DefaultStyles(name != "light")
	input.Focused.Text = lipgloss.NewStyle().Foreground(m.theme.Text)
	input.Focused.Prompt = lipgloss.NewStyle().Foreground(m.theme.Accent)
	input.Focused.Placeholder = lipgloss.NewStyle().Foreground(m.theme.Muted)
	input.Blurred = input.Focused
	input.Cursor.Color = m.theme.Accent
	m.input.SetStyles(input)
	editor := textarea.DefaultStyles(name != "light")
	editor.Focused.Text = lipgloss.NewStyle().Foreground(m.theme.Text)
	editor.Focused.Prompt = lipgloss.NewStyle().Foreground(m.theme.Accent)
	editor.Focused.Placeholder = lipgloss.NewStyle().Foreground(m.theme.Muted)
	editor.Focused.CursorLine = lipgloss.NewStyle().Background(m.theme.Surface)
	editor.Focused.EndOfBuffer = lipgloss.NewStyle().Foreground(m.theme.Border)
	editor.Blurred = editor.Focused
	editor.Cursor.Color = m.theme.Accent
	m.editor.SetStyles(editor)
	m.previewText = ""
}

// tuiPlain is the boundary between stored text and terminal control. Strip
// escape sequences and directional overrides before applying our own styles.
func tuiPlain(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func fitCells(s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	out := make([]string, h)
	for i := range out {
		if i < len(lines) {
			out[i] = ansi.Truncate(lines[i], w, "")
		}
		out[i] += strings.Repeat(" ", max(0, w-ansi.StringWidth(out[i])))
	}
	return strings.Join(out, "\n")
}

func (m tuiModel) heading(s string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(s)
}
func (m tuiModel) muted(s string) string {
	return lipgloss.NewStyle().Foreground(m.theme.Muted).Render(s)
}
func (m tuiModel) ink(s string) string {
	return lipgloss.NewStyle().Foreground(m.theme.Text).Render(s)
}
func (m tuiModel) tone(s string, c color.Color) string {
	return lipgloss.NewStyle().Foreground(c).Render(s)
}

func (m tuiModel) choiceLine(s string, w int, selected bool) string {
	w = min(w, 86)
	if selected {
		return lipgloss.NewStyle().Foreground(m.theme.Accent).Background(m.theme.Surface).Bold(true).Render(fitCells("▎ "+tuiPlain(s), w, 1))
	}
	return m.ink(fitCells("  "+tuiPlain(s), w, 1))
}

const (
	tuiFooterRows = 2
	tuiPanelRows  = 2
	tuiTabRow     = 2
	tuiTabInset   = 1
)

// Rendering, hit-testing and pagination share the same chrome geometry. Short
// terminals keep the space above navigation; taller ones also separate scope.
func (m tuiModel) headerHeight() int {
	if m.height >= 30 {
		return 4
	}
	return 3
}
func (m tuiModel) bodyTop() int       { return m.headerHeight() + 1 }
func (m tuiModel) contentTop() int    { return m.bodyTop() + tuiPanelRows }
func (m tuiModel) bodyHeight() int    { return max(1, m.height-m.bodyTop()-tuiFooterRows) }
func (m tuiModel) contentHeight() int { return max(1, m.bodyHeight()-tuiPanelRows) }
func (m tuiModel) listRowHeight() int {
	if m.activeTab == tabMemory && m.height >= 30 {
		return 3
	}
	return 2
}

func (m tuiModel) tabLabel(i int) string {
	if tabID(i) == m.activeTab {
		return fmt.Sprintf(" ›%d %s  ", i+1, tabNames[i])
	}
	return fmt.Sprintf("  %d %s  ", i+1, tabNames[i])
}

func (m tuiModel) panel(title, body string, w, h int, focused bool) string {
	if w < 3 || h < 3 {
		return fitCells(body, w, h)
	}
	mark, label := "  ", m.ink(tuiPlain(title))
	if focused {
		mark, label = m.heading("▎ "), m.heading(tuiPlain(title))
	}
	rows := []string{fitCells(mark+label, w, 1), m.tone(strings.Repeat("─", w), m.theme.Border)}
	for _, line := range strings.Split(fitCells(body, w-2, h-2), "\n") {
		rows = append(rows, " "+line+" ")
	}
	return strings.Join(rows, "\n")
}

func (m tuiModel) separator(panel string, w int) string {
	lines := strings.Split(panel, "\n")
	for i, line := range lines {
		lines[i] = fitCells(line, w-1, 1) + m.tone("│", m.theme.Border)
	}
	return strings.Join(lines, "\n")
}

func (m tuiModel) columnWidths() (int, int, int) {
	if m.width < 100 {
		return 0, m.width, m.width
	}
	ns := 0
	if m.activeTab == tabMemory && m.width >= 120 {
		ns = 20
	}
	left := (m.width - ns) * 57 / 100
	return ns, left, m.width - ns - left
}

func (m *tuiModel) updateSizes() {
	if m.layoutWidth == m.width && m.layoutHeight == m.height && m.layoutTab == m.activeTab {
		return
	}
	m.layoutWidth, m.layoutHeight, m.layoutTab = m.width, m.height, m.activeTab
	h := m.contentHeight()
	// The delegate owns keyboard page movement even though rendering is custom.
	// Keep its spacing in sync when crossing the compact-height breakpoint.
	d := list.NewDefaultDelegate()
	d.SetSpacing(m.listRowHeight() - 2)
	m.factList.SetDelegate(d)
	ns, lw, dw := m.columnWidths()
	if ns == 0 {
		ns = m.width
	}
	m.agentList.SetSize(max(1, ns-2), h)
	m.factList.SetSize(max(1, lw-2), h)
	m.sessionList.SetSize(max(1, lw-2), h)
	m.nodeList.SetSize(max(1, lw-2), h)
	m.recallList.SetSize(max(1, lw-2), h)
	m.checkpointList.SetSize(max(1, lw-2), h)
	if m.width < 100 || m.activeTab == tabStats {
		dw = m.width
	}
	m.detail.SetWidth(max(1, dw-2))
	m.detail.SetHeight(h)
	m.detail.SoftWrap = true
	m.input.SetWidth(max(1, min(86, m.width-6)))
	m.editor.SetWidth(max(1, min(90, m.width-6)))
	m.editor.SetHeight(max(1, min(16, h-4)))
}

func (m tuiModel) View() tea.View {
	if m.width < 30 || m.height < 10 {
		v := tea.NewView(fitCells("GRAYMATTER\nResize to at least 30×10.\nq quit", m.width, m.height))
		v.AltScreen = true
		return v
	}
	bodyH := m.bodyHeight()
	body := m.renderBody()
	if m.modal != "" {
		body = m.renderModal(m.width, bodyH)
	} else if m.inputMode != "" {
		body = m.renderInput(m.width, bodyH)
	}
	content := m.renderHeader() + "\n" + m.renderToolbar() + "\n" + fitCells(body, m.width, bodyH) + "\n" + m.renderFooter()
	content = fitCells(content, m.width, m.height)
	noColor := os.Getenv("NO_COLOR") != ""
	if noColor {
		content = ansi.Strip(content)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = "Graymatter · Memory Workbench"
	if !noColor {
		v.BackgroundColor = m.theme.Canvas
		v.ForegroundColor = m.theme.Text
	}
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func (m tuiModel) renderHeader() string {
	brand := m.heading(" GRAYMATTER") + m.muted("  memory workbench")
	right := m.muted("v" + version)
	if m.demo {
		right = m.tone("DEMO", m.theme.Warning) + m.muted(" · sample data  ") + right
	}
	if m.readOnly {
		right = m.tone("READ ONLY", m.theme.Info) + "  " + right
	}
	gap := max(1, m.width-ansi.StringWidth(brand)-ansi.StringWidth(right)-1)
	var tabs []string
	for i := range tabNames {
		label := m.tabLabel(i)
		if tabID(i) == m.activeTab {
			label = lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Background(m.theme.Surface).Render(label)
		} else {
			label = m.muted(label)
		}
		tabs = append(tabs, label)
	}
	return fitCells(brand+strings.Repeat(" ", gap)+right+"\n\n"+strings.Repeat(" ", tuiTabInset)+strings.Join(tabs, ""), m.width, m.headerHeight())
}

func (m tuiModel) renderToolbar() string {
	name := tuiPlain(m.namespace)
	if name == "" {
		name = "choose namespace · n"
	}
	scope := " " + name
	switch m.activeTab {
	case tabMemory:
		state := m.factMode
		if state == "" {
			state = "active"
		}
		scope += " / " + state + fmt.Sprintf(" · %d memories", m.factTotal)
		if m.factQuery != "" {
			scope += " · filter: " + tuiPlain(m.factQuery)
		}
		if m.factTotal > len(m.factList.Items()) {
			scope += fmt.Sprintf(" · %d shown · Ctrl+N more", len(m.factList.Items()))
		}
	case tabRecall:
		if m.recallQuery == "" {
			scope += " / recall · Enter to query"
		} else {
			scope += " / " + tuiPlain(m.recallQuery)
		}
	case tabSessions:
		if m.activityCheckpoints {
			scope += " / checkpoints"
		} else {
			scope = " All namespaces / harness runs"
		}
	case tabGraph:
		scope = fmt.Sprintf(" Store-wide / %d entities · %d edges · %d orphans", len(m.nodeList.Items()), m.kgEdgeCount, m.kgOrphans)
	case tabUsage:
		scope = " Subscriptions & API"
	case tabStats:
		scope = " Status / store & providers"
	}
	r := m.resources[m.activeResource()]
	state, c := "", m.theme.Muted
	if r.Loading {
		state, c = "◌ Loading", m.theme.Info
	} else if r.Err != nil {
		state, c = "⚠ "+tuiPlain(r.Err.Error()), m.theme.Warning
		if !r.Updated.IsZero() {
			state = "⚠ Stale · " + tuiPlain(r.Err.Error())
		}
	} else if !r.Updated.IsZero() {
		state = "Updated " + r.Updated.Format("15:04:05")
	}
	if m.err != nil && m.activeTab == tabMemory {
		state, c = "⚠ "+tuiPlain(m.err.Error()), m.theme.Warning
	}
	// Keep a failed resource visible without letting its message erase scope.
	rw := min(ansi.StringWidth(state), max(16, m.width/2))
	left := fitCells(m.muted(scope), max(1, m.width-rw-2), 1)
	return fitCells(left+" "+m.tone(ansi.Truncate(state, rw, "…"), c), m.width, 1)
}

func (m tuiModel) activeResource() string {
	switch m.activeTab {
	case tabMemory:
		return "memory"
	case tabRecall:
		return "recall"
	case tabSessions:
		if m.activityCheckpoints {
			return "checkpoints"
		}
		return "activity"
	case tabGraph:
		return "graph"
	case tabStats:
		return "status"
	}
	return "usage"
}

func (m tuiModel) renderFooter() string {
	help := "Tab focus · ↑↓ scroll"
	switch m.activeTab {
	case tabMemory:
		help = "f filter · n namespace · [ ] state · a add · e revise · p pin · d retire · v lineage · y copy"
	case tabRecall:
		help = "Enter query · Tab inspect · v lineage · y copy"
	case tabSessions:
		help = "c runs/checkpoints · Tab inspect · x stop run"
	case tabGraph:
		help = "o neighbors · s source facts · Tab inspect"
	case tabUsage:
		help = "a auto · l limits · s spend · c context · v sources · ↑↓ scroll"
	}
	if m.modal == "add" || m.modal == "revise" {
		help = "Ctrl+S save · Enter new line · Esc cancel"
	} else if m.modal == "lineage" {
		help = "Tab choose source · Enter follow · Backspace previous · ↑↓ scroll · Esc close"
	} else if m.modal == "help" {
		help = "Esc return"
	} else if m.modal != "" {
		help = "Enter confirm · Esc cancel"
	} else if m.inputMode != "" {
		help = "Type to search · ↑↓ choose · Enter apply · Esc return"
	}
	first := m.muted(" " + help)
	status := m.status
	var subjectWarning string
	switch m.activeTab {
	case tabMemory:
		subjectWarning = m.factSubjectWarning
	case tabRecall:
		subjectWarning = m.recallSubjectWarning
	}
	if subjectWarning != "" {
		if status != "" {
			subjectWarning += " · " + status
		}
		status = subjectWarning
	}
	if status != "" {
		first = m.ink(" " + tuiPlain(status))
	}
	global := "/ commands · 1–6 tabs · r refresh · q quit "
	gw := ansi.StringWidth(global)
	glance := m.usageSummary(max(0, m.width-gw-2))
	if glance == "" {
		glance = m.heading("u Usage")
	}
	last := " " + fitCells(glance, max(1, m.width-gw-1), 1) + m.muted(global)
	return fitCells(first+"\n"+last, m.width, 2)
}

func (m tuiModel) renderBody() string {
	h := m.bodyHeight()
	switch m.activeTab {
	case tabMemory:
		return m.renderMemory(h)
	case tabRecall:
		return m.renderSplit(fmt.Sprintf("Results · %d", len(m.recallList.VisibleItems())), m.recallList, "Recall receipt", h)
	case tabSessions:
		return m.renderSessions(h)
	case tabGraph:
		return m.renderGraph(h)
	case tabUsage:
		return m.renderUsage(m.width, h)
	case tabStats:
		return m.panel("System & corpus", m.detail.View(), m.width, h, true)
	}
	return ""
}

func (m tuiModel) renderList(l list.Model, w, h int, empty string) string {
	items := l.VisibleItems()
	if len(items) == 0 {
		return fitCells(m.muted(empty), w, h)
	}
	rowHeight := m.listRowHeight()
	count := max(1, h/rowHeight)
	sel := l.Index()
	start := (sel / count) * count
	var rows []string
	for i := start; i < min(len(items), start+count); i++ {
		item, ok := items[i].(interface {
			Title() string
			Description() string
		})
		if !ok {
			continue
		}
		title := strings.ReplaceAll(tuiPlain(item.Title()), "\n", " ")
		meta := tuiPlain(item.Description())
		if f, ok := items[i].(factItem); ok {
			wrapped := strings.Split(ansi.Wrap(title, max(1, w-2), ""), "\n")
			title = wrapped[0]
			if len(wrapped) > 1 {
				meta = wrapped[1]
				if len(wrapped) > 2 {
					meta = ansi.Truncate(meta, max(1, w-3), "") + "…"
				}
			} else {
				meta = factState(f.fact) + " · " + f.fact.CreatedAt.Format("02 Jan")
				if f.fact.Pinned {
					meta = "pinned · " + meta
				}
			}
		}
		marker := "  "
		if i == sel {
			marker = "▎ "
		}
		line := fitCells(marker+ansi.Truncate(title, max(1, w-2), "…"), w, 1)
		second := fitCells("  "+ansi.Truncate(meta, max(1, w-2), "…"), w, 1)
		if i == sel {
			rows = append(rows, lipgloss.NewStyle().Foreground(m.theme.Accent).Background(m.theme.Surface).Bold(true).Render(line), lipgloss.NewStyle().Foreground(m.theme.Muted).Background(m.theme.Surface).Render(second))
		} else {
			rows = append(rows, m.ink(line), m.muted(second))
		}
		if rowHeight > 2 {
			rows = append(rows, "")
		}
	}
	return fitCells(strings.Join(rows, "\n"), w, h)
}

func (m tuiModel) renderNamespaces(w, h int) string {
	items := m.agentList.VisibleItems()
	if len(items) == 0 {
		return fitCells(m.muted("No namespaces yet"), w, h)
	}
	count := max(1, h)
	start := (m.agentList.Index() / count) * count
	var rows []string
	for i := start; i < min(len(items), start+count); i++ {
		a := items[i].(agentItem)
		n := fmt.Sprint(a.count)
		label := fitCells(ansi.Truncate(tuiPlain(a.id), max(1, w-ansi.StringWidth(n)-3), "…"), max(1, w-ansi.StringWidth(n)-2), 1)
		line := "  " + label + n
		if a.id == m.namespace {
			line = lipgloss.NewStyle().Foreground(m.theme.Accent).Background(m.theme.Surface).Bold(true).Render(fitCells("▎ "+label+n, w, 1))
		} else {
			line = m.muted(fitCells(line, w, 1))
		}
		rows = append(rows, line)
	}
	return fitCells(strings.Join(rows, "\n"), w, h)
}

func (m tuiModel) renderMemory(h int) string {
	ns, lw, dw := m.columnWidths()
	facts := m.renderList(m.factList, lw-2, h-2, "No matching memories.\n\na add a fact · f change filter\n[ ] change lifecycle view")
	title := "Memory"
	if n := len(m.factList.VisibleItems()); n > 0 {
		title += fmt.Sprintf(" · %d/%d", m.factList.Index()+1, n)
	}
	if m.width < 100 {
		if m.focus == 1 || m.memPane == memPaneDetail {
			return m.panel("Memory · Esc returns", m.detail.View(), m.width, h, true)
		}
		if m.memPane == memPaneAgents {
			return m.panel("Namespaces · Enter opens", m.renderNamespaces(m.width-2, h-2), m.width, h, true)
		}
		return m.panel(title, facts, m.width, h, true)
	}
	parts := []string{}
	if ns > 0 {
		parts = append(parts, m.separator(m.panel("Namespaces", m.renderNamespaces(ns-2, h-2), ns, h, m.memPane == memPaneAgents), ns))
	}
	parts = append(parts, m.separator(m.panel(title, facts, lw, h, m.memPane == memPaneFacts), lw), m.panel("Selected memory", m.detail.View(), dw, h, m.memPane == memPaneDetail))
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

func (m tuiModel) renderSplit(title string, l list.Model, detailTitle string, h int) string {
	_, lw, dw := m.columnWidths()
	empty := "No entries yet.\n\nr refresh"
	if m.activeTab == tabRecall {
		empty = "Search what your agents remember.\n\nEnter to write a query"
	}
	if m.width < 100 {
		if m.focus == 1 {
			return m.panel(detailTitle+" · Esc returns", m.detail.View(), m.width, h, true)
		}
		return m.panel(title, m.renderList(l, m.width-2, h-2, empty), m.width, h, true)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m.separator(m.panel(title, m.renderList(l, lw-2, h-2, empty), lw, h, m.focus == 0), lw), m.panel(detailTitle, m.detail.View(), dw, h, m.focus == 1))
}

func (m tuiModel) renderSessions(h int) string {
	if m.activityCheckpoints {
		return m.renderSplit("Checkpoints", m.checkpointList, "Checkpoint detail", h)
	}
	return m.renderSplit("Harness runs", m.sessionList, "Run detail", h)
}

func (m tuiModel) renderGraph(h int) string {
	if len(m.nodeList.Items()) == 0 {
		return m.panel("Knowledge graph", m.muted("No entities yet.\n\nEnable extraction with graymatter init --kg,\nor link entities with memory_reflect.\n\nr refresh"), m.width, h, false)
	}
	return m.renderSplit("Entities", m.nodeList, "Relationships & sources", h)
}
