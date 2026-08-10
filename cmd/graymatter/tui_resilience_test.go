package main

import (
	"context"
	"fmt"
	netrpc "net/rpc"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// deadStore behaves like a daemon connection that has gone away: every read
// fails the way net/rpc reports a closed connection.
type deadStore struct{ cliStore }

func (deadStore) ListAgents() ([]string, error)      { return nil, netrpc.ErrShutdown }
func (deadStore) List(string) ([]memory.Fact, error) { return nil, netrpc.ErrShutdown }
func (deadStore) Stats(string) (memory.MemoryStats, error) {
	return memory.MemoryStats{}, netrpc.ErrShutdown
}
func (deadStore) SessionsList() ([]harness.HarnessSession, error) {
	return nil, netrpc.ErrShutdown
}
func (deadStore) KGNodes() ([]kg.Node, error) { return nil, netrpc.ErrShutdown }
func (deadStore) IsReadOnly() bool            { return false }
func (deadStore) Close() error                { return nil }

// newTestTUI builds the model the way cmd_tui.go does, so widget state is real
// and a panic here would be the product's rather than the test's.
func newTestTUI(t *testing.T, store cliStore) tuiModel {
	t.Helper()
	nl := func(title string) list.Model {
		l := list.New(nil, list.NewDefaultDelegate(), 40, 20)
		l.Title = title
		return l
	}
	return tuiModel{
		store:       store,
		width:       120,
		height:      32,
		agentList:   nl("Agents"),
		factList:    nl("Facts"),
		sessionList: nl("Sessions"),
		nodeList:    nl("KG Nodes"),
		detail:      viewport.New(40, 20),
		nodeDetail:  viewport.New(40, 20),
	}
}

// TestTUI_ErrorDoesNotTakeOverTheScreen pins that a failed load leaves the UI
// usable. It used to replace everything with "Error: ... Press q to quit.",
// which ended the session for what was often a momentary failure.
func TestTUI_ErrorDoesNotTakeOverTheScreen(t *testing.T) {
	m := newTestTUI(t, deadStore{})

	msg := m.loadAgents()()
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("loadAgents returned %T, want errMsg", msg)
	}
	updated, _ := m.Update(em)
	view := updated.(tuiModel).View()

	if strings.Contains(view, "Press q to quit") {
		t.Error("the error still replaces the whole UI")
	}
	// The chrome must survive so the user can switch tabs and retry.
	for _, want := range []string{"GRAYMATTER", "refresh", "tabs"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lost %q; the UI is not usable after an error", want)
		}
	}
	// And the failure has to be visible somewhere.
	if !strings.Contains(view, "⚠") {
		t.Error("no error indicator in the view; the failure is invisible")
	}
}

// TestTUI_ErrorClearsOnRecovery is the other half: nothing used to reset m.err,
// so the UI stayed broken after the store came back.
func TestTUI_ErrorClearsOnRecovery(t *testing.T) {
	m := newTestTUI(t, deadStore{})

	broken, _ := m.Update(errMsg{netrpc.ErrShutdown})
	if broken.(tuiModel).err == nil {
		t.Fatal("error was not recorded")
	}

	recovered, _ := broken.(tuiModel).Update(agentsLoadedMsg{[]agentItem{{id: "a", count: 1}}})
	if got := recovered.(tuiModel).err; got != nil {
		t.Errorf("a successful load left the error in place: %v", got)
	}
	if strings.Contains(recovered.(tuiModel).View(), "⚠") {
		t.Error("the error indicator outlived the failure")
	}
}

// TestDashboard_ReportsUnreachableInsteadOfZeros is the finding that started
// this: with the store gone, every figure defaulted to zero and the panel drew
// them as though they were real. "Your memory is empty" and "the connection is
// dead" looked identical, which is exactly the confusion reported in #14.
func TestDashboard_ReportsUnreachableInsteadOfZeros(t *testing.T) {
	m := newTestTUI(t, deadStore{})

	msg := m.loadDashboard()()
	loaded, ok := msg.(dashboardLoadedMsg)
	if !ok {
		t.Fatalf("loadDashboard returned %T", msg)
	}
	if loaded.data.Err == nil {
		t.Fatal("dashboard reported success against a dead store")
	}

	m.dashboard = loaded.data
	out := m.renderDashboard(24)
	if !strings.Contains(out, "Store unreachable") {
		t.Errorf("dashboard does not say the store is unreachable:\n%s", out)
	}
	if !strings.Contains(out, "doctor") {
		t.Error("dashboard should point at `graymatter doctor` when it cannot read")
	}
}

// TestTUI_EmptyStoreIsNotAnError guards the opposite mistake. A genuinely empty
// project must stay quiet, or the warning becomes noise everyone learns to
// ignore.
func TestTUI_EmptyStoreIsNotAnError(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	m := newTestTUI(t, &directStore{mem: mem, store: mem.Advanced()})

	if msg := m.loadAgents()(); !isType[agentsLoadedMsg](msg) {
		t.Errorf("loadAgents on an empty store returned %T, want agentsLoadedMsg", msg)
	}
	if msg := m.loadSessions()(); !isType[sessionsLoadedMsg](msg) {
		t.Errorf("loadSessions on an empty store returned %T", msg)
	}
	if msg := m.loadNodes()(); !isType[nodesLoadedMsg](msg) {
		t.Errorf("loadNodes on an empty store returned %T", msg)
	}

	dash := m.loadDashboard()()
	d, ok := dash.(dashboardLoadedMsg)
	if !ok {
		t.Fatalf("loadDashboard returned %T", dash)
	}
	if d.data.Err != nil {
		t.Errorf("an empty store must not report as unreachable: %v", d.data.Err)
	}

	m.dashboard = d.data
	if out := m.renderDashboard(24); strings.Contains(out, "Store unreachable") {
		t.Error("empty store rendered as unreachable")
	}
	_ = context.Background()
}

func isType[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

// TestKPIBlock_NeverWraps pins the layout invariant. The KPI strip is joined on
// the top edge, so one tile that wraps to a second line leaves the other three
// with their bottom borders a row high. Widths are measured in terminal cells
// rather than runes, because the dashboard's box-drawing and block glyphs are
// multi-byte.
// The contract is bounded by what the dashboard's own formatters can emit, so
// the values here come from those formatters at extremes rather than from
// invented strings. A value wider than its tile is out of contract: the code
// deliberately clips the caption and never the number, because a half-printed
// figure misleads where a half-printed caption only annoys.
func TestKPIBlock_NeverWraps(t *testing.T) {
	values := []string{
		formatCompact(0),
		formatCompact(999),
		formatCompact(1_500_000),
		formatCompact(987_654_321),
		formatBytes(0),
		formatBytes(486),
		formatBytes(9_876_543_210),
		"100%",
	}
	units := []string{
		"",
		"above 0.5 weight",
		"text + embeddings",
		"Σ access count", // multi-byte
		fmt.Sprintf("across %d agents", 128),
	}

	// The narrowest tile the layout can produce: renderKPIRow derives tileW from
	// the terminal width, and the dashboard floors its own width at 40.
	for _, w := range []int{18, 22, 25, 30, 48} {
		for _, v := range values {
			for _, u := range units {
				out := kpiBlock("MEMORY COST", v, u, colorCyan, w)
				if got := lipgloss.Height(out); got != 4 {
					t.Errorf("width=%d value=%q unit=%q: height %d, want 4 (label+value inside a border)\n%s",
						w, v, u, got, out)
				}
			}
		}
	}
}

func TestTruncateCells(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want int // expected cell width, at most max
	}{
		{"short", 20, 5},
		{"exactly-ten", 11, 11},
		{"truncate me please", 8, 8},
		{"Σ access count", 6, 6}, // multi-byte
		{"anything", 0, 0},
		{"anything", 1, 1},
	} {
		got := truncateCells(c.in, c.max)
		if w := lipgloss.Width(got); w != c.want {
			t.Errorf("truncateCells(%q, %d) = %q (width %d), want width %d", c.in, c.max, got, w, c.want)
		}
		if lipgloss.Width(got) > c.max {
			t.Errorf("truncateCells(%q, %d) exceeded max: %q", c.in, c.max, got)
		}
	}
}
