package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

// captureDir is where the ANSI dumps land. Unset, the test only logs, so it is
// inert in CI; set GM_CAPTURE_DIR to regenerate the README screenshots.
var captureDir = os.Getenv("GM_CAPTURE_DIR")

func seedForCapture(t *testing.T) cliStore {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	st := &directStore{mem: mem, store: mem.Advanced()}
	ctx := context.Background()

	seed := map[string][]string{
		"graymatter-backend": {
			"the team deploys on Fridays only after 2pm, never before",
			"auth tokens expire after 15 minutes; refresh happens client side",
			"bbolt is single-writer, daemon mode owns the lock",
			"CHANGELOG follows Keep a Changelog with Added/Changed/Fixed",
			"user prefers tables over prose for anything comparative",
			"prefer stdlib over new dependencies unless the win is large",
			"CI runs the race matrix on Linux, macOS and Windows",
		},
		"graymatter-frontend": {
			"design tokens live in tokens.css, never hardcode hex values",
			"the observability dashboard refreshes every 5 seconds",
			"prefers bullet points, dislikes long introductions",
			"lipgloss widths are measured in terminal cells, not runes",
		},
		"okuna-api": {
			"rate limit is 1000 req/min per key, burst 50",
			"all money amounts stored as integer cents",
		},
		"__shared__": {
			"all timestamps stored as UTC ISO-8601",
			"never commit secrets; use the environment",
			"branch off main; PRs only when external review is wanted",
		},
	}
	for agent, facts := range seed {
		for _, f := range facts {
			if err := st.Remember(ctx, agent, f); err != nil {
				t.Fatalf("Remember: %v", err)
			}
		}
	}

	_ = st.TokenRecord("graymatter-backend", "claude-opus-5", 1843200, 214000, 960000, 120000)
	_ = st.TokenRecord("graymatter-backend", "claude-haiku-4-5", 421000, 88000, 150000, 20000)
	_ = st.TokenRecord("graymatter-frontend", "claude-sonnet-5", 612000, 93000, 300000, 40000)
	_ = st.TokenRecord("okuna-api", "claude-opus-4-8", 288000, 41000, 130000, 18000)
	return st
}

func newCaptureTUI(t *testing.T, store cliStore) tuiModel {
	t.Helper()
	nl := func(title string) list.Model {
		l := list.New(nil, list.NewDefaultDelegate(), 40, 20)
		l.Title = title
		l.SetShowStatusBar(false)
		l.SetFilteringEnabled(true)
		return l
	}
	m := tuiModel{
		store:       store,
		width:       124,
		height:      36,
		agentList:   nl("Agents"),
		factList:    nl("Facts"),
		sessionList: nl("Sessions"),
		nodeList:    nl("KG Nodes"),
	}
	m.updateSizes()
	return m
}

func loadAll(t *testing.T, m tuiModel, agent string) tuiModel {
	t.Helper()
	for _, msg := range []interface{}{
		m.loadAgents()(), m.loadDashboard()(), m.loadSessions()(), m.loadNodes()(),
	} {
		if msg != nil {
			mm, _ := m.Update(msg)
			m = mm.(tuiModel)
		}
	}
	if agent != "" {
		if msg := m.loadFacts(agent)(); msg != nil {
			mm, _ := m.Update(msg)
			m = mm.(tuiModel)
		}
	}
	return m
}

func writeCapture(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(captureDir, name+".ans")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	t.Logf("wrote %s (%d bytes)", p, len(content))
}

// TestCapture renders the TUI in production-shaped states with colour forced on,
// so the dumps can be converted to an image. lipgloss disables colour outside a
// TTY, which is why the profile is set explicitly rather than relying on env.
func TestCapture(t *testing.T) {
	if captureDir == "" {
		t.Skip("set GM_CAPTURE_DIR to regenerate the dashboard screenshots")
	}
	// SetColorProfile is package-level state in lipgloss, so forcing colour here
	// would otherwise leak into every test that runs after this one in the same
	// package and make their rendered output depend on test order.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	store := seedForCapture(t)
	base := loadAll(t, newCaptureTUI(t, store), "graymatter-backend")

	m := base
	m.activeTab = tabMemory
	writeCapture(t, "01-memory", m.View())

	m = base
	m.activeTab = tabStats
	writeCapture(t, "02-stats", m.View())

	m = base
	m.activeTab = tabGraph
	writeCapture(t, "03-graph", m.View())

	// A transient failure: the UI stays usable, the header carries the warning.
	m = base
	m.activeTab = tabMemory
	broken, _ := m.Update(errMsg{errCaptureDead})
	writeCapture(t, "04-error-banner", broken.(tuiModel).View())

	// Store gone while on the dashboard.
	dead := newCaptureTUI(t, deadStore{})
	msg := dead.loadDashboard()()
	dead.dashboard = msg.(dashboardLoadedMsg).data
	dead.activeTab = tabStats
	d2, _ := dead.Update(errMsg{errCaptureDead})
	writeCapture(t, "05-store-unreachable", d2.(tuiModel).View())

	// A brand-new project: empty, but not alarming.
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	fresh, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	em := loadAll(t, newCaptureTUI(t, &directStore{mem: fresh, store: fresh.Advanced()}), "")
	em.activeTab = tabStats
	writeCapture(t, "06-empty", em.View())
}

var errCaptureDead = errCapture{}

type errCapture struct{}

func (errCapture) Error() string { return "connection is shut down" }

// TestTUI_GraphTabFitsViewport pins the whole-screen invariant: whatever the
// tab, View() must never render more lines than the terminal height, and the
// header (logo + tab bar) must stay visible. The Graph tab used to overflow
// because its stats header (3 rows) was never discounted from the pane
// heights, pushing the header off the top of the screen.
func TestTUI_GraphTabFitsViewport(t *testing.T) {
	st := seedForCapture(t)
	ds, ok := st.(*directStore)
	if !ok {
		t.Fatalf("expected *directStore")
	}
	g, err := kg.Open(ds.store.DB())
	if err != nil {
		t.Fatal(err)
	}
	for i, label := range []string{"bbolt", "chromem-go", "RRF", "daemon", "Obsidian", "MCP", "decay", "tombstone"} {
		if err := g.Upsert(kg.Node{ID: label, Label: label, EntityType: "concept", Weight: 1 - float64(i)*0.05}); err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range [][2]int{{124, 36}, {190, 44}, {100, 30}, {80, 24}} {
		m := newCaptureTUI(t, st)
		m.width, m.height = size[0], size[1]
		m.activeTab = tabGraph
		m.updateSizes()
		m = loadAll(t, m, "")
		out := m.View()
		lines := strings.Count(out, "\n") + 1
		if lines > m.height {
			t.Errorf("graph tab at %dx%d renders %d lines (viewport %d)", m.width, m.height, lines, m.height)
		}
		if !strings.Contains(out, "GRAYMATTER") {
			t.Errorf("graph tab at %dx%d lost the header", m.width, m.height)
		}
		if !strings.Contains(out, "1-4") {
			t.Errorf("graph tab at %dx%d lost the footer", m.width, m.height)
		}
		// The detail pane must show the highlighted node, never dead space.
		if !strings.Contains(out, "Entity:") {
			t.Errorf("graph tab at %dx%d has an empty detail pane", m.width, m.height)
		}
	}
}

// Pinned facts are visible in the Memory tab (ADR-010): star marker on the
// row and the flag in the detail pane.
func TestTUI_PinnedFactMarker(t *testing.T) {
	st := seedForCapture(t)
	m := newCaptureTUI(t, st)
	m = loadAll(t, m, "graymatter-backend")

	// Pin the selected fact directly on the model's list.
	items := m.factList.Items()
	if len(items) == 0 {
		t.Fatal("no facts loaded")
	}
	sel := items[0].(factItem)
	sel.fact.Pinned = true
	m.factList.SetItem(0, sel)
	m.memPane = memPaneFacts

	out := m.View()
	if !strings.Contains(out, "\u2605 ") {
		t.Error("pinned star marker missing from fact list")
	}
}
