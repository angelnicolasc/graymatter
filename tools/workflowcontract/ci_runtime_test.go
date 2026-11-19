package workflowcontract

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Scope checks to YAML job/step indentation, so an unrelated job or comment
// cannot accidentally satisfy a contract for the moved correctness suites.
func ciJob(body, name string) string {
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:\n`).FindStringIndex(body)
	if start == nil {
		return ""
	}
	body = body[start[1]:]
	if next := regexp.MustCompile(`(?m)^  [a-zA-Z0-9_-]+:\n`).FindStringIndex(body); next != nil {
		body = body[:next[0]]
	}
	return body
}

func ciStep(job, name string) string {
	start := regexp.MustCompile(`(?m)^      - name: ` + regexp.QuoteMeta(name) + `\n`).FindStringIndex(job)
	if start == nil {
		return ""
	}
	job = job[start[1]:]
	if next := regexp.MustCompile(`(?m)^      - `).FindStringIndex(job); next != nil {
		job = job[:next[0]]
	}
	return job
}

func parallelCIErrors(body string) []string {
	var errs []string
	require := func(scope, fragment string) {
		if !strings.Contains(scope, fragment) {
			errs = append(errs, fmt.Sprintf("missing %q", fragment))
		}
	}
	forbid := func(scope, fragment string) {
		if strings.Contains(scope, fragment) {
			errs = append(errs, fmt.Sprintf("unexpected %q", fragment))
		}
	}
	testJob, benchmarkJob := ciJob(body, "test"), ciJob(body, "benchmark-tests")
	for _, job := range []string{testJob, benchmarkJob} {
		header := strings.SplitN(job, "    steps:\n", 2)[0]
		for _, fragment := range []string{
			"runs-on: ${{ matrix.os }}", "fail-fast: false",
			"os: [ubuntu-latest, macos-latest, windows-latest]", `go: ["1.25", "1.26.11"]`,
		} {
			require(header, fragment)
		}
		for _, fragment := range []string{"needs:", "if:", "continue-on-error:", "exclude:", "include:"} {
			forbid(header, fragment)
		}
	}
	const benchmarkCommand = "go test -race -count=1 -timeout=1200s ./benchmarks/token_count/... ./benchmarks/retrieval_quality/... ./benchmarks/revision_currency/..."
	benchmarkStep := ciStep(benchmarkJob, "Test benchmark packages")
	require(benchmarkStep, "run: "+benchmarkCommand)
	for _, fragment := range []string{"continue-on-error:", "if:", "-short", "|| true"} {
		forbid(benchmarkJob, fragment)
	}
	if strings.Count(body, benchmarkCommand) != 1 {
		errs = append(errs, "benchmark command must run exactly once per matrix entry")
	}
	require(ciStep(benchmarkJob, "Source-replace the library into the CLI module"), "working-directory: cmd/graymatter\n        run: go mod edit -replace github.com/angelnicolasc/graymatter=../..")
	for name, command := range map[string]string{
		"Test core library with coverage": "go test -race -count=1 -timeout=600s -coverprofile=coverage-core.out -covermode=atomic ./pkg/memory/...",
		"Test CLI module with coverage":   "go test -race -count=1 -timeout=300s -coverprofile=../../coverage-cli.out -covermode=atomic ./internal/harness/... ./internal/kg/... ./internal/server/... ./internal/plugin/... ./internal/mcp/... ./internal/session/... ./internal/daemon/... ./internal/httpauth/... ./internal/hookpacket/... ./internal/usage/...",
		"Test root package":               "go test -race -count=1 -timeout=300s .",
		"Test hook benchmark validation":  "go test -race -short -count=1 ./benchmarks/hook_latency/",
	} {
		step := ciStep(testJob, name)
		require(step, "run: "+command)
		for _, fragment := range []string{"continue-on-error:", "if:", "|| true"} {
			forbid(step, fragment)
		}
	}
	require(ciStep(testJob, "Coverage gate — core library (≥70%)"), "$TOTAL < 70")
	require(ciStep(testJob, "Coverage gate — CLI module (≥65%)"), "$TOTAL < 65")
	require(ciStep(ciJob(body, "coverage-union"), "Coverage gate — core union (≥82%)"), "$TOTAL < 82")
	require(ciStep(ciJob(body, "coverage-union"), "Coverage gate — CLI union (≥72%, ratcheted after phase-2 tests)"), "$TOTAL < 72")
	require(ciJob(body, "coverage-union"), "    needs: [test, benchmark-tests]\n")
	require(ciJob(body, "bench"), "    needs: [test, benchmark-tests, coverage-union, version-consistency, vulncheck]\n")

	timingStep := ciStep(testJob, "Test CLI entrypoint package")
	for _, fragment := range []string{
		"working-directory: cmd/graymatter", "shell: bash", "set -euo pipefail",
		"go test -json -race -count=1 -timeout=600s . |", "tee ../../cli-tests.json |",
		`go run ../../tools/testtiming -summary "$GITHUB_STEP_SUMMARY"`,
	} {
		require(timingStep, fragment)
	}
	for _, fragment := range []string{"continue-on-error:", "if:", "|| true", "set +e"} {
		forbid(timingStep, fragment)
	}
	upload := ciStep(testJob, "Upload CLI test timings")
	for _, fragment := range []string{"if: always()", "uses: actions/upload-artifact@", "name: cli-test-timings-${{ matrix.os }}-go${{ matrix.go }}", "path: cli-tests.json"} {
		require(upload, fragment)
	}
	return errs
}

func TestParallelCorrectnessAndCLITimings(t *testing.T) {
	for _, err := range parallelCIErrors(read(t, "ci")) {
		t.Error(err)
	}
}

func TestParallelCIContractRejectsLostGates(t *testing.T) {
	body := read(t, "ci")
	for name, mutation := range map[string][2]string{
		"lost internal package":    {"./internal/daemon/...", ""},
		"lost usage coverage":      {"./internal/usage/...", ""},
		"weaker coverage":          {"$TOTAL < 70", "$TOTAL < 60"},
		"lost package":             {"./benchmarks/revision_currency/...", ""},
		"lost race detector":       {"go test -race -count=1 -timeout=1200s", "go test -count=1 -timeout=1200s"},
		"missing matrix OS":        {"os: [ubuntu-latest, macos-latest, windows-latest]", "os: [ubuntu-latest, macos-latest]"},
		"nonblocking benchmarks":   {"run: go test -race -count=1 -timeout=1200s", "continue-on-error: true\n        run: go test -race -count=1 -timeout=1200s"},
		"serial benchmarks":        {"  benchmark-tests:\n", "  benchmark-tests:\n    needs: test\n"},
		"detached gate":            {"needs: [test, benchmark-tests]\n", "needs: test\n"},
		"masked test exit":         {"set -euo pipefail", "set -eu"},
		"missing failure artifact": {"      - name: Upload CLI test timings\n        if: always()", "      - name: Upload CLI test timings"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := strings.Replace(body, mutation[0], mutation[1], 1)
			if changed == body || len(parallelCIErrors(changed)) == 0 {
				t.Fatal("mutation did not fail the workflow contract")
			}
		})
	}
}
