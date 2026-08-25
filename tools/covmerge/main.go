// Command covmerge merges Go coverage profiles into one, so percentages can
// be computed over the union of platforms instead of a single operating
// system. Blocks covered on ANY platform count as covered; counts from
// matching blocks are summed (only ever compared against zero downstream).
//
// Profiles produced on different GOOS describe mostly the same statement
// blocks for shared code, plus blocks unique to that platform's files — which
// is exactly why single-OS numbers undercount cross-platform code: the ACL
// tests only run on Windows, the unix-socket tests only on POSIX.
//
// Usage:
//
//	go run ./tools/covmerge -o merged.out profile1.out profile2.out [...]
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

type block struct {
	stmts int
	count int
}

func main() {
	out := flag.String("o", "merged.out", "output profile path")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "covmerge: at least one input profile is required")
		os.Exit(2)
	}

	merged := map[string]*block{}
	for _, path := range flag.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "covmerge: %v\n", err)
			os.Exit(1)
		}
		first := true
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "mode:") {
				if first {
					first = false // mode declarations may differ (set/atomic); ours is atomic
				}
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 3 {
				fmt.Fprintf(os.Stderr, "covmerge: malformed line in %s: %q\n", path, line)
				os.Exit(1)
			}
			var stmts, count int
			if _, err := fmt.Sscanf(fields[1], "%d", &stmts); err != nil {
				fmt.Fprintf(os.Stderr, "covmerge: %v\n", err)
				os.Exit(1)
			}
			fmt.Sscanf(fields[2], "%d", &count)
			b := merged[fields[0]]
			if b == nil {
				merged[fields[0]] = &block{stmts: stmts, count: count}
				continue
			}
			b.count += count // statements are identical per key; counts accumulate
		}
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("mode: atomic\n")
	for _, k := range keys {
		b := merged[k]
		fmt.Fprintf(&sb, "%s %d %d\n", k, b.stmts, b.count)
	}
	if err := os.WriteFile(*out, []byte(sb.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "covmerge: write %s: %v\n", *out, err)
		os.Exit(1)
	}
}
