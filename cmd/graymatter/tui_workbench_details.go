package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/lineageview"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func (m tuiModel) detailSection(title, body string) string {
	return "\n\n" + m.heading(title) + "\n" + body
}

func (m tuiModel) factPreview(f memory.Fact) string {
	state := factState(f)
	c := m.theme.Success
	if f.IsSuperseded() {
		c = m.theme.Warning
	} else if f.IsAlias() {
		c = m.theme.Purple
	}
	badge := m.tone("● "+state, c)
	if f.Pinned {
		badge += m.tone("  ★ pinned", m.theme.Success)
	}
	out := m.ink(tuiPlain(f.Text)) + "\n\n" + badge
	out += "\n" + m.muted("Confidence: "+confidenceDetail(f.Confidence))
	out += m.detailSection("Memory details", m.muted(fmt.Sprintf("Created  %s\nAccessed %s · %d accesses\nRetention weight  %.4f", f.CreatedAt.Format("02 Jan 2006 · 15:04 MST"), f.AccessedAt.Format("02 Jan 2006 · 15:04 MST"), f.AccessCount, f.Weight)))
	if f.IsSuperseded() {
		out += "\n" + m.tone("Excluded from retrieval", m.theme.Warning) + "\n" + m.muted("Superseded by: "+tuiPlain(f.SupersededBy))
	}
	if f.IsAlias() {
		out += "\n" + m.muted("Vocabulary alias · not injectable\nSource: "+tuiPlain(f.AliasSource))
	}
	out += m.detailSection("Record", m.muted("ID: "+tuiPlain(f.ID)+"\nNamespace: "+tuiPlain(f.AgentID)))
	if f.Pinned {
		out += "\n" + m.muted("Pinned: yes · exempt from decay and pruning")
	}
	out += m.lineagePreview(f.LineageProjection)
	out += "\n\n" + m.muted("Weight measures retention.")
	return out
}

func (m tuiModel) receiptPreview(f memory.RecallReceipt) string {
	rank := func(v int) string {
		if v == 0 {
			return "—"
		}
		return fmt.Sprint(v)
	}
	out := m.ink(tuiPlain(f.Text))
	out += m.detailSection("Why this result", m.tone(fmt.Sprintf("Vector #%s   Keyword #%s   Recent #%s", rank(f.Ranks.VectorRank), rank(f.Ranks.KeywordRank), rank(f.Ranks.RecencyRank)), m.theme.Info))
	out += "\n" + m.muted(receiptScoreDetail(f))
	out += "\n\n" + m.muted("Confidence: "+confidenceDetail(f.Provenance.Confidence))
	if f.Provenance.Pinned {
		out += "\n" + m.tone("★ Pinned", m.theme.Success)
	}
	out += "\n" + m.muted(fmt.Sprintf("Retention %.4f · age %.1f days", f.Weight, f.AgeDays))
	out += m.detailSection("Receipt", m.muted("Query: "+tuiPlain(m.recallQuery)+"\nFact: "+tuiPlain(f.Provenance.FactID)+"\nWritten: "+f.Provenance.WrittenAt.Format(time.RFC3339)))
	if len(f.Provenance.Supersedes) > 0 {
		out += "\n" + m.muted("Supersedes IDs (compatibility field) · "+tuiPlain(strings.Join(f.Provenance.Supersedes, ", ")))
	}
	out += m.lineagePreview(f.Provenance.LineageProjection)
	out += "\n\n" + m.muted("Scores rank results; they are not probabilities.\nInspection does not update access counters.\nRanking may use your embedding provider.\nGraph enrichment is not included.")
	return out
}

func (m tuiModel) lineagePreview(p memory.LineageProjection) string {
	lines := lineageview.Lines(p)
	if len(lines) == 0 {
		return ""
	}
	lines = append(lines, "Namespace-scoped · v opens version inspection")
	return m.detailSection("Version lineage", m.muted(tuiPlain(strings.Join(lines, "\n"))))
}

func (m tuiModel) checkpointPreview(cp session.Checkpoint) string {
	out := m.ink(cp.CreatedAt.Format("02 Jan 2006 · 15:04 MST")) + "\n" + m.muted(tuiPlain(cp.AgentID))
	if len(cp.State) > 0 {
		keys := make([]string, 0, len(cp.State))
		for key := range cp.State {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var rows []string
		for _, key := range keys {
			value := cp.State[key]
			text, ok := value.(string)
			if !ok {
				b, _ := json.MarshalIndent(value, "", "  ")
				text = string(b)
			}
			rows = append(rows, m.muted(tuiPlain(key))+"\n"+m.ink(tuiPlain(text)))
		}
		out += m.detailSection("Saved state", strings.Join(rows, "\n\n"))
	}
	if len(cp.Messages) > 0 {
		var rows []string
		for _, msg := range cp.Messages {
			rows = append(rows, m.tone(tuiPlain(msg.Role), m.theme.Info)+"\n"+m.ink(tuiPlain(msg.Content)))
		}
		out += m.detailSection(fmt.Sprintf("Messages · %d", len(cp.Messages)), strings.Join(rows, "\n\n"))
	}
	out += m.detailSection("Record", m.muted("ID: "+tuiPlain(cp.ID)+"\nNamespace: "+tuiPlain(cp.AgentID)))
	if len(cp.Metadata) > 0 {
		b, _ := json.MarshalIndent(cp.Metadata, "", "  ")
		out += "\n" + m.muted(tuiPlain(string(b)))
	}
	return out
}

func (m tuiModel) graphPreview(n kg.Node) string {
	out := m.ink("Entity: "+tuiPlain(n.Label)) + "\n" + m.tone(tuiPlain(n.EntityType), m.theme.Purple)
	var edges []string
	for _, e := range m.graphEdges {
		if e.From != n.ID && e.To != n.ID {
			continue
		}
		direction, neighbor := "→ ", e.To
		if e.To == n.ID {
			direction, neighbor = "← ", e.From
		}
		line := m.ink(direction+tuiPlain(m.nodeLabel(neighbor))) + "\n  " + m.tone(tuiPlain(e.Relation), m.theme.Accent) + m.muted(fmt.Sprintf(" · %.3f", e.Weight))
		if len(e.Sources) == 0 {
			line += "\n  " + m.muted("source not recorded")
		} else {
			for _, id := range e.Sources {
				line += "\n  " + m.muted("fact "+tuiPlain(id))
			}
		}
		edges = append(edges, line)
	}
	if len(edges) == 0 {
		out += m.detailSection("Relationships", m.muted("No recorded neighbors."))
	} else {
		out += m.detailSection(fmt.Sprintf("Relationships · %d", len(edges)), strings.Join(edges, "\n\n"))
	}
	out += m.detailSection("Record", m.muted(fmt.Sprintf("ID: %s\nWeight: %.4f\nFirst: %s\nLast: %s", tuiPlain(n.ID), n.Weight, n.FirstSeen.Format(time.RFC3339), n.LastSeen.Format(time.RFC3339))))
	out += "\n\n" + m.muted("Store-wide graph · sources are retained receipts, not a complete history.")
	return out
}
