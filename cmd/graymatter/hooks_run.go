package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/benchsyn"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
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
//   - Runtime handler errors: exit 0, empty stdout, with a receipt in the
//     selected store's hooks.log. Invalid routing, unsafe database leaves,
//     and no-create rejections stop before any store-side write.

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

	// hooksStateFile caches recent injected block hashes so an identical recall
	// is not re-injected on consecutive turns in the same session.
	hooksStateFile = "state.json"

	hookStateSessionLimit  = 32
	hookStateSessionPrefix = "last_block_sha256:v2:"

	// hooksRememberPrefix turns a prompt into a deterministic instant-save,
	// no model in the loop.
	hooksRememberPrefix = "remember:"

	// hooksRememberSharedPrefix is the same instant-save aimed at the
	// __shared__ namespace every agent reads: "remember shared: <text>".
	// Checked before hooksRememberPrefix, which it does not collide with
	// ("remember " carries a space where "remember:" carries a colon).
	hooksRememberSharedPrefix = "remember shared:"

	// hookRecallMarkerPrefix identifies a fresh hook-recall block. The rendered
	// marker also names the hook's actual namespace; MCP guidance can therefore
	// skip only a matching, non-empty section instead of hiding facts stored
	// under a role-suffixed or otherwise different agent_id.
	hookRecallMarkerPrefix = "[GrayMatter hook recall ran for agent_id="
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

// Injection budgets per event and namespace. The two namespaces get separate
// budgets, not one merged top-k: project conventions are old by nature, and
// on session-start — where an empty query leaves recency as the only ranked
// signal — a merged ranking would let them displace the agent's freshest
// facts. Shared gets its own ceiling instead, so it can never cannibalize the
// agent's history. Session-start totals top-8, the injection size
// `graymatter status` quotes an estimate for.
const (
	hookSessionStartAgentTopK  = 5
	hookSessionStartSharedTopK = 3

	hookUserPromptAgentTopK  = 3
	hookUserPromptSharedTopK = 3
)

func hooksRunCmd() *cobra.Command {
	var noCreate bool
	var packetPolicy string
	cmd := &cobra.Command{
		Use:   "run <event>",
		Short: "Execute a hook event handler (called by Claude Code, not by you)",
		Long: `Handles one Claude Code hook event. Reads the event JSON from stdin
and writes injectable context to stdout.

Events: session-start, user-prompt, pre-compact, session-end.

Failures exit 0 with empty stdout. Handler errors log to the selected store;
invalid routing, unsafe database entries, and --no-create rejections stop
before opening or logging to a store.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"session-start", "user-prompt", "pre-compact", "session-end"},
		RunE: func(cmd *cobra.Command, args []string) error {
			dirFlag := cmd.Flag("dir")
			return runHookEventWithPolicySelected(args[0], noCreate, packetPolicy, dirFlag != nil && dirFlag.Changed)
		},
	}
	cmd.Flags().StringVar(&packetPolicy, "packet-policy", "native", "prompt context selection: native or experimental lexical")
	cmd.Flags().BoolVar(&noCreate, "no-create", false, "skip without opening or logging when the selected store is unprepared")
	cmd.Flags().Bool("graymatter-managed-hook", false, "mark a command managed by GrayMatter")
	_ = cmd.Flags().MarkHidden("graymatter-managed-hook")
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

// readHookPayload is the compatibility reader for callers that only need an
// empty value on malformed input. The command uses the checked reader below:
// malformed project signals must never fall back to the process directory.
func readHookPayload(r io.Reader) hookEventPayload {
	p, _ := readHookPayloadChecked(r)
	return p
}

func readHookPayloadChecked(r io.Reader) (hookEventPayload, error) {
	var p hookEventPayload
	data, err := io.ReadAll(io.LimitReader(r, hookStdinMax+1))
	if err != nil {
		return p, err
	}
	if len(data) > hookStdinMax {
		return p, fmt.Errorf("hook payload exceeds %d bytes", hookStdinMax)
	}
	if len(data) == 0 {
		return p, nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return p, fmt.Errorf("hook payload must be a JSON object")
	}
	var cwdSeen bool
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return p, fmt.Errorf("hook payload must be a JSON object")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return p, fmt.Errorf("hook payload must be a JSON object")
		}
		name, ok := key.(string)
		if !ok || !strings.EqualFold(name, "cwd") {
			continue
		}
		if name != "cwd" || cwdSeen {
			return p, fmt.Errorf("hook payload cwd must be an unambiguous lowercase field")
		}
		cwdSeen = true
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return p, fmt.Errorf("hook payload cwd must be a string")
		}
		var cwd string
		if err := json.Unmarshal(value, &cwd); err != nil {
			return p, fmt.Errorf("hook payload cwd must be a string")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return p, fmt.Errorf("hook payload must be a JSON object")
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return hookEventPayload{}, fmt.Errorf("hook payload has invalid field types")
	}
	return p, nil
}

// runHookEvent dispatches one event, enforcing the exit-0/silent-failure
// contract in exactly one place.
func runHookEvent(event string) error {
	return runHookEventWithNoCreate(event, false)
}

func runHookEventWithNoCreate(event string, noCreate bool) error {
	return runHookEventWithPolicy(event, noCreate, "native")
}

func runHookEventWithPolicy(event string, noCreate bool, policy string) error {
	// Direct callers use the package-level directory as an explicit selection.
	// The Cobra entrypoint passes the flag's actual Changed bit instead.
	return runHookEventWithPolicySelected(event, noCreate, policy, true)
}

func runHookEventWithPolicySelected(event string, noCreate bool, policy string, dirChanged bool) error {
	start := timeNow()
	payload, payloadErr := readHookPayloadChecked(os.Stdin)
	if payloadErr != nil {
		fmt.Fprintln(os.Stderr, "graymatter hook skipped: invalid event payload")
		return nil // fail soft without guessing a project or writing a store
	}
	cwd, cwdErr := os.Getwd()
	claudeRoot, claudeRootPresent := os.LookupEnv("CLAUDE_PROJECT_DIR")
	route, routeErr := resolveRuntimeContext(runtimeContextInput{
		configuredDir: dataDir, dirChanged: dirChanged,
		capturedCWD: cwd, cwdErr: cwdErr,
		claudeProjectDir: claudeRoot, claudeProjectDirPresent: claudeRootPresent,
		payloadCWD: payload.CWD, transport: runtimeHook,
	})
	if routeErr != nil {
		fmt.Fprintln(os.Stderr, "graymatter hook skipped: invalid or conflicting project route")
		return nil // fail soft without writing to a possibly wrong store
	}
	if _, err := inspectRuntimeStore(route.storeDir, noCreate); err != nil {
		if !errors.Is(err, errRuntimeStoreUnprepared) {
			fmt.Fprintln(os.Stderr, "graymatter hook skipped: invalid store entry")
		}
		return nil
	}

	if !validHookPacketPolicy(policy) {
		hookLogAt(route, payload, event, timeNow().Sub(start), "error", "invalid packet policy")
		return nil
	}
	out, err := dispatchHookWithPolicyAt(route, event, payload, policy)
	elapsed := timeNow().Sub(start)

	if err != nil {
		hookLogAt(route, payload, event, elapsed, "error", err.Error())
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
	hookLogAt(route, payload, event, elapsed, "ok", detail)
	return nil
}

// dispatchHook runs the event's handler and returns the text to inject.
func dispatchHook(event string, payload hookEventPayload) (string, error) {
	return dispatchHookWithPolicy(event, payload, "native")
}

func dispatchHookWithPolicy(event string, payload hookEventPayload, policy string) (string, error) {
	route := runtimeContext{storeDir: dataDir, agentID: deriveAgentID(hookCWD(payload))}
	return dispatchHookWithPolicyAt(route, event, payload, policy)
}

func dispatchHookWithPolicyAt(route runtimeContext, event string, payload hookEventPayload, policy string) (string, error) {
	switch event {
	case "session-start":
		return hookSessionStartAt(route, payload)
	case "user-prompt":
		return hookUserPromptWithPolicyAt(route, payload, policy)
	case "pre-compact":
		return hookCheckpointAt(route, payload, "pre-compact")
	case "session-end":
		return hookSessionEndAt(route, payload)
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

// hookSessionStart recalls the agent's top facts plus the __shared__ project
// conventions and renders them for injection. Query "" makes recency the only
// ranked signal (there are no query terms to match), which is exactly "start
// where the last session ended": the freshest live facts, deterministic order.
func hookSessionStart(agent string, payload hookEventPayload) (string, error) {
	return hookSessionStartAt(runtimeContext{storeDir: dataDir, agentID: agent}, payload)
}

func hookSessionStartAt(route runtimeContext, payload hookEventPayload) (string, error) {
	start := timeNow()
	store, err := openStoreAt(route.storeDir)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	block, degrade := hookRecallBlock(store, route.agentID, "", hookSessionStartAgentTopK, hookSessionStartSharedTopK)
	if block == "" {
		if degrade != nil {
			return "", degrade
		}
		return "", nil // never noise: an empty memory injects nothing
	}
	if degrade != nil {
		hookLogAt(route, payload, "session-start", timeNow().Sub(start), "error", degrade.Error())
	}
	return block, nil
}

// hookUserPrompt either performs a deterministic remember (prompt starts with
// "remember:" or "remember shared:") or a short recall from both namespaces.
// Consecutive turns with identical recall output in the same identified
// session are suppressed via the state file: the context is already there.
func hookUserPrompt(agent string, payload hookEventPayload) (string, error) {
	return hookUserPromptWithPolicy(agent, payload, "native")
}

func hookUserPromptWithPolicy(agent string, payload hookEventPayload, policy string) (string, error) {
	return hookUserPromptWithPolicyAt(runtimeContext{storeDir: dataDir, agentID: agent}, payload, policy)
}

func hookUserPromptWithPolicyAt(route runtimeContext, payload hookEventPayload, policy string) (string, error) {
	prompt := strings.TrimSpace(payload.Prompt)
	if prompt == "" {
		return "", nil
	}

	// Instant-save to the project-wide namespace: "remember shared: <text>".
	if rest, ok := hookPromptRest(prompt, hooksRememberSharedPrefix); ok {
		if rest == "" {
			return "", fmt.Errorf("remember shared: with no text")
		}
		store, err := openStoreAt(route.storeDir)
		if err != nil {
			return "", fmt.Errorf("open store: %w", err)
		}
		defer func() { _ = store.Close() }()
		if err := store.PutShared(context.Background(), rest); err != nil {
			return "", fmt.Errorf("remember shared: %w", err)
		}
		return fmt.Sprintf("Saved to shared memory: %s", rest), nil
	}

	// Instant-save to the cwd agent: "remember: <text>".
	if rest, ok := hookPromptRest(prompt, hooksRememberPrefix); ok {
		if rest == "" {
			return "", fmt.Errorf("remember: with no text")
		}
		store, err := openStoreAt(route.storeDir)
		if err != nil {
			return "", fmt.Errorf("open store: %w", err)
		}
		defer func() { _ = store.Close() }()
		if err := store.Remember(context.Background(), route.agentID, rest); err != nil {
			return "", fmt.Errorf("remember: %w", err)
		}
		return fmt.Sprintf("Saved to memory (%s): %s", route.agentID, rest), nil
	}

	start := timeNow()
	selectionDeadline := time.Now().Add(time.Duration(userPromptHookTimeout-1) * time.Second)
	store, err := openStoreAt(route.storeDir)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	var block string
	var degrade error
	if policy == "lexical" {
		// Connection time consumes the selection budget too. Store opening
		// retains its existing transport limits; do not start more work late.
		ctx, cancel := context.WithDeadline(context.Background(), selectionDeadline)
		defer cancel()
		var receipt hookPacketReceipt
		block, receipt, degrade = hookRecallLexicalBlock(ctx, store, route.agentID, prompt)
		detail, _ := json.Marshal(receipt)
		hookLogAt(route, payload, "user-prompt", timeNow().Sub(start), "packet", string(detail))
	} else {
		block, degrade = hookRecallBlock(store, route.agentID, prompt, hookUserPromptAgentTopK, hookUserPromptSharedTopK)
	}
	if block == "" {
		if degrade != nil {
			return "", degrade
		}
		return "", nil
	}
	if degrade != nil {
		hookLogAt(route, payload, "user-prompt", timeNow().Sub(start), "error", degrade.Error())
	}
	if hookStateSeenBlockAt(route.storeDir, route.agentID, payload.SessionID, block) {
		return "", nil // identical context is already present in this session
	}
	hookStateRecordBlockAt(route.storeDir, route.agentID, payload.SessionID, block)
	return block, nil
}

// hookPromptRest strips a prefix case-insensitively, returning the remaining
// text and whether the prefix was present.
func hookPromptRest(prompt, prefix string) (string, bool) {
	if len(prompt) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(prompt[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(prompt[len(prefix):]), true
}

// hookRecallBlock recalls one injection's worth of context: the agent's own
// facts plus the __shared__ namespace, each under its own budget. A var seam
// (timeNow precedent) so the degradation paths are testable without a store
// that fails selectively.
//
// Degradation contract: one namespace failing never costs the other's facts —
// the block comes back carrying whichever side still answered, along with a
// non-nil degrade error the caller turns into an error receipt in hooks.log
// while injecting the block anyway. Only when nothing injectable survived
// (both namespaces failed, or the only populated one did) is the error a hard
// failure that aborts the injection.
var hookRecallBlock = func(store cliStore, agent, query string, agentTopK, sharedTopK int) (string, error) {
	ctx := context.Background()
	agentFacts, agentErr := hookRecallProjection(ctx, store, agent, query, agentTopK, false)
	sharedFacts, sharedErr := hookRecallProjection(ctx, store, agent, query, sharedTopK, true)
	withWarning := func(block string, err error) (string, error) {
		issueCtx, cancel := context.WithTimeout(ctx, time.Duration(userPromptHookTimeout-1)*time.Second)
		defer cancel()
		block, issuesErr := hookSubjectWarning(issueCtx, store, agent, block)
		return block, errors.Join(err, issuesErr)
	}

	switch {
	case agentErr != nil && sharedErr != nil:
		return "", fmt.Errorf("recall agent: %v; recall shared: %v", agentErr, sharedErr)
	case agentErr != nil:
		block := renderMemoryBlock(agent, nil, sharedFacts)
		if block == "" {
			return "", fmt.Errorf("recall agent: %w", agentErr)
		}
		return withWarning(block, fmt.Errorf("agent recall failed, injecting shared facts only: %w", agentErr))
	case sharedErr != nil:
		block := renderMemoryBlock(agent, agentFacts, nil)
		if block == "" {
			return "", fmt.Errorf("recall shared: %w", sharedErr)
		}
		return withWarning(block, fmt.Errorf("shared recall failed, injecting agent facts only: %w", sharedErr))
	}
	return withWarning(renderMemoryBlock(agent, agentFacts, sharedFacts), nil)
}

func hookSubjectWarning(ctx context.Context, store cliStore, agent, block string) (string, error) {
	issues, err := readSubjectIssues(ctx, store, agent, memory.SharedAgentID)
	if err != nil {
		return block, fmt.Errorf("subject diagnostics: %w", err)
	}
	if warning := issues.Warning(); warning != "" {
		if block == "" {
			block = hookRecallMarker(agent)
		}
		block += "\n" + warning
	}
	return block, nil
}

// hookCheckpoint is the pre-compact runner: one deterministic checkpoint, no
// LLM, well inside the 200 ms budget.
func hookCheckpoint(agent string, payload hookEventPayload, event string) (string, error) {
	return hookCheckpointAt(runtimeContext{storeDir: dataDir, agentID: agent}, payload, event)
}

func hookCheckpointAt(route runtimeContext, payload hookEventPayload, event string) (string, error) {
	store, err := openStoreAt(route.storeDir)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	_, err = store.CheckpointSave(sessionCheckpointFor(route.agentID, payload, event))
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
	return hookSessionEndAt(runtimeContext{storeDir: dataDir, agentID: agent}, payload)
}

func hookSessionEndAt(route runtimeContext, payload hookEventPayload) (string, error) {
	store, err := openStoreAt(route.storeDir)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	if _, err := store.CheckpointSave(sessionCheckpointFor(route.agentID, payload, "session-end")); err != nil {
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
	cmd := exec.Command(exe, "consolidate", route.agentID, "--dir", route.storeDir)
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
// transcript too). A non-empty block starts with a marker naming the exact
// agent namespace the hook queried. MCP guidance can then reuse only matching,
// non-empty sections from the newest applicable hook block. Two
// labeled sections follow — the agent's own facts, then the project-wide
// __shared__ conventions — because a model consuming the block must tell its
// own history from standing project rules. Facts longer than one line are
// folded, and text stored in both namespaces renders once under Memory.
func renderMemoryBlock(agent string, agentFacts, sharedFacts []string) string {
	var sb strings.Builder
	seen := make(map[string]bool, len(agentFacts)+len(sharedFacts))
	render := func(header string, facts []string) {
		lines := make([]string, 0, len(facts))
		for _, f := range facts {
			line := strings.ReplaceAll(strings.TrimSpace(f), "\n", " ")
			if line == "" || seen[line] {
				continue
			}
			seen[line] = true
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			return
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(header + "\n")
		for _, line := range lines {
			sb.WriteString("- " + line + "\n")
		}
	}
	render("## Memory", agentFacts)
	render("## Shared memory (project-wide)", sharedFacts)
	if sb.Len() == 0 {
		return ""
	}
	return hookRecallMarker(agent) + "\n" + strings.TrimRight(sb.String(), "\n")
}

func hookRecallMarker(agent string) string {
	return fmt.Sprintf("%s%q.]", hookRecallMarkerPrefix, agent)
}

// --- state + log ----------------------------------------------------------------

// hookStatePath resolves <dataDir>/hooks/state.json.
func hookStatePath(dataDir string) string {
	return filepath.Join(dataDir, "hooks", hooksStateFile)
}

// hookStateSeenBlock fails open without a session ID to avoid false suppression.
func hookStateSeenBlock(agent, sessionID, block string) bool {
	return hookStateSeenBlockAt(dataDir, agent, sessionID, block)
}

func hookStateSeenBlockAt(storeDir, agent, sessionID, block string) bool {
	if sessionID == "" {
		return false
	}
	data, err := os.ReadFile(hookStatePath(storeDir))
	if err != nil {
		return false
	}
	var st map[string]string
	if err := json.Unmarshal(data, &st); err != nil {
		return false
	}
	_, hash, ok := hookStateEntry(st[hookStateKey(agent, sessionID)])
	return ok && hash == hookBlockHash(block)
}

// hookStateRecordBlock retains the latest block per identified session.
// Best-effort failures only cost a duplicate injection.
func hookStateRecordBlock(agent, sessionID, block string) {
	hookStateRecordBlockAt(dataDir, agent, sessionID, block)
}

func hookStateRecordBlockAt(storeDir, agent, sessionID, block string) {
	if sessionID == "" {
		return
	}
	path := hookStatePath(storeDir)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

	st := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st) // keep other agents' entries when readable
	}
	if st == nil {
		st = map[string]string{}
	}

	type sessionEntry struct {
		key      string
		sequence uint64
	}
	key := hookStateKey(agent, sessionID)
	entries := make([]sessionEntry, 0, len(st)+1)
	var maxSequence uint64
	for existingKey, value := range st {
		if strings.HasPrefix(existingKey, hookStateSessionPrefix) {
			sequence, _, ok := hookStateEntry(value)
			if !ok || existingKey == key {
				delete(st, existingKey)
				continue
			}
			entries = append(entries, sessionEntry{existingKey, sequence})
			if sequence > maxSequence {
				maxSequence = sequence
			}
		}
	}
	sequence := maxSequence + 1
	st[key] = fmt.Sprintf("%d:%s", sequence, hookBlockHash(block))
	entries = append(entries, sessionEntry{key, sequence})
	sort.Slice(entries, func(i, j int) bool { return entries[i].sequence > entries[j].sequence })
	if len(entries) > hookStateSessionLimit {
		for _, entry := range entries[hookStateSessionLimit:] {
			delete(st, entry.key)
		}
	}
	if out, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(path, out, 0o644)
	}
}

// The state-file path scopes the store; this key adds agent + opaque session.
func hookStateKey(agent, sessionID string) string {
	return hookStateSessionPrefix + agent + ":" + hookBlockHash(sessionID)
}

func hookStateEntry(value string) (uint64, string, bool) {
	sequenceText, hash, ok := strings.Cut(value, ":")
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	return sequence, hash, ok && err == nil && hash != ""
}

func hookBlockHash(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])
}

// hookLog appends one JSON line to <dataDir>/hooks.log. Best-effort by
// contract: a logging failure must not turn into a hook failure.
func hookLog(payload hookEventPayload, event string, elapsed time.Duration, outcome, detail string) {
	hookLogAt(runtimeContext{storeDir: dataDir, agentID: deriveAgentID(hookCWD(payload))}, payload, event, elapsed, outcome, detail)
}

func hookLogAt(route runtimeContext, payload hookEventPayload, event string, elapsed time.Duration, outcome, detail string) {
	path := filepath.Join(route.storeDir, "hooks.log")
	entry := map[string]any{
		"ts":      timeNow().UTC().Format(time.RFC3339),
		"event":   event,
		"outcome": outcome,
		"ms":      elapsed.Milliseconds(),
		"detail":  detail,
		"agent":   route.agentID,
		"session": payload.SessionID,
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
