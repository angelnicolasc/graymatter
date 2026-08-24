// Command token_count is the documented reproduction path for the token
// figures published in README.md and docs/benchmarks.md:
//
//	go run ./benchmarks/token_count
//
// The measurement itself lives in internal/bench (package bench), shared with
// `graymatter bench` so the installed binary and this command cannot drift
// apart while claiming to measure the same thing. The published tables are
// gated against a live run in main_test.go, which is unchanged by that
// refactor — it still calls runBenchmark here.
//
// What the benchmark does NOT measure is in docs/benchmarks.md and is worth
// reading before quoting a number from it: relevance is never checked, so a
// system returning 8 random facts would score the same reduction, and
// full-history injection is the weakest baseline available.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/angelnicolasc/graymatter/internal/bench"
)

// result aliases the shared row type so the gate test keeps compiling against
// exactly the fields it has always asserted on.
type result = bench.TokenResult

var sessionCounts = bench.SessionCounts

func runBenchmark() ([]result, error) {
	return bench.RunTokenCount()
}

func main() {
	start := time.Now()

	results, err := runBenchmark()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	fmt.Print(bench.RenderTokenReport(results, time.Since(start)))
}
