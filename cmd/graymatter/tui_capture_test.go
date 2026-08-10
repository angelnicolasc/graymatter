package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	graymatter "github.com/angelnicolasc/graymatter"
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
	if captureDir == "" {
		t.Log(content)
		return
	}
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
	lipgloss.SetColorProfile(termenv.TrueColor)

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
