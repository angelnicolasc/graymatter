package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/lineageview"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/charmbracelet/x/ansi"
)

func (m *tuiModel) syncPreview(reset bool) {
	m.updateSizes()
	key := fmt.Sprintf("%d/%s/", m.activeTab, m.namespace)
	switch m.activeTab {
	case tabMemory:
		key += m.selectedFactID()
	case tabRecall:
		if r, ok := m.recallList.SelectedItem().(receiptItem); ok {
			key += r.receipt.Provenance.FactID
		}
	case tabSessions:
		if m.activityCheckpoints {
			if c, ok := m.checkpointList.SelectedItem().(checkpointItem); ok {
				key += c.cp.ID
			}
		} else if s, ok := m.sessionList.SelectedItem().(sessionItem); ok {
			key += s.s.ID
		}
	case tabGraph:
		if n, ok := m.nodeList.SelectedItem().(nodeItem); ok {
			key += n.n.ID
		}
	}
	if reset || key != m.previewID {
		m.detail.GotoTop()
	}
	m.previewID = key
	content := ansi.Wrap(m.previewContent(), max(1, m.detail.Width()), "")
	if content != m.previewText {
		m.detail.SetContent(content)
		m.previewText = content
	}
}
func (m tuiModel) previewContent() string {
	switch m.activeTab {
	case tabMemory:
		if f, ok := m.factList.SelectedItem().(factItem); ok {
			return m.factPreview(f.fact)
		}
		return m.ink("Your memory, in context.") + "\n\n" + m.muted("Select a fact to read its full content.\na add a fact · n choose namespace")
	case tabRecall:
		if r, ok := m.recallList.SelectedItem().(receiptItem); ok {
			return m.receiptPreview(r.receipt)
		}
		return m.ink("See why a memory surfaces.") + "\n\n" + m.muted("Enter a query to inspect ranked results.\nAccess counters stay unchanged.\n\nYour embedding provider may process the query.")
	case tabSessions:
		if m.activityCheckpoints {
			if c, ok := m.checkpointList.SelectedItem().(checkpointItem); ok {
				return m.checkpointPreview(c.cp)
			}
			return m.muted("No saved checkpoints in this namespace.\n\nCheckpoints capture explicitly recorded state.")
		}
		if s, ok := m.sessionList.SelectedItem().(sessionItem); ok {
			r := s.s
			finish := "in progress"
			if r.FinishedAt != nil {
				finish = r.FinishedAt.Format(time.RFC3339)
			}
			c := m.theme.Success
			if r.Status == "failed" {
				c = m.theme.Warning
			}
			out := m.ink(tuiPlain(r.AgentFile)) + "\n\n" + m.tone("● "+tuiPlain(r.Status), c)
			if r.ErrorMsg != "" {
				out += "\n" + m.tone(tuiPlain(r.ErrorMsg), m.theme.Warning)
			}
			out += m.detailSection("Run", m.muted(fmt.Sprintf("Namespace: %s\nStarted: %s\nFinished: %s\nAttempts: %d", tuiPlain(r.AgentID), r.StartedAt.Format(time.RFC3339), finish, r.Attempts)))
			out += m.detailSection("Record", m.muted(fmt.Sprintf("ID: %s\nPID: %d\nCheckpoint: %s\nLog file: %s", tuiPlain(r.ID), r.PID, tuiPlain(r.LastCPID), tuiPlain(r.LogFile))))
			return out + "\n\n" + m.muted("Graymatter harness runs only.")
		}
		return m.ink("Follow your agent runs.") + "\n\n" + m.muted("Runs started by graymatter run appear here.\nc opens saved checkpoints.")
	case tabGraph:
		if n, ok := m.nodeList.SelectedItem().(nodeItem); ok {
			return m.graphPreview(n.n)
		}
	case tabStats:
		return m.statusContent()
	}
	return ""
}
func (m tuiModel) nodeLabel(id string) string {
	for _, it := range m.nodeList.Items() {
		n := it.(nodeItem)
		if n.n.ID == id {
			return n.n.Label
		}
	}
	return id
}
func formatNodeDetail(n kg.Node) string {
	return fmt.Sprintf("Entity: %s\nType: %s\nID: %s\nWeight: %.4f\nFirst: %s\nLast: %s\n", tuiPlain(n.Label), tuiPlain(n.EntityType), tuiPlain(n.ID), n.Weight, n.FirstSeen.Format(time.RFC3339), n.LastSeen.Format(time.RFC3339))
}
func formatFactDetail(f memory.Fact) string {
	var b strings.Builder
	b.WriteString(tuiPlain(f.Text))
	b.WriteString("\n\nMEMORY DETAILS\n")
	b.WriteString(fmt.Sprintf("State: %s\nID: %s\nNamespace: %s\nCreated: %s\nLast accessed: %s\nAccesses: %d\nWeight: %.4f\nConfidence: %s\n", factState(f), tuiPlain(f.ID), tuiPlain(f.AgentID), f.CreatedAt.Format(time.RFC3339), f.AccessedAt.Format(time.RFC3339), f.AccessCount, f.Weight, confidenceDetail(f.Confidence)))
	if f.Pinned {
		b.WriteString("Pinned: yes (exempt from decay and pruning)\n")
	}
	if f.IsAlias() {
		b.WriteString("Kind: vocabulary alias (not injectable)\nSource: " + tuiPlain(f.AliasSource) + "\n")
	}
	if f.IsSuperseded() {
		b.WriteString("Superseded by: " + tuiPlain(f.SupersededBy) + "\nThis record is excluded from retrieval.\n")
	}
	for _, line := range lineageview.Lines(f.LineageProjection) {
		b.WriteString(tuiPlain(line) + "\n")
	}
	b.WriteString("\nVersion lineage is scoped to this namespace. Weight measures retention.\n")
	return b.String()
}

func receiptScoreDetail(f memory.RecallReceipt) string {
	out := fmt.Sprintf("Base RRF score: %.6f (k=%.0f)", f.Ranks.FusedScore, f.Ranks.K)
	if r := f.Ranking; r != nil {
		out += fmt.Sprintf("\nFinal score: %.6f\nConfidence factor: %.4f\nPolicy: %s · confidence weight %.4f\nEffective confidence: %s", r.FinalScore, r.Factor, tuiPlain(r.Policy), r.ConfidenceWeight, tuiPlain(r.EffectiveConfidence))
	}
	return out
}
func (m tuiModel) statusContent() string {
	d := m.dashboard
	if d.Err != nil {
		return m.tone("⚠ Store unreachable", m.theme.Warning) + "\n\n" + m.ink(tuiPlain(d.Err.Error())) + "\n\n" + m.muted("r retry · graymatter doctor for diagnostics")
	}
	if !d.Loaded {
		return m.muted("Loading store snapshot…")
	}
	w := max(1, (m.width-2)/4)
	values := []string{fmt.Sprint(d.FactsN), fmt.Sprint(d.AgentsN), formatCompact(d.RecallsN), formatBytes(d.StorageB)}
	labels := []string{"stored records", "namespaces", "fact accesses", "payload estimate"}
	var top, bottom string
	for i, value := range values {
		c := m.theme.Success
		if i == 2 {
			c = m.theme.Info
		}
		if i == 3 {
			c = m.theme.Purple
		}
		top += fitCells(m.tone(value, c), w, 1)
		bottom += fitCells(m.muted(labels[i]), w, 1)
	}
	out := top + "\n" + bottom
	if d.Loaded && m.store != nil {
		out += m.detailSection("Memory maintenance", m.muted(maintenanceModeLabel(d.MaintenanceMode)))
	}
	if d.FactsN == 0 {
		out += "\n\n" + m.muted("No memories yet. Add one from Memory.")
	}
	if r := m.resources["provider"]; !r.Updated.IsZero() {
		h := m.health
		provider := tuiPlain(h.Provider)
		if provider == "" {
			provider = "Not configured"
		}
		provider = m.ink(provider) + m.muted(fmt.Sprintf(" · configured: %t · %s", h.ProviderConfigured, tuiPlain(h.ProviderReachability)))
		provider += "\n" + m.muted(fmt.Sprintf("%d dimensions · %d degraded writes · %d pending vectors", h.Embedding.EmbedDims, h.Embedding.DegradedFacts, h.Embedding.PendingVectors))
		if h.Embedding.VectorSearchDegraded {
			provider += "\n" + m.tone("Affected namespaces use keyword and recency ranking while the vector index is unavailable", m.theme.Warning)
		}
		if h.Embedding.LastDegradError != "" {
			provider += "\n" + m.tone(tuiPlain(h.Embedding.LastDegradError), m.theme.Warning)
		}
		out += m.detailSection("Embedding provider", provider)
	}
	var rows []string
	for _, name := range []string{"namespaces", "memory", "recall", "activity", "checkpoints", "graph", "status", "provider"} {
		r := m.resources[name]
		state, c, mark := "not loaded", m.theme.Muted, "○ "
		if !r.Updated.IsZero() {
			state, c, mark = "updated "+r.Updated.Format(time.RFC3339), m.theme.Text, "● "
		}
		if r.Loading {
			state, c, mark = "loading", m.theme.Info, "◌ "
		}
		if r.Err != nil {
			state, c, mark = tuiPlain(r.Err.Error()), m.theme.Warning, "⚠ "
			if !r.Updated.IsZero() {
				state = "stale · " + state + " · last success " + r.Updated.Format(time.RFC3339)
			}
		}
		rows = append(rows, m.tone(mark, c)+fitCells(m.muted(name), 13, 1)+m.tone(state, c))
	}
	out += m.detailSection("Resource freshness", strings.Join(rows, "\n"))
	out += m.detailSection("Store", m.muted(tuiPlain(m.dataDir)+fmt.Sprintf("\nRead-only session: %t · Mean retention weight: %.3f", m.readOnly, d.AvgWeight)))
	out += "\n\n" + m.muted("Records include all lifecycle states.\nPayload estimate: text + embeddings, not disk size.\nFact accesses: not query count.\nCorpus activity counts fact creation, not retrieval requests.\nProvider connectivity is not inferred from database connectivity.\nu opens subscriptions and API spend.")
	return out
}
func (m tuiModel) renderInput(w, h int) string {
	title := map[string]string{"palette": "Commands", "filter": "Filter current list", "namespace": "Choose namespace", "recall": "Inspect recall", "graph-neighbor": "Connected entities", "graph-source": "Supporting facts"}[m.inputMode]
	var lines []string
	lines = append(lines, m.input.View(), "")
	limit := max(1, h-7)
	selected := m.paletteIndex
	switch m.inputMode {
	case "palette":
		cs := m.matchingCommands()
		if len(cs) > 0 {
			selected = min(selected, len(cs)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(cs), start+limit); i++ {
			c := cs[i]
			line := fitCells("/"+c.name, 19, 1) + c.description
			if c.key != "" {
				line = fitCells(line, max(22, min(w-8, 78)-len(c.key)), 1) + c.key
			}
			lines = append(lines, m.choiceLine(line, w-2, i == selected))
		}
	case "namespace":
		ns := m.namespaceMatches(m.input.Value())
		if len(ns) == 0 && !m.readOnly {
			lines = append(lines, "Enter selects a new namespace; a adds its first fact.")
		}
		if len(ns) > 0 {
			selected = min(selected, len(ns)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(ns), start+limit); i++ {
			lines = append(lines, m.choiceLine(tuiPlain(ns[i].id)+fmt.Sprintf("  · %d records", ns[i].count), w-2, i == selected))
		}
	case "graph-neighbor":
		ns := m.neighborMatches(m.input.Value())
		if len(ns) > 0 {
			selected = min(selected, len(ns)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(ns), start+limit); i++ {
			lines = append(lines, m.choiceLine(tuiPlain(ns[i].n.Label), w-2, i == selected))
		}
	case "graph-source":
		ids := m.sourceMatches(m.input.Value())
		if len(ids) > 0 {
			selected = min(selected, len(ids)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(ids), start+limit); i++ {
			lines = append(lines, m.choiceLine(tuiPlain(ids[i]), w-2, i == selected))
		}
	case "recall":
		lines = append(lines, m.muted("Search in "+tuiPlain(m.namespace)), "", m.ink("Inspect the memories your agents would retrieve."), m.muted("Access counters stay unchanged. Your embedding provider may process the query."))
	case "filter":
		lines = append(lines, m.muted("Find text in this list · empty text clears the filter."))
	}
	return m.panel(title, strings.Join(lines, "\n"), w, h, true)
}
func (m tuiModel) renderModal(w, h int) string {
	title, body := "", ""
	switch m.modal {
	case "add":
		title = "Add memory · " + m.modalAgent
		body = m.editor.View()
	case "revise":
		title = "Revise memory · " + shortID(m.modalFact.ID)
		body = m.editor.View() + "\n\n" + m.muted("Ordinary pruning can remove the retired row. The replacement records its source version as evidence.")
	case "retire":
		title = "Retire this memory?"
		lines := strings.Split(ansi.Hardwrap(tuiPlain(m.modalFact.Text), max(1, w-6), false), "\n")
		limit := max(1, h-10)
		if len(lines) > limit {
			lines = append(lines[:limit], "… (excerpt; full text remains in the inspector)")
		}
		body = m.ink(strings.Join(lines, "\n")) + "\n\n" + m.tone("Leaves recall immediately. Ordinary pruning can remove the tombstone.", m.theme.Warning) + "\n\n" + m.muted("Namespace: "+tuiPlain(m.modalFact.AgentID)+"\nID: "+tuiPlain(m.modalFact.ID))
	case "kill":
		title = "Stop this running harness session?"
		body = m.ink(tuiPlain(m.modalSession)) + "\n\n" + m.tone("The session's process will be terminated.", m.theme.Warning)
	case "lineage":
		return m.renderLineage(w, h)
	case "help":
		title = "Keyboard & scope"
		body = "1–6 views · / or Ctrl+K commands · q quit\nTab / Shift+Tab focus · ↑↓ or j/k navigate\nEnter inspect · Esc return · y copy · Ctrl+L redraw\nMemory: n namespace · f filter · [ ] lifecycle\na add · e revise · p pin/unpin · d retire · Ctrl+N more\nRecall: Enter query (safe inspection)\nMemory and Recall: v inspect retained lineage\nActivity: c runs/checkpoints · x confirmed stop\nGraph: o neighbors · s supporting facts\nUsage: a auto · l limits · s spend · c context\nGraph is store-wide; Activity covers harness runs.\nNO_COLOR or --theme light|dark|terminal\nOptional --mouse; copy falls back to OSC52.\nEsc returns to your current selection."
	}
	if m.busy {
		body += "\n\n" + m.tone("◌ Saving…", m.theme.Info)
	}
	return m.panel(title, body, w, h, true)
}
