package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// `graymatter hooks` wires GrayMatter into Claude Code's hook system so
// memory is injected and captured automatically, per-turn, with no reliance
// on the model remembering to call a tool.
//
// Two halves:
//
//   - install/uninstall/doctor: manage the `hooks` block in Claude Code's
//     settings.json (project: .claude/settings.json, global:
//     ~/.claude/settings.json), merging with whatever the user already has —
//     never overwriting foreign entries, keeping a .bak across every rewrite.
//   - run <event>: the binary Claude Code executes on each hook event. Every
//     failure path exits 0 with empty stdout and logs to <dataDir>/hooks.log —
//     a broken memory must degrade silently, never break the user's session.

const (
	// Markers distinguish current commands from legacy malformed entries.
	hooksCommandMarker    = "--graymatter-managed-hook"
	hooksRunCommandMarker = " hooks run "
	hooksNoCreateArg      = "--no-create"

	// hooksEventSessionStart is one of the four events this package manages.
	// Claude Code fires SessionStart with source startup|resume|clear|compact;
	// we attach to startup|resume|fork (fresh context) and compact (re-inject
	// after /compact — the "your memory survives /compact" path). The matcher
	// deliberately skips "clear": clearing the context is how a user asks for
	// a blank slate.
	hooksEventSessionStart = "SessionStart"
	hooksEventUserPrompt   = "UserPromptSubmit"
	hooksEventPreCompact   = "PreCompact"
	hooksEventSessionEnd   = "SessionEnd"

	// userPromptHookTimeout seconds. UserPromptSubmit's client default is far
	// longer; the runner answers in well under a second, and a hung memory
	// must never hold the user's prompt hostage.
	userPromptHookTimeout = 10

	// hooksBackupSuffix is appended to settings.json on every rewrite that
	// changes content (parity with context-sync's .bak discipline).
	hooksBackupSuffix = ".bak"
)

func hooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Manage automatic memory hooks for Claude Code",
		Long: `Install, remove, and verify GrayMatter's Claude Code hooks.

Once installed, every session injects the most relevant memories
automatically (SessionStart), every prompt gets a short recall
(UserPromptSubmit), checkpoints are taken before /compact (PreCompact),
and consolidation runs detached at session end (SessionEnd) — no tool
calls required from the model.

Project scope writes .claude/settings.json (committable, shared with the
team). Global scope writes ~/.claude/settings.json. Both merge with any
hooks you already have; GrayMatter's entries are upserted in place, and
every rewrite keeps the previous file as <file>.bak.`,
	}
	cmd.AddCommand(hooksInstallCmd(), hooksUninstallCmd(), hooksDoctorCmd(), hooksRunCmd())
	return cmd
}

func hooksInstallCmd() *cobra.Command {
	var (
		scope  string
		all    bool
		global bool
	)
	cmd := &cobra.Command{
		Use:   "install [--scope project|global] [--all]",
		Short: "Write GrayMatter's hooks into Claude Code settings.json",
		Long: `Upserts GrayMatter's hook groups using exec form (Claude Code 2.1.139 or later).

Merge, never overwrite: hooks that are not GrayMatter's (formatters,
guardrails, anything else) are preserved. If GrayMatter's own entries
were edited by hand, the rewrite reports the drift and proceeds, keeping
the previous file as .bak.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return fmt.Errorf("hooks install --all is not supported; install one scope explicitly to avoid duplicate hook execution")
			}
			scopes, err := parseHookScopes(scope, global, false)
			if err != nil {
				return err
			}
			exe, err := resolveOwnBinary()
			if err != nil {
				return err
			}

			for _, sc := range scopes {
				path, err := claudeSettingsPath(sc)
				if err != nil {
					return err
				}
				res, err := upsertHookSettings(path, exe, true, sc)
				if err != nil {
					return err
				}
				printHookResult(cmd, "install", res)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "project", "where to write: project (.claude/settings.json) or global (~/.claude/settings.json)")
	cmd.Flags().BoolVar(&all, "all", false, "unsupported: install one scope at a time to avoid duplicate hook execution")
	cmd.Flags().BoolVar(&global, "global", false, "alias of --scope global")
	_ = cmd.Flags().MarkHidden("global")
	return cmd
}

func hooksUninstallCmd() *cobra.Command {
	var (
		scope  string
		all    bool
		global bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall [--scope project|global] [--all]",
		Short: "Remove GrayMatter's hooks from Claude Code settings.json",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			scopes, err := parseHookScopes(scope, global, all)
			if err != nil {
				return err
			}
			exe, err := resolveOwnBinary()
			if err != nil {
				return err
			}
			for _, sc := range scopes {
				path, err := claudeSettingsPath(sc)
				if err != nil {
					return err
				}
				// exe is unused on uninstall (no canonical groups are written)
				// but is threaded through so the upsert signature stays single.
				res, err := upsertHookSettings(path, exe, false, sc)
				if err != nil {
					return err
				}
				printHookResult(cmd, "uninstall", res)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "project", "where to write: project (.claude/settings.json) or global (~/.claude/settings.json)")
	cmd.Flags().BoolVar(&all, "all", false, "uninstall from both project and global scope")
	cmd.Flags().BoolVar(&global, "global", false, "alias of --scope global")
	_ = cmd.Flags().MarkHidden("global")
	return cmd
}

// resolveOwnBinary returns this executable as an absolute native path — the
// exact value exec-form hooks record, so it resolves from any working directory.
func resolveOwnBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own binary: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("resolve own binary path: %w", err)
	}
	return exe, nil
}

// --- settings management ------------------------------------------------------

type hookScope string

const (
	scopeProject hookScope = "project"
	scopeGlobal  hookScope = "global"
)

func parseHookScopes(scope string, global, all bool) ([]hookScope, error) {
	if all {
		if scope != "" && scope != string(scopeProject) && scope != string(scopeGlobal) {
			return nil, fmt.Errorf("unknown scope %q (want project or global)", scope)
		}
		return []hookScope{scopeProject, scopeGlobal}, nil
	}
	if global {
		if scope != "" && scope != string(scopeProject) && scope != string(scopeGlobal) {
			return nil, fmt.Errorf("conflicting --scope %q and --global", scope)
		}
		return []hookScope{scopeGlobal}, nil
	}
	switch scope {
	case "", string(scopeProject):
		return []hookScope{scopeProject}, nil
	case string(scopeGlobal):
		return []hookScope{scopeGlobal}, nil
	default:
		return nil, fmt.Errorf("unknown scope %q (want project or global)", scope)
	}
}

// claudeSettingsPath resolves the settings.json for a scope. The project path
// is relative to the process working directory, matching how Claude Code
// resolves project settings.
func claudeSettingsPath(scope hookScope) (string, error) {
	if scope == scopeGlobal {
		home, err := resolveHome()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	}
	return filepath.Join(".claude", "settings.json"), nil
}

// hookSettingsResult is what a settings rewrite reports back.
type hookSettingsResult struct {
	Path     string `json:"path"`
	Changed  bool   `json:"changed"`
	BackedUp bool   `json:"backup_created"`
	// Drifted is true when GrayMatter's own entries existed but differed from
	// what this version writes (hand edit, older install, moved binary). The
	// rewrite proceeds — the previous file is in .bak — but the user is told.
	Drifted bool `json:"hand_edit_detected"`
	// Present reports whether GrayMatter's entries are installed after the
	// operation (always true for install; false after uninstall).
	Present bool   `json:"installed"`
	Warn    string `json:"warning,omitempty"`
}

// upsertHookSettings merges GrayMatter's hook groups into the settings file
// at path. With install=true the canonical groups for the given binary are
// written; with install=false every GrayMatter entry is removed. Foreign
// content is never touched: unknown keys, unknown hook events, and foreign
// matcher groups pass through unchanged.
//
// An unparseable settings file is never clobbered — the caller gets a warning
// and the file is left exactly as it was. A file whose "hooks" value is not
// an object is likewise left alone.
func upsertHookSettings(path, exe string, install bool, scopes ...hookScope) (hookSettingsResult, error) {
	scope := scopeProject
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	res := hookSettingsResult{Path: path}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", path, err)
	}

	var root map[string]any
	switch {
	case os.IsNotExist(err) || len(strings.TrimSpace(string(data))) == 0:
		if !install {
			return res, nil // nothing to uninstall
		}
		root = map[string]any{}
	default:
		if err := json.Unmarshal(data, &root); err != nil {
			res.Warn = fmt.Sprintf("%s exists but is not valid JSON; left untouched (add the hooks block manually)", path)
			return res, nil
		}
		if root == nil {
			root = map[string]any{}
		}
	}

	hooks, _ := root["hooks"].(map[string]any)
	if root["hooks"] != nil && hooks == nil {
		res.Warn = fmt.Sprintf("%s has a non-object \"hooks\" key; left untouched", path)
		return res, nil
	}
	if hooks == nil {
		if !install {
			return res, nil
		}
		hooks = map[string]any{}
	}

	changed := false
	drifted := false
	unmanaged := []string{}
	for _, event := range hookEventNames() {
		want := hookGroupsFor(exe, event, scope)
		if !install {
			want = nil
		}
		next, mutated, drift, managed := rewriteHookEvent(hooks[event], want, install, exe)
		if !managed {
			unmanaged = append(unmanaged, event)
			continue
		}
		hooks[event] = next
		changed = changed || mutated
		drifted = drifted || drift
		if install {
			res.Present = true
		}
	}
	if len(unmanaged) > 0 {
		res.Warn = fmt.Sprintf("%s: %s hooks not managed (not a hook list); left untouched", path, strings.Join(unmanaged, ", "))
	}

	if !install {
		// Drop event keys we emptied, and the hooks key itself when nothing
		// remains — uninstalling should not leave an empty husk behind.
		for _, event := range hookEventNames() {
			if arr, ok := hooks[event].([]any); ok && len(arr) == 0 {
				delete(hooks, event)
				changed = true
			}
		}
		if len(hooks) == 0 {
			delete(root, "hooks")
		}
	} else {
		root["hooks"] = hooks
	}
	res.Drifted = drifted

	if !changed {
		return res, nil // already exactly what we want; never churn formatting
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return res, fmt.Errorf("marshal %s: %w", path, err)
	}
	out = append(out, '\n')

	if len(data) > 0 {
		if err := os.WriteFile(path+hooksBackupSuffix, data, 0o644); err != nil {
			return res, fmt.Errorf("write backup %s%s: %w", path, hooksBackupSuffix, err)
		}
		res.BackedUp = true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}
	res.Changed = true
	return res, nil
}

// rewriteHookEvent applies our upsert to one event's array. Ours are entries
// whose matcher group carries a GrayMatter-owned command; they are
// removed and, on install, replaced by the canonical groups appended after
// the foreign ones. Foreign groups keep their original order and content.
//
// Returns the new value (existing untouched when the value is not a hook list
// we can parse), whether anything actually changed, whether the removed
// entries had drifted from what we would write, and whether the existing
// value was manageable at all.
func rewriteHookEvent(existing any, want []any, install bool, expectedExe ...string) (next any, mutated, drift, managed bool) {
	if existing == nil {
		return want, len(want) > 0, false, true
	}
	arr, ok := existing.([]any)
	if !ok {
		return existing, false, false, false // not ours to manage
	}

	oursBefore := make([]any, 0, len(arr))
	foreign := make([]any, 0, len(arr))
	for _, group := range arr {
		if hookGroupIsOurs(group, expectedExe...) {
			oursBefore = append(oursBefore, group)
			continue
		}
		foreign = append(foreign, group)
	}

	if install && len(want) > 0 {
		nextArr := make([]any, 0, len(foreign)+len(want))
		nextArr = append(nextArr, foreign...)
		nextArr = append(nextArr, want...)
		next = nextArr
		if len(oursBefore) > 0 {
			drift = !hookGroupsEquivalent(oursBefore, want)
		}
		mutated = !hookArraysEqual(oursBefore, want)
		if !mutated {
			// Same length and not drifted: could still differ in content order
			// relative to foreign groups; a deep compare settles it.
			mutated = !hookArraysEqual(arr, nextArr)
		}
		return next, mutated, drift, true
	}

	// Uninstall: drop our entries, keep foreign.
	if len(oursBefore) == 0 {
		return existing, false, false, true
	}
	nextArr := make([]any, len(foreign))
	copy(nextArr, foreign)
	return nextArr, true, false, true
}

// hookArraysEqual compares two hook event arrays by canonical JSON.
func hookArraysEqual(a, b []any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// hookGroupsEquivalent treats the two previous shell-command formats as the
// canonical exec form for drift purposes. Reinstall still rewrites them, but a
// format migration alone is not a hand edit. Every other field is compared.
func hookGroupsEquivalent(existing, want []any) bool {
	data, err := json.Marshal(existing)
	if err != nil {
		return false
	}
	var normalized []any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return false
	}
	for i := range normalized {
		if i >= len(want) {
			break
		}
		normalizeLegacyHookGroup(normalized[i], want[i])
	}
	return hookArraysEqual(normalized, want)
}

func normalizeLegacyHookGroup(existing, want any) {
	eg, ok := existing.(map[string]any)
	if !ok {
		return
	}
	wg, ok := want.(map[string]any)
	if !ok {
		return
	}
	existingHooks, _ := eg["hooks"].([]any)
	wantedHooks, _ := wg["hooks"].([]any)
	for i := range existingHooks {
		if i >= len(wantedHooks) {
			break
		}
		oldHook, ok := existingHooks[i].(map[string]any)
		if !ok {
			continue
		}
		newHook, ok := wantedHooks[i].(map[string]any)
		if ok && legacyHookMatches(oldHook, newHook) {
			oldHook["command"] = newHook["command"]
			oldHook["args"] = newHook["args"]
		}
	}
}

// hookGroupIsOurs reports whether a matcher group belongs to GrayMatter.
func hookGroupIsOurs(group any, expectedExe ...string) bool {
	ours, _ := hookGroupGuardStatus(group, expectedExe...)
	return ours
}

func hookGroupGuardStatus(group any, expectedExe ...string) (ours, guarded bool) {
	guarded = true
	g, ok := group.(map[string]any)
	if !ok {
		return false, true
	}
	list, _ := g["hooks"].([]any)
	for _, h := range list {
		hook, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if hookEntryIsOurs(hook, expectedExe...) {
			ours = true
			guarded = guarded && hookEntryHasArg(hook, hooksNoCreateArg)
		}
	}
	return ours, guarded
}

// hookEventNames returns the events GrayMatter manages, in canonical order.
func hookEventNames() []string {
	return []string{hooksEventSessionStart, hooksEventUserPrompt, hooksEventPreCompact, hooksEventSessionEnd}
}

// hookGroupsFor builds the canonical matcher groups for one event and the
// given executable. The args carry the CLI event name (the snake_case value
// `hooks run` takes), not the settings event key.
func hookGroupsFor(exe, event string, scopes ...hookScope) []any {
	scope := scopeProject
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	group := func(matcher string, timeout int) map[string]any {
		args := []string{"hooks", "run", hookRunArg(event), hooksCommandMarker}
		if scope == scopeGlobal {
			args = append(args, hooksNoCreateArg)
		}
		hook := map[string]any{"type": "command", "command": exe, "args": args}
		if timeout > 0 {
			hook["timeout"] = timeout
		}
		g := map[string]any{"hooks": []any{hook}}
		if matcher != "" {
			g["matcher"] = matcher
		}
		return g
	}
	switch event {
	case hooksEventSessionStart:
		// Two groups: fresh sessions (startup, resume, fork) and post-compact
		// re-injection.
		return []any{
			group("startup|resume|fork", 0),
			group("compact", 0),
		}
	case hooksEventUserPrompt:
		return []any{group("", userPromptHookTimeout)}
	case hooksEventPreCompact:
		return []any{group("", 0)}
	case hooksEventSessionEnd:
		return []any{group("", 0)}
	}
	return nil
}

// hookRunArg maps a settings event key to the `hooks run` argument.
func hookRunArg(event string) string {
	switch event {
	case hooksEventSessionStart:
		return "session-start"
	case hooksEventUserPrompt:
		return "user-prompt"
	case hooksEventPreCompact:
		return "pre-compact"
	case hooksEventSessionEnd:
		return "session-end"
	}
	return event
}

// hookCommand renders the previous shell form. It remains solely so legacy
// installations can be exercised against the compatibility parser.
func hookCommand(exe, event string, scopes ...hookScope) string {
	scope := scopeProject
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	exe = filepath.ToSlash(exe)
	trailer := hooksRunCommandMarker + event + " " + hooksCommandMarker
	if scope == scopeGlobal {
		trailer += " " + hooksNoCreateArg
	}
	if strings.ContainsAny(exe, " '\"") {
		return strconv.Quote(exe) + trailer
	}
	return exe + trailer
}

// --- doctor -------------------------------------------------------------------

func hooksDoctorCmd() *cobra.Command {
	var (
		scope  string
		all    bool
		global bool
	)
	cmd := &cobra.Command{
		Use:   "doctor [--scope project|global] [--all]",
		Short: "Verify hooks are registered, the binary resolves, and latency is sane",
		Long: `Checks, for each requested scope, that:

  1. settings.json exists and carries GrayMatter's hook entries,
  2. the binary those entries point at exists (and matches this binary),
  3. the store answers within budget (a recall round-trip, timed).

Exits non-zero when any check fails, so scripts and CI can gate on it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			scopes, err := parseHookScopes(scope, global, all)
			if err != nil {
				return err
			}
			exeAbs, err := resolveOwnBinary()
			if err != nil {
				return err
			}

			fails := 0
			for _, sc := range scopes {
				path, err := claudeSettingsPath(sc)
				if err != nil {
					return err
				}
				for _, c := range runHooksDoctorChecks(path, exeAbs, sc) {
					if jsonOut {
						data, _ := json.Marshal(c)
						fmt.Fprintln(cmd.OutOrStdout(), string(data))
					} else {
						glyph := map[string]string{"ok": "✓", "info": "·", "warn": "!", "fail": "✗"}[c.Status]
						fmt.Fprintf(cmd.OutOrStdout(), "  %s %-10s %s\n", glyph, c.Name, c.Detail)
						if c.Hint != "" {
							fmt.Fprintf(cmd.OutOrStdout(), "    → %s\n", c.Hint)
						}
					}
					if c.Status == "fail" {
						fails++
					}
				}
			}
			if fails > 0 {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "project", "which settings to inspect: project or global")
	cmd.Flags().BoolVar(&all, "all", false, "check both project and global scope")
	cmd.Flags().BoolVar(&global, "global", false, "alias of --scope global")
	_ = cmd.Flags().MarkHidden("global")
	return cmd
}

type hookCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// runHooksDoctorChecks performs the settings, binary, and store checks for one
// settings file.
func runHooksDoctorChecks(path, exeAbs string, scope hookScope) []hookCheck {
	checks := []hookCheck{}

	data, err := os.ReadFile(path)
	if err != nil {
		checks = append(checks, hookCheck{
			Name: "settings", Status: "fail", Detail: fmt.Sprintf("%s does not exist", path),
			Hint: "run `graymatter hooks install --scope " + string(scope) + "` (or `graymatter init --hooks`)",
		})
		return append(checks, hooksStoreCheck())
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		checks = append(checks, hookCheck{
			Name: "settings", Status: "fail", Detail: fmt.Sprintf("%s is not valid JSON", path),
			Hint: "fix or remove the file, then run `graymatter hooks install`",
		})
		return append(checks, hooksStoreCheck())
	}

	hooks, _ := root["hooks"].(map[string]any)
	missing := []string{}
	exePath := ""
	for _, event := range hookEventNames() {
		arr, _ := hooks[event].([]any)
		found := false
		for _, group := range arr {
			g, ok := group.(map[string]any)
			if !ok {
				continue
			}
			list, _ := g["hooks"].([]any)
			for _, h := range list {
				hook, ok := h.(map[string]any)
				if !ok {
					continue
				}
				if hookEntryIsOurs(hook, exeAbs) {
					found = true
					if exePath == "" {
						exePath = hookEntryBinaryPath(hook)
					}
				}
			}
		}
		if !found {
			missing = append(missing, event)
		}
	}

	switch {
	case len(missing) == len(hookEventNames()):
		checks = append(checks, hookCheck{
			Name: "settings", Status: "fail", Detail: fmt.Sprintf("%s has no GrayMatter hooks", path),
			Hint: "run `graymatter hooks install --scope " + string(scope) + "`",
		})
	case len(missing) > 0:
		checks = append(checks, hookCheck{
			Name: "settings", Status: "fail",
			Detail: fmt.Sprintf("%s is missing hooks for %s", path, strings.Join(missing, ", ")),
			Hint:   "re-run `graymatter hooks install --scope " + string(scope) + "`",
		})
	default:
		detail := fmt.Sprintf("all 4 events registered in %s", path)
		if drift, _ := hookSettingsDrift(path, exeAbs, scope); drift {
			detail += " (entries differ from what this version writes)"
		}
		checks = append(checks, hookCheck{Name: "settings", Status: "ok", Detail: detail})
	}
	if installed, guarded := globalHookGuardStatus(root); scope == scopeGlobal && installed && !guarded {
		checks = append(checks, hookCheck{
			Name: "scope guard", Status: "warn",
			Detail: "global hooks predate the no-create guard and can initialize stores in unrelated directories",
			Hint:   "re-run `graymatter hooks install --scope global`",
		})
	}
	// Binary check: does the recorded command point at something that exists,
	// and is it this binary?
	binCheck := hookCheck{Name: "binary", Status: "ok", Detail: exeAbs}
	if exePath == "" {
		binCheck.Status, binCheck.Detail = "warn", "no hook command found to resolve a binary from"
	} else if _, statErr := os.Stat(exePath); statErr != nil {
		binCheck.Status, binCheck.Detail = "fail", fmt.Sprintf("recorded binary %s does not exist", exePath)
		binCheck.Hint = "the binary moved or was uninstalled; re-run `graymatter hooks install`"
	} else if !sameBinary(exePath, exeAbs) {
		binCheck.Status, binCheck.Detail = "warn", fmt.Sprintf("settings point at %s, but this is %s", exePath, exeAbs)
		binCheck.Hint = "usually an upgrade; re-run `graymatter hooks install` to refresh the recorded path"
	}
	checks = append(checks, binCheck)

	return append(checks, hooksStoreCheck())
}

// hooksStoreCheck times one store round-trip, the same path the hooks
// themselves take. The latency budget mirrors the user-prompt hook: if the
// store cannot answer within it, per-turn injection is the wrong shape and
// the doctor says so.
func hooksStoreCheck() hookCheck {
	start := timeNow()
	store, err := openStore()
	if err != nil {
		return hookCheck{
			Name: "store", Status: "fail", Detail: "store unreachable: " + err.Error(),
			Hint: "hooks degrade silently when the store is down; run `graymatter doctor` for the full picture",
		}
	}
	defer func() { _ = store.Close() }()

	agent := deriveAgentID(mustWorkdir())
	if _, err := store.Recall(context.Background(), agent, "", 3); err != nil {
		return hookCheck{
			Name: "store", Status: "fail", Detail: "recall failed: " + err.Error(),
			Hint: "hooks will inject nothing while recall errors; run `graymatter doctor`",
		}
	}
	elapsed := timeNow().Sub(start)
	c := hookCheck{
		Name:   "store",
		Status: "ok",
		Detail: fmt.Sprintf("recall round-trip %dms (agent %q)", elapsed.Milliseconds(), agent),
	}
	if elapsed > hookLatencyBudget {
		c.Status = "warn"
		c.Detail = fmt.Sprintf("recall round-trip %dms exceeds the %v budget", elapsed.Milliseconds(), hookLatencyBudget)
		c.Hint = "per-turn injection only works while it is fast; check disk health and daemon state"
	}
	return c
}

// hookSettingsDrift reports whether the installed entries differ from what
// this binary would write. Best-effort: any parse problem means "no drift".
func hookSettingsDrift(path, exeAbs string, scopes ...hookScope) (bool, error) {
	scope := scopeProject
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return false, err
	}
	hooks, _ := root["hooks"].(map[string]any)
	drift := false
	for _, event := range hookEventNames() {
		_, _, d, managed := rewriteHookEvent(hooks[event], hookGroupsFor(exeAbs, event, scope), true, exeAbs)
		drift = drift || (managed && d)
	}
	return drift, nil
}

func globalHookGuardStatus(root map[string]any) (installed, guarded bool) {
	guarded = true
	hooks, _ := root["hooks"].(map[string]any)
	for _, event := range hookEventNames() {
		arr, _ := hooks[event].([]any)
		for _, group := range arr {
			ours, safe := hookGroupGuardStatus(group)
			if ours {
				installed, guarded = true, guarded && safe
			}
		}
	}
	return installed, guarded
}

func hookEntryArgs(hook map[string]any) ([]string, bool) {
	raw, present := hook["args"]
	if !present {
		return nil, false
	}
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		args := make([]string, len(values))
		for i, value := range values {
			arg, ok := value.(string)
			if !ok {
				return nil, false
			}
			args[i] = arg
		}
		return args, true
	default:
		return nil, false
	}
}

func hookEntryHasArg(hook map[string]any, want string) bool {
	if _, structured := hook["args"]; structured {
		args, ok := hookEntryArgs(hook)
		if !ok {
			return false
		}
		for _, arg := range args {
			if arg == want {
				return true
			}
		}
		return false
	}
	command, _ := hook["command"].(string)
	return hookCommandHasArg(command, want)
}

func hookEntryIsOurs(hook map[string]any, expectedExe ...string) bool {
	command, ok := hook["command"].(string)
	if !ok {
		return false
	}
	if _, structured := hook["args"]; !structured {
		return hookCommandIsOurs(command, expectedExe...)
	}
	args, ok := hookEntryArgs(hook)
	if !ok {
		return false
	}
	if len(args) < 3 || args[0] != "hooks" || args[1] != "run" {
		return false
	}
	if stringSliceContains(args, hooksCommandMarker) {
		return true
	}
	if len(expectedExe) > 0 && filepath.Clean(command) == filepath.Clean(expectedExe[0]) {
		return true
	}
	return strings.TrimSuffix(strings.ToLower(filepath.Base(command)), ".exe") == "graymatter"
}

func hookEntryBinaryPath(hook map[string]any) string {
	command, _ := hook["command"].(string)
	if _, structured := hook["args"]; structured {
		return command
	}
	return hookBinaryPath(command)
}

func legacyHookMatches(existing, want map[string]any) bool {
	if _, structured := existing["args"]; structured {
		return false
	}
	command, ok := existing["command"].(string)
	if !ok {
		return false
	}
	wantCommand, ok := want["command"].(string)
	if !ok || filepath.Clean(hookBinaryPath(command)) != filepath.Clean(wantCommand) {
		return false
	}
	wantArgs, ok := hookEntryArgs(want)
	if !ok || len(wantArgs) < 4 {
		return false
	}
	idx := strings.LastIndex(command, hooksRunCommandMarker)
	if idx < 0 {
		return false
	}
	tail := strings.Fields(command[idx+len(hooksRunCommandMarker):])
	prefix := strings.TrimSpace(command[:idx])
	if strings.HasSuffix(prefix, " graymatter") {
		return len(tail) == 1 && tail[0] == wantArgs[2]
	}
	return stringSlicesEqual(tail, wantArgs[2:])
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hookCommandHasArg(command, want string) bool {
	idx := strings.LastIndex(command, hooksRunCommandMarker)
	if idx < 0 {
		return false
	}
	for _, arg := range strings.Fields(command[idx+len(hooksRunCommandMarker):]) {
		if arg == want {
			return true
		}
	}
	return false
}

func hookCommandIsOurs(command string, expectedExe ...string) bool {
	if hookCommandHasArg(command, hooksCommandMarker) {
		return true
	}
	got := hookBinaryPath(command)
	if len(expectedExe) > 0 && filepath.Clean(got) == filepath.Clean(filepath.FromSlash(expectedExe[0])) {
		return true
	}
	return strings.TrimSuffix(strings.ToLower(filepath.Base(got)), ".exe") == "graymatter"
}

func hookBinaryPath(command string) string {
	idx := strings.LastIndex(command, hooksRunCommandMarker)
	if idx < 0 {
		return ""
	}
	p := strings.TrimSpace(command[:idx])
	p = strings.TrimSpace(strings.TrimSuffix(p, " graymatter"))
	if unquoted, err := strconv.Unquote(p); err == nil {
		p = unquoted
	} else {
		p = strings.Trim(p, `"'`)
	}
	return filepath.FromSlash(p)
}

// printHookResult renders one settings rewrite for humans, or as a JSON line
// under --json. verb is "install" or "uninstall"; it only shapes the prose.
func printHookResult(cmd *cobra.Command, verb string, res hookSettingsResult) {
	if jsonOut {
		data, _ := json.Marshal(res)
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return
	}
	if res.Warn != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "! %s\n", res.Warn)
	}
	if quiet {
		return
	}
	switch {
	case res.Changed:
		note := ""
		if res.BackedUp {
			note = fmt.Sprintf(", previous file kept at %s%s", res.Path, hooksBackupSuffix)
		}
		what := "hooks installed"
		if verb == "uninstall" {
			what = "hooks removed"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✓ %s: %s%s\n", res.Path, what, note)
	case verb == "install":
		fmt.Fprintf(cmd.OutOrStdout(), "· %s: already installed\n", res.Path)
	default:
		fmt.Fprintf(cmd.OutOrStdout(), "· %s: nothing to remove\n", res.Path)
	}
	if res.Drifted {
		note := ""
		if res.BackedUp {
			note = fmt.Sprintf(" (previous file kept at %s%s)", res.Path, hooksBackupSuffix)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: GrayMatter's entries in %s had been modified by hand; they were rewritten%s\n", res.Path, note)
	}
}
