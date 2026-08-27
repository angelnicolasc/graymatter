package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/benchsyn"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

// hooks run <event> is what Claude Code executes on every hook event. The
// contract, verified against Claude Code's hook documentation:
//
//   - Input: one JSON object on stdin with session_id, transcript_path, cwd,
//     hook_event_name, plus event-specific fields (source for SessionStart,
//     prompt for UserPromptSubmit).
//   - Output: exit 0 and plain text on stdout → the text is added to Claude's
//     context (SessionStart and UserPromptSubmit). For the other two events
//     stdout is not injected, so the runners print nothing.
//   - Every error path: exit 0, empty stdout, one JSON line appended to
//     <dataDir>/hooks.log. A memory system that breaks must never break the
//     session it serves — degrade silently, leave a receipt in the log.

const (
	// hookLatencyBudget is the per-turn budget the user-prompt hook must meet
	// (playbook: < 150 ms p99 with a 10k-fact store). doctor warns above it.
	// The budgets live in internal/benchsyn (the CLI's single source, audited
	// by `graymatter bench --hooks` and the benchmarks/hook_latency CI gate);
	// this is the alias the doctor's store check gates on.
	hookLatencyBudget = benchsyn.HookUserPromptBudget

	// hookStdinMax caps how much stdin we are willing to read. Hook payloads
	// are small JSON objects; anything beyond this is not a hook payload.
	hookStdinMax = 1 << 20

	// hooksStateFile caches the last injected block's hash so an identical
	// recall is not re-injected on consecutive turns.
	hooksStateFile = "state.json"

	// hooksRememberPrefix turns a prompt into a deterministic instant-save,
	// no model in the loop.
	hooksRememberPrefix = "remember:"
)

// timeNow is the process clock, a seam so latency paths are testable.
var timeNow = time.Now

// hookLatencyTargets documents the budgets each runner answers within; the
// benchmark in benchmarks/hook_latency gates them.
const (
	hookUserPromptBudget = 150 * time.Millisecond
	hookSessionEndBudget = 500 * time.Millisecond
	hookPreCompactBudget = 200 * time.Millisecond
)

func hooksRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <event>",
		Short: "Execute a hook event handler (called by Claude Code, not by you)",
		Long: `Handles one Claude Code hook event. Reads the event JSON from stdin
and writes injectable context to stdout.

Events: session-start, user-prompt, pre-compact, session-end.

Every failure exits 0 with empty stdout and logs to <dataDir>/hooks.log —
hooks must degrade silently, never break the session.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"session-start", "user-prompt", "pre-compact", "session-end"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHookEvent(args[0])
		},
	}
	return cmd
}

// hookEventPayload is the union of fields Claude Code sends across the four
// events. Only the relevant ones are read per event.
type hookEventPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"`
	Prompt         string `json:"prompt"`
}

// readHookPayload reads and parses the event JSON from stdin. An empty or
// unparsable payload is an empty struct, never an error — runners still work
// (with cwd from the process) when Claude Code sends nothing. A UTF-8 BOM is
// stripped before parsing: nothing sane sends one, but a payload produced
// through a Windows text pipeline may carry it, and refusing to parse over
// three bytes would silently disable every hook.
func readHookPayload(r io.Reader) hookEventPayload {
	var p hookEventPayload
	data, err := io.ReadAll(io.LimitReader(r, hookStdinMax))
	if err != nil || len(data) == 0 {
		return p
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	_ = json.Unmarshal(data, &p) // unparsable JSON leaves the zero value
	return p
}

// runHookEvent dispatches one event, enforcing the exit-0/silent-failure
// contract in exactly one place.
func runHookEvent(event string) error {
	start := timeNow()
	payload := readHookPayload(os.Stdin)

	out, err := dispatchHook(event, payload)
	elapsed := timeNow().Sub(start)

	if err != nil {
		hookLog(payload, event, elapsed, "error", err.Error())
		return nil // exit 0, stdout untouched
	}
	if out != "" {
		fmt.Println(out)
	}
	// Detail wording: the pre-compact and session-end runners produce no
	// injection by contract (their stdout is not added to Claude's context);
	// what they produce is a checkpoint. Compare against the CLI event names
	// runHookEvent dispatches on, not the settings event keys.
	detail := "injected"
	if out == "" {
		detail = "nothing to inject"
	}
	switch event {
	case "pre-compact", "session-end":
		detail = "checkpointed"
	}
	hookLog(payload, event, elapsed, "ok", detail)
	return nil
}

// dispatchHook runs the event's handler and returns the text to inject.
func dispatchHook(event string, payload hookEventPayload) (string, error) {
	agent := deriveAgentID(hookCWD(payload))
	switch event {
	case "session-start":
		return hookSessionStart(agent, payload)
	case "user-prompt":
		return hookUserPrompt(agent, payload)
	case "pre-compact":
		return hookCheckpoint(agent, payload, "pre-compact")
	case "session-end":
		return hookSessionEnd(agent, payload)
	default:
		return "", fmt.Errorf("unknown hook event %q (want session-start, user-prompt, pre-compact, session-end)", event)
	}
}

// hookCWD picks the working directory the event reports, falling back to the
// process's own.
func hookCWD(payload hookEventPayload) string {
	if payload.CWD != "" {
		return payload.CWD
	}
	return mustWorkdir()
}

// mustWorkdir returns the process working directory, or "" when it cannot be
// read (every caller treats that as "use a placeholder agent id and log").
func mustWorkdir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

var hookAgentSanitize = regexp.MustCompile(`[^a-z0-9]+`)

// deriveAgentID maps a directory to the agent id the hooks recall and store
// under: the folder's base name, lowercased, with runs of non-alphanumerics
// collapsed to one dash. "C:\code\My Project" → "my-project". The same folder
// always maps to the same agent, so the hooks and the CLI agree by construction
// as long as the CLI passes the same id.
//
// path.Base (slash-based, not filepath) does the basename work so Windows
// paths — where filepath.Base's UNC handling misreads a double leading slash —
// and POSIX paths normalise identically.
func deriveAgentID(dir string) string {
	norm := strings.ReplaceAll(dir, "\\", "/")
	id := strings.Trim(hookAgentSanitize.ReplaceAllString(strings.ToLower(path.Base(norm)), "-"), "-")
	if id == "" {
		id = "project"
	}
	return id
}

// --- event handlers ------------------------------------------------------------

// hookSessionStart recalls the agent's top facts and renders them for
// injection. Query "" makes recency the only ranked signal (there are no
// query terms to match), which is exactly "start where the last session
// ended": the freshest live facts, deterministic order.
func hookSessionStart(agent string, payload hookEventPayload) (string, error) {
	store, err := openStore()
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	facts, err := store.Recall(context.Background(), agent, "", 5)
	if err != nil {
		return "", fmt.Errorf("recall: %w", err)
	}
	if len(facts) == 0 {
		return "", nil // never noise: an empty memory injects nothing
	}
	return renderMemoryBlock(facts), nil
}

// hookUserPrompt either performs a deterministic remember (prompt starts with
// "remember:") or a short recall. Consecutive turns with identical recall
// output are suppressed via the state file: the context is already there.
func hookUserPrompt(agent string, payload hookEventPayload) (string, error) {
	prompt := strings.TrimSpace(payload.Prompt)
	if prompt == "" {
		return "", nil
	}

	// Instant-save path: "remember: <text>".
	if rest, ok := hooksRememberRest(prompt); ok {
		if rest == "" {
			return "", fmt.Errorf("remember: with no text")
		}
		store, err := openStore()
		if err != nil {
			return "", fmt.Errorf("open store: %w", err)
		}
		defer func() { _ = store.Close() }()
		if err := store.Remember(context.Background(), agent, rest); err != nil {
			return "", fmt.Errorf("remember: %w", err)
		}
		return fmt.Sprintf("Saved to memory (%s): %s", agent, rest), nil
	}

	store, err := openStore()
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	facts, err := store.Recall(context.Background(), agent, prompt, 3)
	if err != nil {
		return "", fmt.Errorf("recall: %w", err)
	}
	if len(facts) == 0 {
		return "", nil
	}
	block := renderMemoryBlock(facts)
	if hookStateSeenBlock(agent, block) {
		return "", nil // identical context already injected on a recent turn
	}
	hookStateRecordBlock(agent, block)
	return block, nil
}

// hooksRememberRest strips the remember: prefix case-insensitively. Returns
// the remaining text and whether the prefix was present.
func hooksRememberRest(prompt string) (string, bool) {
	if len(prompt) < len(hooksRememberPrefix) {
		return "", false
	}
	if !strings.EqualFold(prompt[:len(hooksRememberPrefix)], hooksRememberPrefix) {
		return "", false
	}
	return strings.TrimSpace(prompt[len(hooksRememberPrefix):]), true
}

// hookCheckpoint is the pre-compact runner: one deterministic checkpoint, no
// LLM, well inside the 200 ms budget.
func hookCheckpoint(agent string, payload hookEventPayload, event string) (string, error) {
	store, err := openStore()
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	_, err = store.CheckpointSave(sessionCheckpointFor(agent, payload, event))
	if err != nil {
		return "", fmt.Errorf("checkpoint save: %w", err)
	}
	return "", nil
}

// hookSessionEnd checkpoints and then spawns consolidation detached: the
// child survives the editor closing, and this process returns inside the
// session-end budget. Spawn failure is logged by the caller — consolidation
// missing once is a lost optimisation, not a broken session.
func hookSessionEnd(agent string, payload hookEventPayload) (string, error) {
	store, err := openStore()
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	if _, err := store.CheckpointSave(sessionCheckpointFor(agent, payload, "session-end")); err != nil {
		_ = store.Close()
		return "", fmt.Errorf("checkpoint save: %w", err)
	}
	if err := store.Close(); err != nil {
		return "", fmt.Errorf("close store: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own binary: %w", err)
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	cmd := exec.Command(exe, "consolidate", agent, "--dir", dir)
	cmd.SysProcAttr = detachSysProcAttr()
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("spawn consolidate: %w", err)
	}
	_ = cmd.Process.Release() // detach fully: no zombie, no wait
	return "", nil
}

// sessionCheckpointFor builds the checkpoint both hook events save. Deterministic:
// no messages, no state beyond provenance of why it exists.
func sessionCheckpointFor(agent string, payload hookEventPayload, event string) session.Checkpoint {
	return session.Checkpoint{
		AgentID: agent,
		State: map[string]any{
			"source":     "hooks",
			"event":      event,
			"session_id": payload.SessionID,
		},
		Metadata: map[string]string{
			"source":     "hooks",
			"event":      event,
			"session_id": payload.SessionID,
		},
	}
}

// --- rendering -----------------------------------------------------------------

// renderMemoryBlock renders the injected context. Plain text (Claude Code
// accepts plain text or JSON; plain keeps the block human-readable in the
// transcript too). Facts longer than one line are folded — the block must stay
// skimmable inside a prompt.
func renderMemoryBlock(facts []string) string {
	var sb strings.Builder
	sb.WriteString("## Memory\n")
	for _, f := range facts {
		line := strings.ReplaceAll(strings.TrimSpace(f), "\n", " ")
		sb.WriteString("- " + line + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// --- state + log ----------------------------------------------------------------

// hookStatePath resolves <dataDir>/hooks/state.json.
func hookStatePath(dataDir string) string {
	return filepath.Join(dataDir, "hooks", hooksStateFile)
}

// hookStateSeenBlock reports whether this exact block was the last one
// injected for this agent. Read failures mean "not seen" — a missing cache
// only causes a redundant injection.
func hookStateSeenBlock(agent, block string) bool {
	data, err := os.ReadFile(hookStatePath(dataDir))
	if err != nil {
		return false
	}
	var st map[string]string
	if err := json.Unmarshal(data, &st); err != nil {
		return false
	}
	return st[hookStateKey(agent)] == hookBlockHash(block)
}

// hookStateRecordBlock stores the block's hash as the last injected one for
// this agent. Best-effort: a failure to write only costs a duplicate injection.
func hookStateRecordBlock(agent, block string) {
	path := hookStatePath(dataDir)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

	st := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st) // keep other agents' entries when readable
	}
	st[hookStateKey(agent)] = hookBlockHash(block)
	if out, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(path, out, 0o644)
	}
}

// hookStateKey namespaces the hash per agent so two agents in one project do
// not suppress each other's injections.
func hookStateKey(agent string) string { return "last_block_sha256:" + agent }

func hookBlockHash(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])
}

// hookLog appends one JSON line to <dataDir>/hooks.log. Best-effort by
// contract: a logging failure must not turn into a hook failure.
func hookLog(payload hookEventPayload, event string, elapsed time.Duration, outcome, detail string) {
	path := filepath.Join(dataDir, "hooks.log")
	entry := map[string]any{
		"ts":       timeNow().UTC().Format(time.RFC3339),
		"event":    event,
		"outcome":  outcome,
		"ms":       elapsed.Milliseconds(),
		"detail":   detail,
		"agent":    deriveAgentID(hookCWD(payload)),
		"session":  payload.SessionID,
	}
	if payload.Source != "" {
		entry["source"] = payload.Source
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}
