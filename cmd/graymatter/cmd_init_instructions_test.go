package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestUpsertInstructions_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !res.changed {
		t.Error("expected changed=true on first write")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, want := range []string{instrBeginMarker, instrEndMarker, "memory_search", "memory_reflect", "`agent_id` like every other tool", "memory_alias", "memory_search_batch", "action=\"pin\"", "weak-match", "hook recall ran"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("created file missing %q", want)
		}
	}
}

func TestUpsertInstructions_AppendsPreservingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	const userContent = "# My project\n\nDo not break userspace.\n"
	if err := os.WriteFile(path, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !res.changed {
		t.Error("expected changed=true when appending")
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.HasPrefix(content, "# My project") {
		t.Error("user content was not preserved at the top")
	}
	if !strings.Contains(content, "Do not break userspace.") {
		t.Error("user content line lost")
	}
	if !strings.Contains(content, instrBeginMarker) {
		t.Error("block not appended")
	}
}

func TestUpsertInstructions_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	if _, err := upsertInstructionsBlock(path); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, _ := os.ReadFile(path)

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if res.changed {
		t.Error("second upsert should be a no-op (changed=false)")
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Error("second upsert altered the file")
	}
}

func TestUpsertInstructions_ReplacesManagedBlockOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	prior := "# Header\n\n" +
		instrBeginMarker + "\nOLD STALE BLOCK CONTENT\n" + instrEndMarker + "\n\n" +
		"## Footer the user wrote\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !res.changed {
		t.Error("expected changed=true when replacing a stale block")
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if strings.Contains(content, "OLD STALE BLOCK CONTENT") {
		t.Error("stale block content not replaced")
	}
	if !strings.HasPrefix(content, "# Header") {
		t.Error("content before block lost")
	}
	if !strings.Contains(content, "## Footer the user wrote") {
		t.Error("content after block lost")
	}
	if !strings.Contains(content, "memory_search") {
		t.Error("fresh block content missing")
	}
}

// TestInstructionsBlock_IsProcedural guards the property that issue #14 turned
// on: the block has to tell the model what to do and when, not just describe
// the tools. The earlier version listed the five tools and gated the search on
// "when prior context might matter", which a model can read as never. If these
// anchors disappear the briefing has drifted back to being documentation.
func TestInstructionsBlock_IsProcedural(t *testing.T) {
	block := instructionsBlock()

	for _, want := range []string{
		"Every session, without exception",    // checkpoint/session protocol remains mandatory
		"Before your first substantive reply", // concrete first action
		"Hooks and MCP are\ncomplementary",    // neither integration is disabled
		"hook recall ran",                     // describes the hook/MCP handshake
		"session's initial turn",              // prior-turn markers do not count
		"run both project and `__shared__`",   // namespace mismatch cannot hide deduped facts
		"every missing section",               // empty scopes fall back to MCP
		"focused, ad-hoc lookups",             // explicit searches remain available
		"__shared__",                          // shared namespace is discoverable
		"What triggers a call",                // trigger table, not prose
		"root directory",                      // agent_id is derivable, not invented
		"checkpoint_resume",
		"checkpoint_save",
		// --global puts this block in projects that may not have GrayMatter
		// wired, so it has to guard on the tools existing. The guard is a
		// capability check on purpose: phrased as judgement it would recreate
		// the very hedge that made the old block ignorable.
		"not in your toolbelt",
		"not a\njudgement call",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block no longer contains %q", want)
		}
	}

	// The conditional phrasing that made the old block ignorable.
	for _, unwanted := range []string{"might matter", "when prior context"} {
		if strings.Contains(block, unwanted) {
			t.Errorf("block reintroduced conditional phrasing %q", unwanted)
		}
	}

	// The template placeholder must be fully rendered: a stray ~ means a code
	// span leaked into the generated markdown.
	if strings.Contains(block, "~") {
		t.Error("block contains an unrendered ~ placeholder")
	}
	if strings.Contains(block, hookRecallMarkerPrefix) {
		t.Error("static instructions reproduce the live hook-marker prefix")
	}

	// Guard against stray non-ASCII in the briefing the user actually reads,
	// with an allowlist for the glyphs it uses on purpose. Scoped to the body:
	// instrBeginMarker predates this and cannot be retyped without breaking the
	// upsert on every file that already carries the old marker.
	const allowed = "⚠"
	for _, r := range strings.ReplaceAll(blockTmpl, "~", "`") {
		if r > 127 && !strings.ContainsRune(allowed, r) {
			t.Errorf("unexpected non-ASCII rune %q in generated block body", r)
		}
	}
}

// TestInitGlobalScopeContract prevents --global from drifting back into the
// misleading promise that one invocation wires every repository. The command
// still performs the normal current-project init; only agent instructions are
// written home-wide, while project-scoped MCP configs remain per project.
func TestInitGlobalScopeContract(t *testing.T) {
	cmd := initCmd()
	for _, want := range []string{
		"does not replace the normal setup of the current project",
		"Project-scoped MCP configs remain per project",
		"init or manual configuration",
		"Codex is the exception",
	} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("init long help lost --global scope contract %q", want)
		}
	}
	flag := cmd.Flags().Lookup("global")
	if flag == nil {
		t.Fatal("init has no --global flag")
	}
	for _, want := range []string{"current-project setup still runs", "remain per project"} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("--global help lost scope contract %q: %q", want, flag.Usage)
		}
	}
}

func TestInitGlobal_PerformsLocalSetupAndOnlyGlobalizesInstructions(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "current-project")
	otherProject := filepath.Join(base, "other-project")
	home := filepath.Join(base, "home")
	for _, dir := range []string{project, otherProject, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldDataDir, oldHome, oldQuiet := dataDir, testHomeOverride, quiet
	dataDir = filepath.Join(project, ".graymatter")
	testHomeOverride = home
	quiet = true
	t.Cleanup(func() {
		dataDir, testHomeOverride, quiet = oldDataDir, oldHome, oldQuiet
		_ = os.Chdir(oldWD)
	})
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}

	cmd := initCmd()
	cmd.SetArgs([]string{"--global", "--only", "claudecode", "--no-path"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init --global: %v", err)
	}

	for _, path := range []string{
		filepath.Join(project, ".graymatter", "MEMORY.md"),
		filepath.Join(project, ".mcp.json"),
		filepath.Join(project, "CLAUDE.md"),
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(home, ".config", "opencode", "AGENTS.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected init --global output %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(otherProject, ".mcp.json"),
		filepath.Join(otherProject, "opencode.jsonc"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("init --global must not wire another project, stat %s: %v", path, err)
		}
	}
}

// TestGlobalInstructionPaths_HonoursXDG pins where --global writes. OpenCode
// resolves its global config through XDG_CONFIG_HOME when that is set, so
// hardcoding ~/.config would put the block somewhere nothing reads and make
// --global look like it worked while doing nothing.
func TestGlobalInstructionPaths_HonoursXDG(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })

	// Unset: ~/.config/opencode on every platform, Windows included.
	t.Setenv("XDG_CONFIG_HOME", "")
	paths, err := globalInstructionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "opencode", "AGENTS.md"); paths[1] != want {
		t.Errorf("default opencode path = %q, want %q", paths[1], want)
	}

	// Set: it wins.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	paths, err = globalInstructionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "opencode", "AGENTS.md"); paths[1] != want {
		t.Errorf("XDG opencode path = %q, want %q", paths[1], want)
	}

	// Claude Code's own path is home-based and unaffected by XDG.
	if want := filepath.Join(home, ".claude", "CLAUDE.md"); paths[0] != want {
		t.Errorf("claude path = %q, want %q", paths[0], want)
	}
}

func TestWriteGlobalInstructionFiles(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })
	// XDG_CONFIG_HOME outranks the home directory for the OpenCode path, and it
	// is set on some CI runners. Without pinning it this test would assert
	// against the wrong location and, worse, write into the real config dir.
	t.Setenv("XDG_CONFIG_HOME", "")

	results := writeGlobalInstructionFiles()
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	for _, rel := range []string{
		filepath.Join(".claude", "CLAUDE.md"),
		filepath.Join(".config", "opencode", "AGENTS.md"),
	} {
		data, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			t.Fatalf("%s not written: %v", rel, err)
		}
		if !strings.Contains(string(data), "memory_search") {
			t.Errorf("%s missing the memory block", rel)
		}
	}

	// Second run is a no-op: the block is managed by markers, so re-running
	// init --global must not stack duplicates in a user's global file.
	for _, res := range writeGlobalInstructionFiles() {
		if res.changed {
			t.Errorf("second run rewrote %s", res.path)
		}
	}
}

func TestWriteGlobalInstructionFiles_PreservesUserContent(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })
	t.Setenv("XDG_CONFIG_HOME", "")

	claude := filepath.Join(home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	const userContent = "# My global rules\n\nAlways answer in Spanish.\n"
	if err := os.WriteFile(claude, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	writeGlobalInstructionFiles()

	data, _ := os.ReadFile(claude)
	if !strings.HasPrefix(string(data), userContent) {
		t.Error("existing global instructions were not preserved")
	}
	if !strings.Contains(string(data), "memory_search") {
		t.Error("memory block not appended to the global file")
	}
}

func TestInspectBlock(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The pre-v0.7.0 briefing, markers and all. This is the file the users in
	// issue #14 were left with, and the reason "is there a block" was never a
	// good enough question.
	staleBody := instrBeginMarker + `
## Memory (GrayMatter)

- ` + "`memory_search`" + ` — call at the start of a task when prior context might matter.
` + instrEndMarker + "\n"

	managed := filepath.Join(dir, "CLAUDE.md")
	if _, err := upsertInstructionsBlock(managed); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want blockStatus
	}{
		{"managed block we just wrote", managed, blockCurrent},
		{"pre-0.7 block", write("STALE.md", staleBody), blockStale},
		{"hand-written mention", write("AGENTS.md", "Use the GrayMatter MCP tools.\n"), blockCustom},
		{"unrelated file", write("OTHER.md", "nothing to see\n"), blockAbsent},
		{"missing file", filepath.Join(dir, "MISSING.md"), blockAbsent},

		// A CRLF checkout must not read as stale, or every Windows user gets a
		// warning they cannot clear.
		{"current block in a CRLF file", write("CRLF.md",
			strings.ReplaceAll(string(current), "\n", "\r\n")), blockCurrent},

		// One current block plus one stale copy is a stale file: the model is
		// still being fed the old text.
		{"current block next to a stale one", write("BOTH.md",
			string(current)+"\n"+staleBody), blockStale},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inspectBlock(c.path); got != c.want {
				t.Errorf("inspectBlock = %v, want %v", got, c.want)
			}
		})
	}
}

// TestUpsertInstructions_PreservesLineEndings covers what a Windows checkout
// does to this file. With core.autocrlf=true the block comes back from git as
// CRLF; splicing LF into it left the file with both, and because the whole-file
// comparison then never matched, every init rewrote it and reported a change.
func TestUpsertInstructions_PreservesLineEndings(t *testing.T) {
	crCount := func(s string) int { return strings.Count(s, "\r") }

	t.Run("crlf file stays crlf", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "CLAUDE.md")
		if err := os.WriteFile(path, []byte("# Project\r\nnotes\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := upsertInstructionsBlock(path); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		got := string(data)
		if lf := strings.Count(got, "\n"); crCount(got) != lf {
			t.Errorf("mixed endings: %d CR vs %d LF", crCount(got), lf)
		}

		// The second run has to be a no-op, or git sees a dirty tree after
		// every init.
		res, err := upsertInstructionsBlock(path)
		if err != nil {
			t.Fatal(err)
		}
		if res.changed {
			t.Error("second upsert rewrote a file it had just written")
		}
	})

	t.Run("lf file with a stray crlf stays lf", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "CLAUDE.md")
		if err := os.WriteFile(path, []byte("# Project\na\nb\r\nc\nd\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := upsertInstructionsBlock(path); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		// One pasted line must not convert the block. The single pre-existing
		// CR is the only one that should survive.
		if n := crCount(string(data)); n != 1 {
			t.Errorf("got %d CR, want the 1 that was already there", n)
		}
	})
}

// TestUpsertInstructions_CollapsesDuplicateBlocks: replacing only the first
// marker pair left later copies untouched, so a file could carry a current
// block and a stale one at the same time and still look fine to anything that
// read the first pair and stopped.
func TestUpsertInstructions_CollapsesDuplicateBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	stale := instrBeginMarker + "\nold briefing\n" + instrEndMarker
	body := "# Project\n\nkeep me\n\n" + stale + "\n\nmiddle\n\n" + stale + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := upsertInstructionsBlock(path); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))

	if n := strings.Count(got, instrBeginMarker); n != 1 {
		t.Errorf("got %d blocks, want 1", n)
	}
	if strings.Contains(got, "old briefing") {
		t.Error("a stale copy survived the upsert")
	}
	for _, keep := range []string{"keep me", "middle"} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost user content %q", keep)
		}
	}
}

// TestUpsertInstructions_LeavesOrphanMarkerAlone: a begin marker with no
// terminator of its own is not a block this tool wrote. Pairing it with the
// next block's end marker would delete every line in between.
func TestUpsertInstructions_LeavesOrphanMarkerAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	body := "# Project\n" + instrBeginMarker + "\nhand-written note\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := upsertInstructionsBlock(path); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))
	if !strings.Contains(got, "hand-written note") {
		t.Error("orphan marker swallowed the user's content")
	}
	if !strings.Contains(got, instrEndMarker) {
		t.Error("no managed block was written")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// instructionsBlockTokenBudget is the ceiling enforced by
// TestInstructionsBlockBudget. The block is the single most expensive piece of
// recurring copy GrayMatter ships: unlike the MCP handshake, which rides one
// initialize per session, this text lives in CLAUDE.md / AGENTS.md and is read
// on every turn of every session of every project it was installed into. The
// handshake carries a 240-token ceiling for a 210-token string
// (cmd/graymatter/internal/mcp/instructions.go); this is the same discipline at
// the same ~14% headroom, applied where the cost is roughly four times larger.
// Raise it in this constant, with reasoning, rather than letting the copy grow
// silently.
const instructionsBlockTokenBudget = 1060

// TestInstructionsBlockBudget pins the recurring token cost of the installed
// briefing with the same estimator the handshake budget and the benchmarks use,
// so the numbers here, there and in docs all mean the same thing.
func TestInstructionsBlockBudget(t *testing.T) {
	n := tokens.Approx(instructionsBlock())
	if n == 0 {
		t.Fatal("token estimator returned 0 for a non-empty block; estimator or template broke")
	}
	if n > instructionsBlockTokenBudget {
		t.Fatalf("installed instructions block costs %d tokens, budget is %d — shorten the copy or raise the budget with reasoning",
			n, instructionsBlockTokenBudget)
	}
}

// TestInstructionsBlock_ToolCensusContract pins the generated briefing against
// the MCP surface it teaches (issues #111/#112). The live tools/list side is
// pinned by TestToolDefinitionContract in internal/mcp; this pins the block
// side to the same contract without hardcoding the tool list: tool names are
// derived from the MCP registrations in internal/mcp/server.go, the reflect
// anyOf shape is read from the real RawInputSchema JSON, and the handshake
// briefing is read from internal/mcp/instructions.go. Adding a new tool and
// updating the MCP contract test turns this red while the briefing is stale.
// Anchors, not prose: wording may evolve, the contract may not drift.
func TestInstructionsBlock_ToolCensusContract(t *testing.T) {
	block := instructionsBlock()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller cannot locate test file; cross-surface check needs its own directory")
	}
	base := filepath.Dir(thisFile)
	serverSrc, err := os.ReadFile(filepath.Join(base, "internal", "mcp", "server.go"))
	if err != nil {
		t.Fatalf("read MCP server surface: %v", err)
	}
	instrSrc, err := os.ReadFile(filepath.Join(base, "internal", "mcp", "instructions.go"))
	if err != nil {
		t.Fatalf("read server instructions: %v", err)
	}

	// Tool set derived from the MCP surface: every mcp.NewTool("name") in
	// registerTools. No hardcoded list here by design, so a new registration
	// with a stale briefing fails below instead of passing silently.
	toolRe := regexp.MustCompile(`NewTool\("([^"]+)"`)
	seen := map[string]bool{}
	var derived []string
	for _, m := range toolRe.FindAllSubmatch(serverSrc, -1) {
		name := string(m[1])
		if !seen[name] {
			seen[name] = true
			derived = append(derived, name)
		}
	}
	sort.Strings(derived)
	if len(derived) == 0 {
		t.Fatal("derived zero tools from internal/mcp/server.go; registration parse broke")
	}

	// Every registered tool has a row in the generated tools table. Table rows
	// open with "| `name` |", which trigger-table prose never does.
	for _, name := range derived {
		if row := "| `" + name + "` |"; !strings.Contains(block, row) {
			t.Errorf("generated block has no tools-table row for registered tool %q", name)
		}
	}

	// No extra tools-table rows beyond the registered set: a removed tool must
	// not linger in the briefing.
	rowRe := regexp.MustCompile(`(?m)^\| ` + "`" + `([a-z_]+)` + "`" + ` \|`)
	for _, m := range rowRe.FindAllSubmatch([]byte(block), -1) {
		name := string(m[1])
		// The tools table is the only table whose rows open with a backticked
		// name; still, skip the header word if it ever matches.
		if name == "Tool" {
			continue
		}
		// Trigger-table rows open with prose ("| The ..."), so any other
		// backticked opener here belongs to the tools table.
		if !seen[name] {
			t.Errorf("generated block has tools-table row for %q with no MCP registration", name)
		}
	}

	// Reflect identity comes from the real schema JSON, not from prose memory.
	// server.go carries RawInputSchema as a backticked JSON literal; extract
	// and decode it here so a schema edit moves this test with it.
	rawJSON := extractReflectRawSchema(t, string(serverSrc))
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
		AnyOf    []struct {
			Required []string `json:"required"`
		} `json:"anyOf"`
		RawOneOf json.RawMessage `json:"oneOf"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &schema); err != nil {
		t.Fatalf("decode reflect RawInputSchema: %v", err)
	}
	for _, prop := range []string{"action", "agent_id", "agent", "text", "target"} {
		if _, ok := schema.Properties[prop]; !ok {
			t.Errorf("reflect schema missing property %q", prop)
		}
	}
	if len(schema.Required) != 1 || schema.Required[0] != "action" {
		t.Errorf("reflect schema required = %v, want [action] with agent identity in anyOf", schema.Required)
	}
	if len(schema.RawOneOf) != 0 {
		t.Error("reflect schema must use anyOf (at least one, both allowed), not oneOf (exactly one)")
	}
	if len(schema.AnyOf) != 2 {
		t.Fatalf("reflect schema anyOf has %d branches, want 2 (agent_id / agent)", len(schema.AnyOf))
	}
	sawID, sawAlias := false, false
	for _, branch := range schema.AnyOf {
		if len(branch.Required) == 1 {
			switch branch.Required[0] {
			case "agent_id":
				sawID = true
			case "agent":
				sawAlias = true
			}
		}
	}
	if !sawID || !sawAlias {
		t.Errorf("reflect anyOf must offer the agent_id and agent alternatives (got id=%v alias=%v)", sawID, sawAlias)
	}
	agentDesc := schema.Properties["agent"].Description
	if !strings.Contains(strings.ToLower(agentDesc), "deprecated") {
		t.Error("reflect schema must mark `agent` deprecated")
	}
	if !strings.Contains(agentDesc, "agent_id wins") {
		t.Error("reflect schema must document that `agent_id` wins when both are set")
	}

	// The block must teach that same contract: canonical agent_id, deprecated
	// alias, at-least-one required, agent_id wins. Anchors, not full prose.
	if row := "| `memory_reflect` | `action`, `agent_id`"; !strings.Contains(block, row) {
		t.Errorf("reflect row does not spell `agent_id` canonical: %q", row)
	}
	if !strings.Contains(block, "deprecated alias") {
		t.Error("generated block never says `agent` is a deprecated alias")
	}
	if !strings.Contains(block, "at least one") {
		t.Error("generated block must say at least one of agent_id/agent is required (anyOf allows both)")
	}
	if !strings.Contains(block, "agent_id` wins") && !strings.Contains(block, "agent_id wins") {
		t.Error("generated block must say `agent_id` wins when both are set")
	}
	for _, stale := range []string{"exactly one of", "Mixing them up fails validation", "`agent`, not `agent_id`"} {
		if strings.Contains(block, stale) {
			t.Errorf("generated block still carries stale reflect wording %q", stale)
		}
	}

	// The handshake briefing must use the same canonical names, never stale
	// agent-only reflect wording.
	instrText := string(instrSrc)
	if !strings.Contains(instrText, "agent_id") {
		t.Error("serverInstructions never names the canonical `agent_id`")
	}
	if !strings.Contains(instrText, "memory_search") || !strings.Contains(instrText, "memory_reflect") {
		t.Error("serverInstructions must name the tools it tells the model to call")
	}
	for _, stale := range []string{"exactly one of", "Mixing them up fails validation", "`agent`, not `agent_id`"} {
		if strings.Contains(instrText, stale) {
			t.Errorf("serverInstructions still carries stale reflect wording %q", stale)
		}
	}

	// The alias action is the shared constant, not a hardcoded literal that
	// can drift from the handshake (serverInstructions) and the CLI.
	if !strings.Contains(block, "`"+memory.FeedbackAction+"`") {
		t.Errorf("generated block does not render the shared alias action %q", memory.FeedbackAction)
	}
	if strings.Contains(block, "ALIAS_TOOL") {
		t.Error("generated block leaks the unrendered ALIAS_TOOL placeholder")
	}
}

// TestInstructionsBlock_DocsContract pins the focused docs surface for issue
// #112: docs/AGENTS.md and docs/api-stability.md must teach the same
// seven-tool census and canonical reflect contract as the generated block and
// the MCP schema. Anchors only, no full-prose semantic diff. Skips only when
// a docs file is absent; wrong content fails.
func TestInstructionsBlock_DocsContract(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller cannot locate test file; docs check needs its own directory")
	}
	base := filepath.Dir(thisFile)
	agentsPath := filepath.Join(base, "..", "..", "docs", "AGENTS.md")
	stabilityPath := filepath.Join(base, "..", "..", "docs", "api-stability.md")

	agentsRaw, err := os.ReadFile(agentsPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("docs contract skipped: %s absent", agentsPath)
		}
		t.Fatalf("read docs/AGENTS.md: %v", err)
	}
	stabilityRaw, err := os.ReadFile(stabilityPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("docs contract skipped: %s absent", stabilityPath)
		}
		t.Fatalf("read docs/api-stability.md: %v", err)
	}
	agentsDoc := string(agentsRaw)
	stabilityDoc := string(stabilityRaw)
	docs := []struct {
		name string
		src  string
	}{
		{"docs/AGENTS.md", agentsDoc},
		{"docs/api-stability.md", stabilityDoc},
	}

	// (a) Seven-tool census: batch + alias rows present in both docs, with no
	// stale five-tool counts.
	for _, d := range docs {
		t.Run(d.name+" census", func(t *testing.T) {
			for _, row := range []string{"| `memory_search_batch`", "| `memory_alias`"} {
				if !strings.Contains(d.src, row) {
					t.Errorf("%s missing tool-table row %q", d.name, row)
				}
			}
			for _, tool := range []string{"memory_search_batch", "memory_alias", "memory_add", "memory_reflect", "checkpoint_save", "checkpoint_resume"} {
				if !strings.Contains(d.src, "`"+tool+"`") {
					t.Errorf("%s never names tool %q", d.name, tool)
				}
			}
			if !strings.Contains(d.src, "`memory_search`") {
				t.Errorf("%s never names tool %q", d.name, "memory_search")
			}
			for _, stale := range []string{"Five tools are registered", "Five tools", "five tools"} {
				if strings.Contains(d.src, stale) {
					t.Errorf("%s carries stale tool count %q", d.name, stale)
				}
			}
		})
	}
	// docs/AGENTS.md states the seven-tool count explicitly.
	if !strings.Contains(agentsDoc, "Seven tools are registered") {
		t.Error("docs/AGENTS.md must state the seven-tool census (\"Seven tools are registered\")")
	}

	// (b) Canonical reflect contract in both docs.
	for _, d := range docs {
		t.Run(d.name+" reflect", func(t *testing.T) {
			for _, want := range []string{"agent_id", "canonical", "deprecated", "anyOf", "at least one", "wins"} {
				if !strings.Contains(d.src, want) {
					t.Errorf("%s missing reflect anchor %q", d.name, want)
				}
			}
			if !strings.Contains(d.src, "deprecated alias") {
				t.Errorf("%s must say `agent` is a deprecated alias", d.name)
			}
			if !strings.Contains(d.src, "agent_id` wins") && !strings.Contains(d.src, "agent_id wins") {
				t.Errorf("%s must say `agent_id` wins when both are set", d.name)
			}
			for _, stale := range []string{"exactly one of", "`agent`, not `agent_id`", "Mixing them up fails validation"} {
				if strings.Contains(d.src, stale) {
					t.Errorf("%s carries stale reflect wording %q", d.name, stale)
				}
			}
		})
	}

	// (c) memory_search row mentions explain in docs/AGENTS.md (the schema has it).
	found := false
	for _, line := range strings.Split(agentsDoc, "\n") {
		if strings.Contains(line, "memory_search") && strings.Contains(line, "explain") {
			found = true
			break
		}
	}
	if !found {
		t.Error("docs/AGENTS.md memory_search row must mention `explain`")
	}
}

// extractReflectRawSchema returns the JSON literal assigned to
// reflectTool.RawInputSchema in server.go source.
func extractReflectRawSchema(t *testing.T, src string) string {
	t.Helper()
	anchor := src[strings.Index(src, "RawInputSchema"):]
	if anchor == "" {
		t.Fatal("RawInputSchema assignment missing from internal/mcp/server.go")
	}
	open := strings.Index(anchor, "`")
	if open < 0 {
		t.Fatal("RawInputSchema JSON literal missing opening backtick")
	}
	rest := anchor[open+1:]
	close := strings.Index(rest, "`")
	if close < 0 {
		t.Fatal("RawInputSchema JSON literal missing closing backtick")
	}
	return rest[:close]
}
