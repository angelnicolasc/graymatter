package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// keyMsg builds a tea.KeyMsg for a single printable character.
func keyMsg(ch rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}}
}

// TestTUI_DeleteDisabledInReadOnly verifies that pressing 'd' in read-only
// mode sets a descriptive status message and does NOT attempt a store call.
func TestTUI_DeleteDisabledInReadOnly(t *testing.T) {
	m := tuiModel{
		readOnly: true,
		memPane:  memPaneFacts, // pane where 'd' would normally fire
	}

	_ = m.updateMemoryKey(keyMsg('d'))

	if !strings.Contains(m.status, "read-only") {
		t.Errorf("expected status to mention read-only, got %q", m.status)
	}
}

// TestTUI_DeleteAllowedInWriteMode verifies that the 'd' guard does NOT fire
// when the store is writable (normal path falls through to selection logic).
func TestTUI_DeleteAllowedInWriteMode(t *testing.T) {
	m := tuiModel{
		readOnly: false,
		memPane:  memPaneFacts,
		// factList and agentList are zero-value: SelectedItem() returns nil,
		// so the inner delete block is skipped — but no status is set either.
	}

	_ = m.updateMemoryKey(keyMsg('d'))

	if strings.Contains(m.status, "read-only") {
		t.Errorf("write-mode 'd' should not set read-only status, got %q", m.status)
	}
}

// TestTUI_KillDisabledInReadOnly verifies that pressing 'k' in read-only mode
// sets a descriptive status message and does NOT attempt a kill.
func TestTUI_KillDisabledInReadOnly(t *testing.T) {
	m := tuiModel{readOnly: true}

	_ = m.updateSessionsKey(keyMsg('k'))

	if !strings.Contains(m.status, "read-only") {
		t.Errorf("expected status to mention read-only, got %q", m.status)
	}
}

// TestTUI_KillAllowedInWriteMode verifies that the 'k' guard does NOT fire
// when the store is writable.
func TestTUI_KillAllowedInWriteMode(t *testing.T) {
	m := tuiModel{readOnly: false}

	_ = m.updateSessionsKey(keyMsg('k'))

	if strings.Contains(m.status, "read-only") {
		t.Errorf("write-mode 'k' should not set read-only status, got %q", m.status)
	}
}
