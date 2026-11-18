package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

// The default doctor command observes setup artifacts without opening a
// writable runtime. It cannot determine a client's effective configuration.

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | info | warn | fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type doctorSetupReport struct {
	DataDir            string        `json:"data_dir"`
	OK                 bool          `json:"ok"`
	Status             string        `json:"status"`
	DiagnosticMode     string        `json:"diagnostic_mode"`
	Readiness          string        `json:"readiness"`
	DataDirWritability string        `json:"data_dir_writability"`
	Checks             []checkResult `json:"checks"`
}

var errDoctorChecksFailed = errors.New("doctor: diagnostic checks failed")

func newDoctorSetupReport(dir string, checks []checkResult) doctorSetupReport {
	status := "ok"
	for _, c := range checks {
		if c.Status == "fail" {
			status = "fail"
			break
		}
		if c.Status == "warn" {
			status = "warn"
		}
	}
	return doctorSetupReport{
		DataDir: dir, OK: status != "fail", Status: status,
		DiagnosticMode: "read_only", Readiness: "not_evaluated",
		DataDirWritability: "not_tested", Checks: append([]checkResult{}, checks...),
	}
}

var doctorEncodeSetupJSON = func(w io.Writer, report doctorSetupReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func writeDoctorSetupReport(w io.Writer, report doctorSetupReport, jsonOutput bool) error {
	var out bytes.Buffer
	if jsonOutput {
		if err := doctorEncodeSetupJSON(&out, report); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(&out, "GrayMatter doctor — data dir %q (read-only setup observations)\n\n", report.DataDir)
		for _, c := range report.Checks {
			glyph := map[string]string{"ok": "✓", "info": "·", "warn": "!", "fail": "✗"}[c.Status]
			fmt.Fprintf(&out, "  %s %-14s %s\n", glyph, c.Name, c.Detail)
			if c.Hint != "" {
				fmt.Fprintf(&out, "    → %s\n", c.Hint)
			}
		}
		fmt.Fprintf(&out, "\n  status: %s · readiness: not evaluated · data directory writability: not tested\n", report.Status)
		fmt.Fprintln(&out, "  These observations do not verify a client's effective MCP configuration or that memory is ready for use.")
	}
	n, err := w.Write(out.Bytes())
	if err != nil {
		return err
	}
	if n != out.Len() {
		return io.ErrShortWrite
	}
	return nil
}

func doctorCmd() *cobra.Command {
	var (
		audit      bool
		graphMode  bool
		health     bool
		embeddings bool
	)
	cmd := &cobra.Command{
		Use:   "doctor [path]",
		Short: "Diagnose the GrayMatter setup in this directory",
		Long: `Observes local setup artifacts without changing persistent state:

  1. this binary and the graymatter executable found on PATH
  2. data directory exists (writability is not tested)
  3. read-only store observations, if available
  4. references to graymatter in local client config files
  5. memory instructions observed in CLAUDE.md / AGENTS.md
  6. prompt-packet policy entries in project and global settings.json

This mode does not verify client trust, effective settings precedence, argv/env,
or readiness. Config references are substring observations, not proof that a
client loads them. It does not test data directory writability. With an already
running daemon it makes read-only RPCs; it never starts one.

With --audit, skips the setup checks and instead audits the instruction
documents themselves: approx token cost per prompt (tokenizer declared in
the output), near-duplicate paragraphs, staleness by git blame, size
alerts at declared thresholds, and structural conflicts in managed
blocks. Works on any project — no .graymatter directory required.

Other modes have separate contracts. With --health, audits the store itself
instead of the setup: supersede loops, dumping bursts, critical facts near
prune (pin suggestions), and
duplicate density. Deterministic — the same store always produces the same
report, because rules read only store contents and never the wall clock.
Exit code is 1 only when a finding is a failure; warnings exit 0.

With --embeddings, audits the vector channel as the store observed it:
how many eligible live facts carry a vector, how many writes degraded to
keyword-only because the embedder failed, the last failure's message,
and the vector retry backlog. Deterministic like --health, and it works
even when the daemon is down by opening the store directly.

With --graph, reads graph analytics and provenance without starting a daemon.
Add --html with an output path to write a separate graph render. --health,
--audit, --embeddings and hooks doctor have separate contracts.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if graphMode {
				return runDoctorGraph(cmd)
			}
			if health {
				return runDoctorHealth(cmd)
			}
			if embeddings {
				return runDoctorEmbeddings(cmd)
			}
			if audit {
				root := "."
				if len(args) > 0 {
					root = args[0]
				}
				return runDoctorAudit(cmd, root)
			}

			// A positional path only means something under --audit. Taking it
			// silently and then auditing the working directory anyway is the
			// exact failure mode this project fixes elsewhere: input that
			// changes nothing and says so to nobody.
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument %q: a path requires --audit", args[0])
			}
			cmd.SilenceErrors, cmd.SilenceUsage = true, true

			checks := []checkResult{
				checkVersion(),
				checkBinaryOnPath(),
				checkDataDir(dataDir),
				checkStore(dataDir),
				checkMCPWiring("."),
				checkInstructions("."),
				checkGlobalHooks(),
				checkKG(dataDir),
				checkContextSync("."),
			}
			checks = append(checks, checkHookPacketPolicies()...)

			report := newDoctorSetupReport(dataDir, checks)
			if err := writeDoctorSetupReport(cmd.OutOrStdout(), report, jsonOut); err != nil {
				return fmt.Errorf("write doctor report: %w", err)
			}
			if !report.OK {
				return errDoctorChecksFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&audit, "audit", false, "audit instruction documents (tokens, duplicates, staleness, markers) instead of setup checks")
	cmd.Flags().BoolVar(&graphMode, "graph", false, "read graph analytics and provenance")
	cmd.Flags().String("html", "", "with --graph: also write the self-contained HTML graph render to this file")
	cmd.Flags().BoolVar(&health, "health", false, "separate runtime store audit: supersede loops, dumping bursts, near-prune criticals, duplicates")
	cmd.Flags().BoolVar(&embeddings, "embeddings", false, "separate vector-channel audit: coverage, degraded writes, retry backlog")
	return cmd
}

func checkHookPacketPolicies() []checkResult {
	exe, err := resolveOwnBinary()
	if err != nil {
		return []checkResult{{Name: "packet policy", Status: "fail", Detail: "cannot resolve this binary: " + err.Error()}}
	}
	checks := make([]checkResult, 0, 2)
	for _, scope := range []hookScope{scopeProject, scopeGlobal} {
		path, err := claudeSettingsPath(scope)
		if err != nil {
			checks = append(checks, checkResult{Name: "packet policy", Status: "fail", Detail: string(scope) + " settings: " + err.Error()})
			continue
		}
		check := readHookPacketConfigurationCheck(path, exe, scope)
		checks = append(checks, checkResult(check))
	}
	return checks
}

func checkGlobalHooks() checkResult {
	c := checkResult{Name: "global hooks", Status: "info", Detail: "not installed"}
	path, err := claudeSettingsPath(scopeGlobal)
	if err != nil {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return c
	}
	installed, guarded := globalHookGuardStatus(root)
	if installed && !guarded {
		c.Status = "warn"
		c.Detail = "installed without the no-create guard; hooks can initialize stores in unrelated directories"
		c.Hint = "re-run `graymatter hooks install --scope global`"
	} else if installed {
		c.Status, c.Detail = "ok", "installed with the no-create guard"
	}
	return c
}

// checkVersion compares file identity without executing the PATH candidate.
// A distinct candidate may have any version; this check cannot inspect it.
var doctorLookPath = exec.LookPath

func checkVersion() checkResult {
	c := checkResult{Name: "version", Status: "ok", Detail: version + " (this binary)"}

	path, err := doctorLookPath("graymatter")
	if err != nil {
		return c
	}
	self, err := os.Executable()
	if err != nil {
		c.Status, c.Detail = "warn", "could not identify this executable: "+err.Error()
		return c
	}
	if sameBinary(self, path) {
		return c
	}
	c.Status = "warn"
	c.Detail = fmt.Sprintf("%s (this binary); %s is a different executable (version not checked)", version, path)
	c.Hint = "review which executable your MCP client references; doctor does not run the PATH candidate or verify client argv/env"
	return c
}

// sameBinary reports whether two paths name the same file on disk.
func sameBinary(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func checkBinaryOnPath() checkResult {
	c := checkResult{Name: "binary"}
	path, err := doctorLookPath("graymatter")
	switch {
	case err == nil:
		c.Status, c.Detail = "ok", "graymatter on PATH ("+path+")"
	case errors.Is(err, exec.ErrDot):
		c.Status = "warn"
		c.Detail = "graymatter found only in the current directory, not on PATH"
		c.Hint = "MCP configurations that launch `graymatter` by name need it on PATH; move the binary there or inspect the client's configured command"
	default:
		c.Status = "warn"
		c.Detail = "graymatter is not on PATH"
		c.Hint = "MCP configurations that launch `graymatter` by name need it on PATH; install with `go install github.com/angelnicolasc/graymatter/cmd/graymatter@latest` or use an absolute command path"
	}
	return c
}

var doctorDataDirStat = os.Stat
var doctorKGMarkerStat = os.Stat

func checkDataDir(dir string) checkResult {
	c := checkResult{Name: "data dir"}
	info, err := doctorDataDirStat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.Status = "warn"
			c.Detail = fmt.Sprintf("%s does not exist", dir)
			c.Hint = "run `graymatter init --store-only` to prepare this project's store directory"
		} else {
			c.Status = "fail"
			c.Detail = fmt.Sprintf("cannot inspect %s: %v", dir, err)
		}
		return c
	}
	if !info.IsDir() {
		c.Status, c.Detail = "fail", dir+" exists but is not a directory"
		return c
	}
	c.Status, c.Detail = "ok", dir+" exists (writability not tested)"
	return c
}

// staleAfter is how long a project may sit initialised with nothing stored
// before doctor stops calling that normal. Long enough that a genuinely new
// project stays quiet, short enough that a broken setup surfaces inside a
// working day.
const staleAfter = 24 * time.Hour

// projectAge reports how long ago the project was initialised, using MEMORY.md
// as the anchor: init writes it once and nothing else touches it, unlike the
// data directory whose mtime moves every time the daemon starts.
func projectAge(dir string) (time.Duration, bool) {
	info, err := os.Stat(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// flagIfUnused shapes the store check for a project that holds nothing.
//
// This is the gap issue #14 fell through: wiring, instructions and store can
// all be green while the agent never calls a single tool, and the old summary
// still read "Everything looks good". A user had no way to tell a fresh install
// apart from a week of silence, so the failure produced no bug reports, just
// people quietly giving up.
//
// Two regimes, both anchored on facts == 0 regardless of whether gray.db
// exists yet (a CLI `remember` during setup used to silence the hint while
// the MCP session still had no tools):
//
//   - young projects get the restart hint at ok severity — the single most
//     common false alarm is a client that has not been restarted since init;
//   - projects idle past staleAfter degrade to warn and stack the instruction
//     guidance on top, because at that point restart alone did not help.
const zeroFactsRestartHint = "if your agent cannot see the memory tools, restart your MCP client; clients launch their servers at startup, so a session that predates `graymatter init` never picks them up"

func flagIfUnused(c checkResult, dir string, facts int) checkResult {
	if facts > 0 || (c.Status != "ok" && c.Status != "info") {
		return c
	}
	if c.Hint == "" {
		c.Hint = zeroFactsRestartHint
	}
	age, ok := projectAge(dir)
	if !ok || age < staleAfter {
		return c
	}
	c.Status = "warn"
	c.Detail = fmt.Sprintf("initialised %d day(s) ago and still holds no facts", int(age.Hours()/24))
	c.Hint = zeroFactsRestartHint + "; if a restart did happen, confirm CLAUDE.md / AGENTS.md carry the memory block (re-run `graymatter init` to refresh it), that your agent loads that file, or use `graymatter init --global` to install the instruction block home-wide (MCP wiring remains per project)"
	return c
}

func checkStore(dir string) checkResult {
	return checkStoreReadOnly(dir)
}

// checkKG reports knowledge-graph auto-population state and graph size.
//
// Deliberately benign when off: the graph is optional, and a warning for
// something the user never asked for is noise that trains people to ignore
// doctor output. The goal here is discoverability — the one line that tells
// a non-README-reading user the feature exists and how to turn it on — plus
// confirmation (with counts) for those who did opt in. The daemon is not
// required: the sentinel/env decision and a read-only graph open answer
// everything offline, so this check works before any client has started.
func checkKG(dir string) checkResult {
	c := checkResult{Name: "knowledge graph"}
	auto := os.Getenv("GRAYMATTER_KG") == "1"
	if _, err := doctorKGMarkerStat(daemon.KGSentinelPath(dir)); err == nil {
		auto = true
	} else if !errors.Is(err, os.ErrNotExist) {
		c.Status, c.Detail = "fail", fmt.Sprintf("cannot inspect graph activation marker: %v", err)
		return c
	}

	graph, countErr := doctorGraphState(dir)
	nodes, edges := graph.Nodes, graph.Edges
	switch {
	case errors.Is(countErr, errDoctorNoDatabase):
		c.Status, c.Detail = "info", "no database yet; the graph starts empty"
		if auto {
			c.Detail = "auto-population on; the graph starts empty"
			c.Hint = "first entities appear after ~20 facts, when consolidation first runs"
		} else {
			c.Hint = "optional: graymatter init --kg extracts entities and co-mention edges during consolidation"
		}
	case errors.Is(countErr, errDoctorUninspected):
		c.Status, c.Detail = "warn", "gray.db is a symbolic link; graph not inspected"
	case errors.Is(countErr, errDoctorIncomplete), errors.Is(countErr, bolt.ErrTimeout):
		c.Status, c.Detail = "warn", fmt.Sprintf("graph diagnostics incomplete: %v", countErr)
	case countErr != nil:
		c.Status, c.Detail = "fail", fmt.Sprintf("graph inspection failed: %v", countErr)
	case !auto && nodes == 0 && edges == 0:
		c.Status, c.Detail = "info", fmt.Sprintf("off — observed %d nodes / %d edges", nodes, edges)
		c.Hint = "optional: graymatter init --kg extracts entities and co-mention edges during consolidation; explicit links via memory_reflect action=link work regardless"
	case auto && nodes == 0:
		c.Status, c.Detail = "info", fmt.Sprintf("auto-population on — graph observed empty until consolidation (%d nodes / %d edges)", nodes, edges)
		c.Hint = "first entities appear after ~20 facts, when consolidation first runs"
	default:
		c.Status = "ok"
		detail := fmt.Sprintf("observed %d nodes / %d edges", nodes, edges)
		if auto {
			detail = "auto-population active — " + detail
		} else {
			detail = "explicitly linked — " + detail
		}
		c.Detail = detail
	}
	if countErr == nil && graph.Provenance != nil {
		c.Detail += " · " + graphProvenanceSummary(*graph.Provenance)
	} else if countErr == nil && edges != 0 {
		c.Detail += " · provenance unavailable"
	}
	return c
}

// wiredAgents returns the known agents whose MCP config in this project
// references graymatter. Paths come from knownAgents, the same table the init
// writers use, so doctor cannot look somewhere the writer never wrote.
func wiredAgents(projectDir string) []agentDef {
	var out []agentDef
	for _, a := range knownAgents(projectDir) {
		if a.configPath == nil {
			continue
		}
		p, err := a.configPath(projectDir)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// String containment is deliberately tolerant: it covers JSON, JSONC
		// (comments) and TOML without needing three parsers here.
		if strings.Contains(string(data), "graymatter") {
			out = append(out, a)
		}
	}
	return out
}

func checkMCPWiring(projectDir string) checkResult {
	c := checkResult{Name: "mcp wiring"}
	var wired []string
	for _, a := range wiredAgents(projectDir) {
		p, _ := a.configPath(projectDir)
		wired = append(wired, fmt.Sprintf("%s (%s)", a.name, p))
	}
	if len(wired) == 0 {
		c.Status = "warn"
		c.Detail = "no MCP client config references graymatter"
		c.Hint = "run `graymatter init` to wire Claude Code, Cursor, Codex, and OpenCode automatically"
		return c
	}
	c.Status, c.Detail = "ok", "config references observed: "+strings.Join(wired, ", ")+" (substring check; effective client settings not verified)"
	return c
}

// checkInstructions asks the question that matters: for each client actually
// wired here, does a briefing reach it?
//
// It used to look only at the project's CLAUDE.md and AGENTS.md, which made it
// contradict `graymatter init --global` — the very command its own hint
// recommends. A global install writes the block where Claude Code and OpenCode
// read it in every project, and doctor reported that as missing (issue #17).
// The inverse error is just as bad: crediting a global block to Cursor, which
// has no global file graymatter writes, would hide a real gap. So coverage is
// resolved per agent, from the same table that decides where init writes.
func checkInstructions(projectDir string) checkResult {
	c := checkResult{Name: "instructions"}

	agents := wiredAgents(projectDir)
	if len(agents) == 0 {
		// Nothing wired, so there is no agent whose needs we can check against.
		// Demanding a file for all five here would double-report the missing
		// wiring that `mcp wiring` already warns about, so this degrades to the
		// older question — is there a briefing at all — while still reporting a
		// stale one.
		return checkAnyInstructions(projectDir)
	}

	// An agent whose only briefing is stale is "uncovered", but reporting it in
	// both lists says the same thing twice and buries the actionable half. It
	// is counted as outdated only.
	var covered, uncovered, stale []string
	seenStale := map[string]bool{}
	for _, a := range agents {
		if a.instructionFile == "" {
			continue
		}
		// Every source is inspected, not just the first that covers the agent.
		// A project block does not shadow the global one — Claude Code loads
		// the user file and the project file, so an outdated block in either
		// is still being fed to the model and has to be reported.
		hit, outdated := "", false
		for _, cand := range instructionSources(projectDir, a) {
			switch inspectBlock(cand.path) {
			case blockCurrent, blockCustom:
				if hit == "" {
					hit = cand.label
				}
			case blockStale:
				outdated = true
				if !seenStale[cand.path] {
					seenStale[cand.path] = true
					stale = append(stale, cand.label)
				}
			}
		}
		switch {
		case hit != "":
			covered = append(covered, fmt.Sprintf("%s → %s", a.name, hit))
		case outdated:
			// already accounted for in `stale`
		default:
			uncovered = append(uncovered, a.name)
		}
	}

	switch {
	case len(uncovered) > 0 && len(stale) > 0:
		c.Status = "warn"
		c.Detail = fmt.Sprintf("nothing tells %s to use the memory tools, and %s %s an outdated block",
			strings.Join(uncovered, ", "), strings.Join(stale, ", "), carries(len(stale)))
		c.Hint = "re-run `graymatter init` — it adds what is missing and replaces the managed block in place, leaving everything outside the markers alone"
	case len(uncovered) > 0:
		c.Status = "warn"
		c.Detail = "nothing tells " + strings.Join(uncovered, ", ") + " to use the memory tools"
		c.Hint = "an MCP connection only makes tools *available* — without instructions the model never calls them; run `graymatter init` in this project (use `--global` only to install the instruction block home-wide)"
	case len(stale) > 0:
		// Everything is covered, but by a briefing from before v0.7.0 — the one
		// that told the model to search "when prior context might matter",
		// which is a condition it can resolve to false every time (issue #14).
		c.Status = "warn"
		c.Detail = strings.Join(stale, ", ") + " " + carries(len(stale)) + " a memory block from an older version"
		c.Hint = "the old block described the tools instead of prescribing a procedure, which is why agents did not call them; re-run `graymatter init` to replace it in place"
	default:
		c.Status, c.Detail = "ok", strings.Join(covered, ", ")
	}
	return c
}

// carries agrees the verb with the number of files, so a single outdated file
// does not read as a grammar slip in the one message meant to be trusted.
func carries(n int) string {
	if n == 1 {
		return "carries"
	}
	return "carry"
}

// checkAnyInstructions answers "is there a briefing anywhere" for a project
// with no MCP client wired yet. Staleness is still reported: an old block is
// worth flagging whether or not anything is wired.
func checkAnyInstructions(projectDir string) checkResult {
	c := checkResult{Name: "instructions"}
	var present, stale []string
	for _, a := range knownAgents(projectDir) {
		if a.instructionFile == "" {
			continue
		}
		for _, cand := range instructionSources(projectDir, a) {
			switch inspectBlock(cand.path) {
			case blockCurrent, blockCustom:
				if !contains(present, cand.label) {
					present = append(present, cand.label)
				}
			case blockStale:
				if !contains(stale, cand.label) {
					stale = append(stale, cand.label)
				}
			}
		}
	}
	switch {
	case len(present) == 0 && len(stale) == 0:
		c.Status = "warn"
		c.Detail = "neither CLAUDE.md nor AGENTS.md tells the model to use the memory tools"
		c.Hint = "an MCP connection only makes tools *available* — without instructions the model never calls them; run `graymatter init` to add the memory block"
	case len(present) == 0:
		c.Status = "warn"
		c.Detail = strings.Join(stale, ", ") + " " + carries(len(stale)) + " a memory block from an older version"
		c.Hint = "the old block described the tools instead of prescribing a procedure, which is why agents did not call them; re-run `graymatter init` to replace it in place"
	default:
		c.Status, c.Detail = "ok", strings.Join(present, ", ")+" mention the memory tools"
		if len(stale) > 0 {
			c.Status = "warn"
			c.Detail += "; " + strings.Join(stale, ", ") + " " + map[bool]string{true: "is", false: "are"}[len(stale) == 1] + " outdated"
			c.Hint = "re-run `graymatter init` to replace the managed block in place"
		}
	}
	return c
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// instructionSources lists where a briefing for this agent could live, project
// file first: a project block overrides, and is what the user most likely means
// when both exist.
func instructionSources(projectDir string, a agentDef) []struct{ path, label string } {
	out := []struct{ path, label string }{
		{filepath.Join(projectDir, a.instructionFile), a.instructionFile},
	}
	if a.globalInstruction != nil {
		if p, err := a.globalInstruction(); err == nil {
			out = append(out, struct{ path, label string }{p, p + " (global)"})
		}
	}
	return out
}

// lsofHint suggests the lock-holder lookup command on platforms that have one.
func lsofHint(dbPath string) string {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return fmt.Sprintf(" (find the holder with `lsof %s`)", dbPath)
	}
	return ""
}
