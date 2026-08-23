package main

import (
	"strings"
	"testing"
)

const sampleTable = `| Sessions | Full injection | GrayMatter | Reduction |
|----------|---------------|------------|-----------|
| 1        | ~80 tokens    | ~80 tokens  | 0% |
| 10       | ~630 tokens   | ~550 tokens | 12% |
| 100      | ~6,960 tokens | ~670 tokens | **90%** |
`

func TestParseTable(t *testing.T) {
	rows, err := parseTable(sampleTable)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("parsed %d rows, want 3", len(rows))
	}
	last := rows[2]
	if last.sessions != 100 || last.full != 6960 || last.gm != 670 || last.pct != 90 {
		t.Errorf("last row misparsed: %+v", last)
	}
}

func TestRender_DeterministicAndFaithful(t *testing.T) {
	rows, err := parseTable(sampleTable)
	if err != nil {
		t.Fatal(err)
	}
	a, b := render(rows), render(rows)
	if a != b {
		t.Fatal("render is not deterministic")
	}
	for _, want := range []string{"670", "(−90%)", "1 session", "docs/benchmarks.md"} {
		if !strings.Contains(a, want) {
			t.Errorf("rendered chart missing %q", want)
		}
	}
}

func TestParseTable_RejectsEmpty(t *testing.T) {
	if _, err := parseTable("# no table here\n"); err == nil {
		t.Fatal("expected error for input without benchmark rows")
	}
}
