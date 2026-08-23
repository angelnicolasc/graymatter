package docaudit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/contextblock"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

var auditOpts = Options{Now: time.Now()}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func severities(rep *Report) map[string][]Severity {
	m := map[string][]Severity{}
	for _, f := range rep.Findings {
		m[f.Check] = append(m[f.Check], f.Severity)
	}
	return m
}

const cleanBody = `# Project guide

Use table-driven tests for anything with more than two cases.
Deployments go through the staging pipeline before production.
The latency budget for the search path is 300 milliseconds.

## Conventions

Commits follow the conventional-commit format with package scopes.
Reviews require one approval from an owner of the touched module.
Feature flags are removed within two weeks of full rollout.
`

func TestAudit_CleanDocumentHasNoFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "# Guide\n\n"+cleanBody)
	rep, err := AuditPath(dir, auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Severity == SevWarn || f.Severity == SevFail {
			t.Errorf("clean document produced %s finding: %+v", f.Severity, f)
		}
	}
}

func TestAudit_PlantedDuplicates(t *testing.T) {
	dir := t.TempDir()
	dup := strings.Repeat("The migration tool replays journal entries in strict chronological order every night.\n\n", 2)
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "# Ops\n\n"+dup+"\nUnrelated closing paragraph with entirely different vocabulary about testing.\n")
	rep, err := AuditPath(dir, auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(severities(rep)["duplicates"]) != 1 {
		t.Fatalf("planted duplicate not detected: %+v", rep.Findings)
	}
}

func TestAudit_SizeThresholds(t *testing.T) {
	dir := t.TempDir()

	warnDoc := "# Big\n" + strings.Repeat("Line with some ordinary words to fill space.\n", 600)
	writeFile(t, filepath.Join(dir, "AGENTS.md"), warnDoc)
	rep, _ := AuditPath(dir, auditOpts)
	found := false
	for _, f := range rep.Findings {
		if f.Check == "size" && f.Severity == SevWarn {
			found = true
		}
	}
	if !found {
		t.Error("600-line document did not produce a size warning")
	}

	failDoc := "# Bigger\n" + strings.Repeat("Line with some ordinary words to fill space.\n", 1600)
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), failDoc)
	rep, _ = AuditPath(dir, auditOpts)
	found = false
	for _, f := range rep.Findings {
		if f.Check == "size" && f.Severity == SevFail {
			found = true
		}
	}
	if !found {
		t.Error("1600-line document did not produce a size failure")
	}
}

func TestAudit_MarkerDefects(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name       string
		content    string
		wantSev    Severity
		wantDetail string
	}{
		{
			name: "orphan begin marker",
			content: "text\n" +
				"<!-- graymatter:instructions:begin — managed -->\nuser bytes that were never inside a block\n" +
				"<!-- graymatter:instructions:begin — managed by init; edits inside this block are overwritten -->\npaired\n<!-- graymatter:instructions:end -->\n",
			wantSev:    SevFail,
			wantDetail: "orphaned",
		},
		{
			name: "duplicate instructions blocks",
			content: "first\n" +
				"<!-- graymatter:instructions:begin — managed by init; edits inside this block are overwritten -->\nbody\n<!-- graymatter:instructions:end -->\n" +
				"middle\n" +
				"<!-- graymatter:instructions:begin — managed by init; edits inside this block are overwritten -->\nbody again\n<!-- graymatter:instructions:end -->\n",
			wantSev:    SevWarn,
			wantDetail: "duplicate",
		},
		{
			name: "nested managed regions",
			content: "<!-- graymatter:instructions:begin — managed by init; edits inside this block are overwritten -->\n" +
				contextblock.BeginMarker + "\ninner\n" + contextblock.EndMarker + "\n" +
				"<!-- graymatter:instructions:end -->\n",
			wantSev:    SevFail,
			wantDetail: "nested",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, filepath.Join(dir, "AGENTS.md"), tc.content)
			rep, err := AuditPath(dir, auditOpts)
			if err != nil {
				t.Fatal(err)
			}
			hit := false
			for _, f := range rep.Findings {
				if f.Check == "markers" && f.Severity == tc.wantSev && strings.Contains(f.Detail, tc.wantDetail) {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("expected a %s markers finding mentioning %q, got %+v", tc.wantSev, tc.wantDetail, rep.Findings)
			}
			os.Remove(filepath.Join(dir, "AGENTS.md"))
		})
	}
}

func TestAudit_ContextHashMismatch(t *testing.T) {
	dir := t.TempDir()
	body := contextblock.RenderBody(nil)
	body = strings.Replace(body, contextblockHeading(), "## Memory context (GrayMatter) TAMPERED", 1)
	block := contextblock.RenderBlock(body, contextblock.SyncMeta{
		SHA256:   contextblock.HashBody(contextblock.RenderBody(nil)),
		Facts:    0,
		SyncedAt: time.Now().UTC(),
	})
	writeFile(t, filepath.Join(dir, "AGENTS.md"), block)
	rep, err := AuditPath(dir, auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, f := range rep.Findings {
		if f.Check == "markers" && f.Severity == SevWarn && strings.Contains(f.Detail, "hash mismatch") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("tampered context block not flagged: %+v", rep.Findings)
	}
}

func contextblockHeading() string { return "## Memory context (GrayMatter)" }

// --- staleness with real git --------------------------------------------------

// gitInitRepo creates a repository and returns a helper committing files with
// controlled author dates, so staleness buckets are testable without sleeps.
func gitInitRepo(t *testing.T) (dir string, commit func(name, content, date string)) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	run("config", "user.email", "audit@test")
	run("config", "user.name", "auditor")

	commit = func(name, content, date string) {
		writeFile(t, filepath.Join(dir, name), content)
		args := []string{"add", name}
		run(args...)
		env := os.Environ()
		if date != "" {
			env = append(env,
				"GIT_AUTHOR_DATE="+date,
				"GIT_COMMITTER_DATE="+date,
			)
		}
		cmd := exec.Command("git", "-c", "user.email=audit@test", "-c", "user.name=auditor", "commit", "-q", "-m", "add "+name)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit: %v\n%s", err, out)
		}
	}
	return dir, commit
}

func TestAudit_StalenessBucketsWithRealGit(t *testing.T) {
	dir, commit := gitInitRepo(t)

	old := "# Stale guide\n\n" + strings.Repeat("Ancient paragraph line written long ago for blame purposes.\n", 12)
	commit("AGENTS.md", old, "2020-01-15T00:00:00Z")

	// Uncommitted additions land in their own bucket.
	f, _ := os.OpenFile(filepath.Join(dir, "AGENTS.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("\nBrand new uncommitted line appended today.\n")
	f.Close()

	rep, err := AuditPath(dir, auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	st := rep.Files[0].Staleness
	if st == nil || !st.Available {
		t.Fatalf("staleness unavailable: %+v", st)
	}
	if st.Stale == 0 {
		t.Errorf("2020 lines did not land in the >90d bucket: %+v", st)
	}
	if st.Uncommitted == 0 {
		t.Errorf("uncommitted lines not counted: %+v", st)
	}
	if st.MedianAgeDays < MedianStaleDays {
		t.Errorf("median age %.0f below threshold while most lines are from 2020", st.MedianAgeDays)
	}
	warns := 0
	for _, f := range rep.Findings {
		if f.Check == "staleness" {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("expected exactly one staleness warning, got %d", warns)
	}
}

// --- precision harness: the n=20 sample ---------------------------------------

// TestPrecisionHarness_TwentyDocuments is the automated half of the
// acceptance criterion "estimated false-positive rate over a manual sample of
// twenty cases before any public use": ten clean documents must produce zero
// warn/fail findings, ten documents with one planted defect each must trigger
// at least the planted check. It prints the confusion counts.
func TestPrecisionHarness_TwentyDocuments(t *testing.T) {
	dir, commit := gitInitRepo(t)
	opts := auditOpts

	type specimen struct {
		file     string
		content  string
		date     string // empty → committed now; "UNTRACKED" → never committed
		defect   string // "" for clean specimens
		expected string // check name expected to fire on defective ones
	}
	var specs []specimen

	// --- ten clean documents -------------------------------------------------
	for i := 0; i < 10; i++ {
		body := fmt.Sprintf("# Service %d runbook\n\n"+
			"Rotate credentials every ninety days using the vault pipeline.\n"+
			"Alert thresholds page the on-call engineer at severity one.\n"+
			"Dashboards live under the observability tab of team %d.\n"+
			"Runbooks are reviewed quarterly by the platform guild %d.\n\n"+
			"## Escalation %d\n\n"+
			"Page secondary first when primary does not ack within five minutes.\n"+
			"Post incident reviews are due within three business days of resolution.\n",
			i, i, i, i)
		switch i {
		case 3:
			body += "\n" + validInstructionsBlock() + "\n"
		case 6:
			sel, _ := contextblock.Select(nil, 512)
			body += "\n" + contextblock.RenderBlock(contextblock.RenderBody(sel), contextblock.SyncMeta{
				SHA256:   contextblock.HashBody(contextblock.RenderBody(sel)),
				SyncedAt: time.Now().UTC(),
			}) + "\n"
		case 9:
			body += "\nThis tenth file stays untracked to exercise that path.\n"
		}
		sp := specimen{file: fmt.Sprintf("doc-%02d.md", i), content: body}
		if i == 9 {
			sp.date = "UNTRACKED"
		} else {
			sp.date = time.Now().UTC().Format(time.RFC3339)
		}
		specs = append(specs, sp)
	}

	// --- ten documents with one planted defect each --------------------------
	long := "# Long\n" + strings.Repeat("Filler sentence with several ordinary words repeated.\n", 600)
	tooLong := "# Longer\n" + strings.Repeat("Filler sentence with several ordinary words repeated.\n", 1600)

	specs = append(specs,
		specimen{file: "def-00-duplicate.md", content: "# Dup\n\n" + strings.Repeat("The importer validates checksum rows before writing any batch to disk.\n\n", 2) + "Trailing unique paragraph about unrelated backup retention windows.\n", defect: "duplicates", expected: "duplicates"},
		specimen{file: "def-01-size-warn.md", content: long, defect: "size-warn", expected: "size"},
		specimen{file: "def-02-size-fail.md", content: tooLong, defect: "size-fail", expected: "size"},
		specimen{file: "def-03-orphan.md", content: "text\n" + "<!-- graymatter:context:begin — managed by `graymatter context-sync`; edits inside this block are overwritten -->\nno end anywhere\n", defect: "orphan", expected: "markers"},
		specimen{file: "def-04-dup-blocks.md", content: "a\n" + validInstructionsBlock() + "\nb\n" + validInstructionsBlock() + "\n", defect: "duplicate-blocks", expected: "markers"},
		specimen{file: "def-05-hash.md", content: tamperedContextBlock(), defect: "hash-mismatch", expected: "markers"},
		specimen{file: "def-06-nested.md", content: validInstructionsBlockBeginOnly() + contextblock.BeginMarker + "\nx\n" + contextblock.EndMarker + "\n" + "<!-- graymatter:instructions:end -->\n", defect: "nested", expected: "markers"},
		specimen{file: "def-07-stale.md", content: "# Old policy\n\n" + strings.Repeat("Superseded operational guidance retained far past its useful service life.\n", 8), date: "2019-06-01T00:00:00Z", defect: "stale", expected: "staleness"},
		specimen{file: "def-08-unterminated.md", content: validInstructionsBlock() + "\ntext after\n" + "<!-- graymatter:instructions:begin -- dangling\n", defect: "unterminated", expected: "markers"},
		specimen{file: "def-09-short-repeats-only.md", content: "# Heading\n\n- alpha\n- beta\n\nTotally distinct body copy so only short lines repeat here.\n", defect: "none-short-lines", expected: ""},
	)

	// Materialise + commit.
	for i, sp := range specs {
		p := filepath.Join(dir, sp.file)
		writeFile(t, p, sp.content)
		switch sp.date {
		case "":
			commit(sp.file, sp.content, "")
		case "UNTRACKED":
			// left out of git on purpose
		default:
			commit(sp.file, sp.content, sp.date)
		}
		_ = i
	}

	// Run once per specimen against its own directory? No — one repo, but
	// AuditPath takes a FILE path directly, which is what we use here.
	truePositives, falsePositives, trueNegatives := 0, 0, 0
	for _, sp := range specs {
		rep, err := AuditPath(filepath.Join(dir, sp.file), opts)
		if err != nil {
			t.Fatalf("%s: %v", sp.file, err)
		}
		var hits []Finding
		for _, f := range rep.Findings {
			if f.Severity == SevWarn || f.Severity == SevFail {
				hits = append(hits, f)
			}
		}
		if sp.defect == "" || sp.defect == "none-short-lines" {
			if len(hits) > 0 {
				falsePositives++
				t.Errorf("FP on clean %s: %+v", sp.file, hits)
			} else {
				trueNegatives++
			}
			continue
		}
		ok := false
		for _, h := range hits {
			if h.Check == sp.expected {
				ok = true
			}
		}
		if ok {
			truePositives++
		} else {
			t.Errorf("missed defect %q (%s): got %+v", sp.defect, sp.file, rep.Findings)
		}
	}

	t.Logf("harness over %d documents: planted defects caught %d/9; negative controls silent %d/11; false positives=%d",
		len(specs), truePositives, trueNegatives, falsePositives)
	if falsePositives != 0 {
		t.Errorf("false positives on clean documents = %d, want 0", falsePositives)
	}
	if truePositives != 9 {
		t.Errorf("detected defects = %d, want 9", truePositives)
	}
	if trueNegatives != 11 {
		t.Errorf("negative controls = %d, want 11 (10 clean + 1 short-line control)", trueNegatives)
	}
}

func TestFindDuplicates_NearBoundaryThreshold(t *testing.T) {
	// Pins the declared Jaccard threshold from both sides: a pair scoring
	// between DupThreshold and 1.0 must warn, a pair below it must stay
	// silent. Identical paragraphs alone cannot pin the threshold because
	// they score 1.0 against any bar.
	words := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%03d", prefix, i)
		}
		return out
	}
	base := words("w", 34)
	near := append(words("w", 32), words("x", 2)...) // J = 28/32 = 0.875 → warn
	far := append(words("w", 29), words("y", 5)...)  // J = 25/35 = 0.714 → silent
	lines := []string{
		strings.Join(base, " "),
		"",
		strings.Join(near, " "),
		"",
		strings.Join(far, " "),
	}
	pairs := findDuplicates(lines)
	if len(pairs) != 1 {
		t.Fatalf("got %d duplicate pairs, want exactly 1 (the ≥%.2f one): %+v", len(pairs), DupThreshold, pairs)
	}
	if pairs[0].Score < DupThreshold {
		t.Errorf("reported pair scores %.2f, below threshold", pairs[0].Score)
	}
}

func TestAudit_StalenessMixedAgesWithFixedClock(t *testing.T) {
	// Exercises every bucket plus the median with controlled commit dates and
	// an injected clock: six lines per committed band and two uncommitted.
	dir, commit := gitInitRepo(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	dateAt := func(daysAgo int) string { return now.AddDate(0, 0, -daysAgo).Format(time.RFC3339) }

	band := func(tag string) string {
		var b strings.Builder
		for i := 1; i <= 6; i++ {
			fmt.Fprintf(&b, "Band %s line %d carries ordinary words for blame purposes.\n", tag, i)
		}
		return b.String()
	}
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "# Ages\n\n"+band("old"))
	commit("AGENTS.md", "# Ages\n\n"+band("old"), dateAt(200))

	f, _ := os.OpenFile(filepath.Join(dir, "AGENTS.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(band("mid"))
	f.Close()
	commitAll(t, dir, dateAt(60))

	f, _ = os.OpenFile(filepath.Join(dir, "AGENTS.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(band("new"))
	f.Close()
	commitAll(t, dir, dateAt(10))

	f, _ = os.OpenFile(filepath.Join(dir, "AGENTS.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("Uncommitted line alpha appended just now.\nUncommitted line beta appended just now.\n")
	f.Close()

	rep, err := AuditPath(filepath.Join(dir, "AGENTS.md"), Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	st := rep.Files[0].Staleness
	if st == nil || !st.Available {
		t.Fatalf("staleness unavailable: %+v", st)
	}
	// Eight, not six: the heading and its blank line were committed with the
	// oldest band and blame keeps them there.
	if st.Recent != 6 || st.Aging != 6 || st.Stale != 8 || st.Uncommitted != 2 {
		t.Errorf("buckets recent=%d aging=%d stale=%d uncommitted=%d, want 6/6/8/2",
			st.Recent, st.Aging, st.Stale, st.Uncommitted)
	}
	// Median must sit strictly between the youngest and oldest line ages:
	// pins the order statistic, not just any single value.
	if !(st.MedianAgeDays > 11 && st.MedianAgeDays < 199) {
		t.Errorf("median %.1f outside the open interval (11, 199)", st.MedianAgeDays)
	}
	// With a 60-day median no staleness warning may fire: the gate sits at
	// MedianStaleDays, and this asserts it from the quiet side.
	for _, fnd := range rep.Findings {
		if fnd.Check == "staleness" {
			t.Errorf("60-day median produced a staleness finding: %+v", fnd)
		}
	}
}

// commitAll stages everything and commits with both dates overridden.
func commitAll(t *testing.T, dir, date string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	env := os.Environ()
	env = append(env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	cmd := exec.Command("git", "-c", "user.email=audit@test", "-c", "user.name=auditor", "commit", "-q", "-m", "band")
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
}

// --- fixtures -----------------------------------------------------------------

func validInstructionsBlock() string {
	return "<!-- graymatter:instructions:begin — managed by `graymatter init`; edits inside this block are overwritten -->\n" +
		"Use memory_search before answering; store durable facts after the task.\n" +
		"<!-- graymatter:instructions:end -->\n"
}

func TestAudit_MarkerSyntaxInsideFencedCodeIsNotFlagged(t *testing.T) {
	// Documentation that QUOTES the marker syntax inside fenced code blocks is
	// common and healthy; none of it is an active managed region. Every
	// finding here would be a public false positive with exit code 1.
	dir := t.TempDir()
	content := "# Guide\n\n" + cleanBody + "\n\n## Marker syntax reference\n\n" +
		"```\n" +
		"<!-- graymatter:context:begin — managed by `graymatter context-sync`; edits inside this block are overwritten -->\n" +
		"- example fact quoted from documentation, never terminated\n" +
		"```\n\n" +
		"An orphaned instructions example:\n\n" +
		"~~~\n" +
		"<!-- graymatter:instructions:begin — dangling example without terminator\n" +
		"~~~\n\n" +
		"A full pair quoted for completeness:\n\n" +
		"```md\n" + validInstructionsBlock() + "```\n\n" +
		"Closing prose paragraph so the document ends normally and realistically.\n"
	writeFile(t, filepath.Join(dir, "AGENTS.md"), content)
	rep, err := AuditPath(dir, auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Check == "markers" {
			t.Errorf("fenced marker documentation produced a markers finding: %+v", f)
		}
	}
}

func TestAudit_StalenessUntrackedReportsReason(t *testing.T) {
	dir, commit := gitInitRepo(t)
	commit("TRACKED.md", "# tracked\n\nCommitted so the repository is real.\n", "")
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "# Untracked guide\n\nNever committed to git at all.\n")
	rep, err := AuditPath(filepath.Join(dir, "AGENTS.md"), auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	st := rep.Files[0].Staleness
	if st == nil {
		t.Fatal("no staleness section")
	}
	if st.Available {
		t.Fatalf("untracked file reported measurable staleness: %+v", st)
	}
	if !strings.Contains(st.Reason, "not tracked") {
		t.Errorf("reason = %q, want it to state the file is not tracked", st.Reason)
	}
}

func TestAudit_StalenessOutsideRepoReportsReason(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "# Guide\n\nPlain directory, no git anywhere above.\n")
	rep, err := AuditPath(filepath.Join(dir, "CLAUDE.md"), auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	st := rep.Files[0].Staleness
	if st == nil {
		t.Fatal("no staleness section")
	}
	if st.Available {
		t.Fatalf("file outside any repository reported measurable staleness: %+v", st)
	}
	if !strings.Contains(st.Reason, "not a git repository") {
		t.Errorf("reason = %q, want %q", st.Reason, "not a git repository")
	}
}

func TestAudit_QuoteMarkersInContextBlockNoFindings(t *testing.T) {
	// A stored fact that quotes the instructions briefing verbatim gets
	// projected into the managed context block. The file is structurally
	// sound; auditing it must produce zero findings.
	dir := t.TempDir()
	facts := []memory.Fact{{
		ID: "f1", AgentID: "demo", Weight: 1,
		CreatedAt: time.Now().UTC(),
		Text:      "Always quote the briefing verbatim: " + validInstructionsBlock() + " when documenting",
	}}
	body := contextblock.RenderBody(facts)
	block := contextblock.RenderBlock(body, contextblock.SyncMeta{
		SHA256:   contextblock.HashBody(body),
		Facts:    len(facts),
		SyncedAt: time.Now().UTC(),
	})
	writeFile(t, filepath.Join(dir, "AGENTS.md"), block)
	rep, err := AuditPath(dir, auditOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Severity == SevWarn || f.Severity == SevFail {
			t.Errorf("healthy file with quoted markers produced %s finding: %+v", f.Severity, f)
		}
	}
}

func validInstructionsBlockBeginOnly() string {
	return strings.SplitN(validInstructionsBlock(), "\n", 2)[0] + "\n"
}

func tamperedContextBlock() string {
	good := contextblock.RenderBody(nil)
	tampered := strings.Replace(good, "## Memory context (GrayMatter)", "## Memory context (GrayMatter) edited by hand", 1)
	return contextblock.RenderBlock(tampered, contextblock.SyncMeta{
		SHA256:   contextblock.HashBody(good),
		SyncedAt: time.Now().UTC(),
	})
}
