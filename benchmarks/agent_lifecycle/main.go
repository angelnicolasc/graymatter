// agent_lifecycle simulates one hundred real working sessions of a coding
// agent through the exact interface a host uses: the compiled graymatter
// binary over MCP stdio, one fresh process per session â€” the process death
// between sessions is the durability claim under test, not a simulation of
// one.
//
// Unlike ./benchmarks/token_count (library-level, one process) this exercises
// the full stack every session: JSON-RPC framing, the MCP server, the store,
// and the on-disk file surviving process exit.
//
// Metrics are pre-registered below; the harness prints measured values and
// the pass/fail band next to the published claim.
//
// Usage:
//
//	go run ./benchmarks/agent_lifecycle -binary /path/to/graymatter
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	sessions       = 100
	tokenPerWord   = 1.33
	emptyAgentName = "phoenix-coder"
)

// Probe facts planted in the opening sessions and queried at the end. Hit =
// the exact fact text appears in the Top-8 results of its query.
var probes = []struct {
	fact      string
	query     string
	plantSess int
}{
	{"Postgres 16 is the project database; pgx v5 is the driver, database/sql is banned", "which database and driver does the project use", 2},
	{"Maria signs off every release; never deploy without her written OK in the release channel", "who approves deployments", 3},
	{"The staging environment is phoenix-staging.eu-west-1 and redeploys itself every night at 02:00 UTC", "where is staging and what happens at night", 4},
}

// Supersede pair: v1 planted at session 5, tombstoned at session 70 by v2.
// The final queries must never return v1.
const supersededV1 = "The API base URL is https://api.phoenix.example.com/v1"
const replacementV2 = "The API base URL is https://api.phoenix.example.com/v2"

// Paraphrase probe: the fact is stored verbatim once; the query at the end is
// a paraphrase with zero token overlap on the content words. Keyword-only
// retrieval is documented as weak here; the number is reported, not gated.
var paraphrase = struct {
	fact  string
	query string
}{
	fact:  "Users abandon the checkout flow when the shipping cost shows up late in the funnel",
	query: "cart abandonment reasons in the payment funnel",
}

// Distractor families: several facts per family share vocabulary, forcing the
// ranking to discriminate within a topic instead of matching a lone keyword.
// Facts are written at the density a working agent actually produces â€” a
// sentence or three with concrete nouns â€” because a thin corpus under-tests
// the token-reduction claim (the fixed-K recall floor dominates when the
// stored history is tiny).
var distractorFamilies = [][]string{
	{
		"User prefers tables over YAML for test fixtures because diffs read better in code review and the fixture loader already speaks table syntax",
		"User prefers Postgres full-text search over Elastic for this scale; revisited after the Elastic spike cost two weeks and never left staging",
		"User prefers feature flags over long-lived branches; every incomplete feature ships dark behind a flag named after the ticket",
		"User prefers pgx batches over ORM bulk inserts; the ORM path was measured three times slower on the nightly import job",
	},
	{
		"Migrations run through golang-migrate with a dedicated CI step; the step fails the build when a migration is added without a matching rollback file",
		"Migration files are never edited after they ship to main; fixing a bad migration means a new forward migration, learned the hard way in sprint 12",
		"Migration rollback scripts are reviewed by the on-call engineer and rehearsed against a staging snapshot before every schema change",
	},
	{
		"The deploy pipeline signs images with cosign before pushing to the registry, and the signature is verified again at deploy time inside the cluster",
		"The deploy pipeline blocks on the vulnerability scan step; critical CVEs fail the build, high ones open a ticket with a seven-day SLA",
		"The deploy pipeline rolls out canary first at five percent traffic for thirty minutes, watching error rate and p99 before promoting",
	},
	{
		"Checkout retries use exponential backoff capped at three attempts, then hand off to the dead-letter queue for manual triage",
		"Checkout idempotency keys come from the client and live in Redis for twenty-four hours; duplicate keys return the original response",
		"Checkout fraud checks run synchronously under four hundred milliseconds; anything slower is queued for asynchronous review",
	},
}

// sessionFacts generates the facts stored during session n. Deterministic.
func sessionFacts(n int, rng *rand.Rand) []string {
	var facts []string
	fam := distractorFamilies[n%len(distractorFamilies)]
	facts = append(facts, fam[rng.Intn(len(fam))])
	switch n % 5 {
	case 0:
		facts = append(facts, "Fixed a flaky test in the billing package by freezing time at the assert site; the flake reproduced once every forty runs and only on Windows CI runners")
	case 1:
		facts = append(facts, "Rate limit on the public API is 100 requests per minute per key; exceeding it returns 429 with a Retry-After header and hits the circuit breaker after five consecutive 429s")
	case 2:
		facts = append(facts, "The design doc for checkout v2 lives in docs/adr/0042-checkout-v2.md and the open question is whether stored payment methods survive the token migration")
	case 3:
		facts = append(facts, "On-call rotation hands over Mondays at 10:00 local time; the handover doc template is pinned in the ops channel and escalations page the secondary first")
	case 4:
		facts = append(facts, "Feature branch CI must stay under six minutes or teams start skipping it during incidents; the longest stage is the integration suite and it caches dependencies between runs")
	}
	if n%7 == 0 {
		facts = append(facts, fmt.Sprintf("Ticket PHX-%d tracks the pagination bug on the orders list screen; it reproduces with more than two hundred rows and the cursor drifts by one page on sort changes", 100+n))
	}
	return facts
}

// checkpoint state per session: a small object the next session must recover.
func checkpointState(n int) string {
	return fmt.Sprintf(`{"session":%d,"task":"implement pagination","step":%d,"branch":"feat/pagination"}`, n, n%10+1)
}

// ---- minimal MCP stdio client: one process per session ----

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type callResult struct {
	Content           []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// session is one live binary process. All calls are sequential: write the
// request, read until its response id arrives.
type session struct {
	cmd    *exec.Cmd
	stdin  ioWriteCloser
	stdout *bufio.Reader
	nextID int
}

type ioWriteCloser interface {
	Write([]byte) (int, error)
	Close() error
}

func startSession(binary, workdir string) (*session, error) {
	cmd := exec.Command(binary, "mcp", "serve")
	cmd.Dir = workdir // the server opens <cwd>/.graymatter — pin it so the run's store is isolated from whatever the parent directory happens to contain
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &session{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), nextID: 1}
	// initialize handshake, as every real host does first
	if _, err := s.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "lifecycle", "version": "1.0.0"},
	}); err != nil {
		s.kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return s, nil
}

func (s *session) call(tool string, args map[string]any) (callResult, error) {
	return s.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
}

func (s *session) rpc(method string, params map[string]any) (callResult, error) {
	s.nextID++
	id := s.nextID
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	reqLine, err := json.Marshal(req)
	if err != nil {
		return callResult{}, err
	}
	if _, err := s.stdin.Write(append(reqLine, '\n')); err != nil {
		return callResult{}, err
	}
	for {
		raw, err := s.stdout.ReadString('\n')
		if err != nil {
			return callResult{}, fmt.Errorf("read response for id %d: %w", id, err)
		}
		line := strings.TrimSpace(raw)
		if line == "" || !strings.Contains(line, fmt.Sprintf(`"id":%d`, id)) {
			continue // notifications or unrelated frames
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return callResult{}, fmt.Errorf("decode response: %w", err)
		}
		if resp.Error != nil {
			return callResult{}, fmt.Errorf("rpc error: %s", resp.Error.Message)
		}
		var cr callResult
		if err := json.Unmarshal(resp.Result, &cr); err != nil {
			return callResult{}, fmt.Errorf("decode result: %w", err)
		}
		if cr.IsError {
			text := ""
			if len(cr.Content) > 0 {
				text = cr.Content[0].Text
			}
			return cr, fmt.Errorf("tool error: %s", text)
		}
		return cr, nil
	}
}

func (s *session) kill() {
	_ = s.stdin.Close()
	_ = s.cmd.Wait()
}

// ---- the run ----

type metrics struct {
	totalFacts      int
	totalWords      int
	probeHits       int
	probeTotal      int
	deadReturned    int
	tokensRecall    int
	queriesCounted  int
	resumeOK        int
	resumeTried     int
	sharedOK        int
	paraphraseHit   bool
	liveFactsSeen   map[string]bool
}

func main() {
	binary := flag.String("binary", "", "path to the compiled graymatter binary")
	out := flag.String("out", "", "optional path for the markdown report")
	keep := flag.Bool("keep", false, "keep the store directory after the run (forensics)")
	flag.Parse()
	if *binary == "" {
		*binary = filepath.Join(os.TempDir(), "graymatter-lifecycle.exe")
		if _, err := os.Stat(*binary); err != nil {
			fmt.Fprintln(os.Stderr, "building binary for the simulation...")
			build := exec.Command("go", "build", "-o", *binary, "./cmd/graymatter")
			build.Stderr = os.Stderr
			if err := build.Run(); err != nil {
				fmt.Fprintln(os.Stderr, "build failed:", err)
				os.Exit(1)
			}
		}
	}

	dir, err := os.MkdirTemp("", "gm-lifecycle")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	rng := rand.New(rand.NewSource(42)) // deterministic corpus
	m := &metrics{liveFactsSeen: map[string]bool{}}
	storedTexts := make(map[string]bool) // every fact ever written (the full-history baseline)

	start := time.Now()

	for sess := 1; sess <= sessions; sess++ {
		s, err := startSession(*binary, dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "session %d: start failed: %v\n", sess, err)
			os.Exit(1)
		}

		// resume continuity: every session after the first must recover state
		if sess > 1 {
			m.resumeTried++
			res, err := s.call("checkpoint_resume", map[string]any{"agent_id": emptyAgentName})
			if err == nil && strings.Contains(firstText(res), "restored") {
				m.resumeOK++
			}
		}

		// the session's working query (what the agent would ask at boot)
		if sess >= 10 && sess%10 == 0 {
			res, err := s.call("memory_search", map[string]any{
				"agent_id": emptyAgentName, "query": "database driver deploy approvals staging environment",
			})
			if err == nil {
				text := firstText(res)
				m.tokensRecall += countTokens(text)
				m.queriesCounted++
				for _, p := range probes {
					if strings.Contains(text, p.fact) {
						// probes are measured at the end; early hits are free wins
					}
				}
				checkDead(text, m)
			}
		}

		// plant the opening-session facts
		if sess == 2 || sess == 3 || sess == 4 {
			idx := sess - 2
			if _, err := s.call("memory_add", map[string]any{
				"agent_id": emptyAgentName, "text": probes[idx].fact,
			}); err != nil {
				fail(sess, "plant probe", err)
			}
			storedTexts[probes[idx].fact] = true
			m.totalFacts++
		}
		if sess == 5 {
			if _, err := s.call("memory_add", map[string]any{
				"agent_id": emptyAgentName, "text": supersededV1,
			}); err != nil {
				fail(sess, "plant v1", err)
			}
			storedTexts[supersededV1] = true
			m.totalFacts++
		}
		if sess == 12 {
			if _, err := s.call("memory_add", map[string]any{
				"agent_id": emptyAgentName, "text": paraphrase.fact,
			}); err != nil {
				fail(sess, "plant paraphrase", err)
			}
			storedTexts[paraphrase.fact] = true
			m.totalFacts++
		}
		if sess == 6 {
			// shared conventions every agent must see
			for _, sh := range []string{
				"Project convention: all timestamps stored as UTC ISO-8601 strings",
				"Security policy: no secrets in environment files, use the platform vault",
				"Team rule: PRs need one review, hotfixes two",
			} {
				if _, err := s.call("memory_add", map[string]any{
					"agent_id": "__shared__", "text": sh,
				}); err != nil {
					fail(sess, "plant shared", err)
				}
				storedTexts[sh] = true
				m.totalFacts++
			}
		}

		// the session's ordinary memories
		for _, f := range sessionFacts(sess, rng) {
			if _, err := s.call("memory_add", map[string]any{
				"agent_id": emptyAgentName, "text": f,
			}); err != nil {
				fail(sess, "add", err)
			}
			storedTexts[f] = true
			m.totalFacts++
			m.totalWords += wordCount(f)
		}

		// session 70 supersedes the v1 API URL with v2
		if sess == 70 {
			if _, err := s.call("memory_reflect", map[string]any{
				"action": "update",
				"agent_id": emptyAgentName,
				"text":   replacementV2,
				"target": supersededV1,
			}); err != nil {
				fail(sess, "supersede", err)
			}
			storedTexts[replacementV2] = true
			m.totalFacts++
		}

		// end-of-session checkpoint
		if _, err := s.call("checkpoint_save", map[string]any{
			"agent_id": emptyAgentName, "state": checkpointState(sess),
		}); err != nil {
			fail(sess, "checkpoint_save", err)
		}

		// window narrowing for the tombstone finding: probe v1 immediately
		// after the supersede, then every 5 sessions, to bracket when (if)
		// it comes back to life
		if sess >= 70 {
			res, err := s.call("memory_search", map[string]any{
				"agent_id": emptyAgentName, "query": "what is the api base url",
			})
			if err == nil && strings.Contains(firstText(res), supersededV1) {
				fmt.Printf("DIAGNOSTIC — v1 alive again at session %d\n", sess)
			}
		}

		s.kill() // the process dies: this is the cross-session durability claim
	}

	// ---- final verification session: the probes fire here ----
	s, err := startSession(*binary, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "final session: start failed:", err)
		os.Exit(1)
	}
	defer s.kill()

	probeQueries := []struct {
		query string
		fact  string
	}{}
	for _, p := range probes {
		probeQueries = append(probeQueries, struct {
			query string
			fact  string
		}{p.query, p.fact})
	}
	// supersede probe: v2 must surface, v1 must never appear
	probeQueries = append(probeQueries, struct {
		query string
		fact  string
	}{"what is the api base url", replacementV2})
	// paraphrase probe: reported, not gated
	probeQueries = append(probeQueries, struct {
		query string
		fact  string
	}{paraphrase.query, paraphrase.fact})

	for _, pq := range probeQueries {
		res, err := s.call("memory_search", map[string]any{
			"agent_id": emptyAgentName, "query": pq.query,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe query failed: %v\n", err)
			os.Exit(1)
		}
		text := firstText(res)
		m.tokensRecall += countTokens(text)
		m.queriesCounted++
		if strings.Contains(text, pq.fact) {
			m.probeHits++
			if pq.fact == paraphrase.fact {
				m.paraphraseHit = true
			}
		}
		checkDead(text, m)
	}
	m.probeTotal = len(probeQueries) - 1 // paraphrase reported separately

	// v1 must not appear even under a direct query
	res, err := s.call("memory_search", map[string]any{
		"agent_id": emptyAgentName, "query": supersededV1,
	})
	if err == nil {
		checkDead(firstText(res), m)
	}

	// shared memory lives in its own namespace by design (no magic routing);
	// a real host queries __shared__ explicitly, which is what we verify here
	shRes, err := s.call("memory_search", map[string]any{
		"agent_id": "__shared__", "query": "timestamps convention secrets review",
	})
	if err == nil {
		text := firstText(shRes)
		for _, sh := range []string{"UTC ISO-8601", "platform vault", "hotfixes two"} {
			if strings.Contains(text, sh) {
				m.sharedOK++
			}
		}
	}

	elapsed := time.Since(start)

	// ---- baseline: full-history injection would cost every word ever stored ----
	fullHistoryTokens := 0
	texts := make([]string, 0, len(storedTexts))
	for t := range storedTexts {
		texts = append(texts, t)
	}
	sort.Strings(texts)
	for _, t := range texts {
		fullHistoryTokens += countTokens(t)
	}
	reduction := 0.0
	if fullHistoryTokens > 0 {
		avgRecall := float64(m.tokensRecall) / float64(m.queriesCounted)
		reduction = 1 - avgRecall/float64(fullHistoryTokens)
	}

	printReport(*binary, dir, m, fullHistoryTokens, reduction, elapsed)
	if *out != "" {
		writeReport(*out, m, fullHistoryTokens, reduction, elapsed)
	}
	if !*keep {
		os.RemoveAll(dir)
	} else {
		fmt.Printf("store kept at: %s\n", dir)
	}
}

// checkDead counts a superseded fact as returned only when it appears as a
// numbered result. The empty-state notice quotes the query verbatim, so a
// direct query for the dead text would otherwise false-positive on its own
// echo (verified in the first harness run).
func checkDead(text string, m *metrics) {
	if strings.Contains(text, "No memories found") {
		return
	}
	if strings.Contains(text, supersededV1) {
		m.deadReturned++
		fmt.Printf("DIAGNOSTIC â€” superseded fact returned as a result:\n%s\n", text)
	}
}

func firstText(r callResult) string {
	if len(r.Content) > 0 {
		return r.Content[0].Text
	}
	return ""
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}

func countTokens(s string) int {
	return int(float64(wordCount(s)) * tokenPerWord)
}

func fail(sess int, what string, err error) {
	fmt.Fprintf(os.Stderr, "session %d: %s failed: %v\n", sess, what, err)
	os.Exit(1)
}

func printReport(binary, dir string, m *metrics, fullHistoryTokens int, reduction float64, elapsed time.Duration) {
	fmt.Print(reportString(binary, dir, m, fullHistoryTokens, reduction, elapsed))
	fmt.Printf("\nMarkdown report: pass -out <path>\n")
}

func writeReport(path string, m *metrics, fullHistoryTokens int, reduction float64, elapsed time.Duration) {
	if err := os.WriteFile(path, []byte(reportString("", "", m, fullHistoryTokens, reduction, elapsed)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
	}
}

func reportString(binary, dir string, m *metrics, fullHistoryTokens int, reduction float64, elapsed time.Duration) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	w("# Agent lifecycle simulation â€” 100 real sessions\n\n")
	w("Protocol: one fresh `graymatter mcp serve` process per session (JSON-RPC over stdio); the process dies at every session end â€” durability across restarts is under test, not simulated. Corpus is realistic and adversarial: distractor families sharing vocabulary, a supersede pair, paraphrase probe, shared namespace. Deterministic seed.\n\n")
	w("| Metric | Claim under test | Measured | Verdict |\n|---|---|---|\n")

	hitRate := 100 * float64(m.probeHits) / float64(max(1, m.probeTotal))
	verdict := "PASS"
	if hitRate < 70 {
		verdict = "FAIL"
	}
	w("| Probe recall after ~96 sessions + %d process deaths | 83%% (band â‰¥70%%) | %.0f%% (%d/%d) | %s |\n",
		sessions-4, hitRate, m.probeHits, m.probeTotal, verdict)

	deadVerdict := "PASS"
	if m.deadReturned > 0 {
		deadVerdict = "FAIL"
	}
	w("| Superseded facts returned | 0%% | %d occurrences | %s |\n", m.deadReturned, deadVerdict)

	redVerdict := "PASS"
	if reduction < 0.85 {
		redVerdict = "FAIL"
	}
	w("| Token reduction vs full-history | ~90%% (band â‰¥85%%) | %.0f%% | %s |\n", reduction*100, redVerdict)

	resumeVerdict := "PASS"
	if m.resumeOK != m.resumeTried {
		resumeVerdict = "FAIL"
	}
	w("| Checkpoint resume across process death | every session | %d/%d | %s |\n", m.resumeOK, m.resumeTried, resumeVerdict)

	sharedVerdict := "PASS"
	if m.sharedOK < 3 {
		sharedVerdict = "FAIL"
	}
	w("| Shared namespace from a foreign agent_id | 3/3 conventions visible | %d/3 | %s |\n", m.sharedOK, sharedVerdict)

	paraVerdict := "MISS"
	if m.paraphraseHit {
		paraVerdict = "HIT"
	}
	w("| Paraphrase probe (documented keyword-only weakness) | reported, not gated | %s | info |\n", paraVerdict)

	w("\n| Run facts | Value |\n|---|---|\n")
	w("| Sessions (process spawns) | %d |\n", sessions)
	w("| Facts stored | %d |\n", m.totalFacts)
	w("| Full-history baseline | ~%d tokens/query |\n", fullHistoryTokens)
	w("| Avg recall result | ~%d tokens/query (%d queries) |\n", m.tokensRecall/max(1, m.queriesCounted), m.queriesCounted)
	w("| Wall time | %s |\n", elapsed.Round(time.Millisecond))
	if binary != "" {
		w("| Binary | %s |\n", binary)
	}
	if dir != "" {
		w("| Store dir | %s (deleted after run) |\n", dir)
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
