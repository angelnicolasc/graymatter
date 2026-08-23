// extraction_precision measures the regex entity extractor against a
// hand-labeled corpus, per the ADR-003 reversal conditions: nothing wires the
// graph until extraction proves itself here.
//
// Zero network, zero LLM, deterministic. The gate is precision >= 0.70 at the
// ID level; type fidelity and recall are reported alongside because a graph
// can pass the gate and still be useless if every organization is typed as a
// person.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

type goldEntity struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

type goldFact struct {
	ID   string       `json:"id"`
	Text string       `json:"text"`
	Gold []goldEntity `json:"gold"`
}

func main() {
	goldPath, ok := resolveGoldPath()
	if !ok {
		fmt.Fprintln(os.Stderr, "corpus not found: benchmarks/fixtures/extraction-gold-v1.jsonl (run from the repository root)")
		os.Exit(1)
	}

	raw, err := os.ReadFile(goldPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", goldPath, err)
		os.Exit(1)
	}

	var facts []goldFact
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var gf goldFact
		if err := json.Unmarshal([]byte(line), &gf); err != nil {
			fmt.Fprintf(os.Stderr, "%s:%d: %v\n", goldPath, i+1, err)
			os.Exit(1)
		}
		facts = append(facts, gf)
	}
	if len(facts) < 100 {
		fmt.Fprintf(os.Stderr, "corpus too small: %d facts (need >= 100)\n", len(facts))
		os.Exit(1)
	}

	extractor := newBenchExtractor()

	var (
		tpID, fpID, fnID int
		tpStrict         int
		fps              []fpSample
		perType          = map[string]*typeStats{}
	)
	for _, f := range facts {
		scoreFact(extractor, f, &tpID, &fpID, &fnID, &tpStrict, perType, &fps)
	}
	precision := ratio(tpID, tpID+fpID)
	recall := ratio(tpID, tpID+fnID)
	f1 := ratio2(2*precision*recall, precision+recall)
	strictP := ratio(tpStrict, tpID)

	printReport(report{
		Facts: len(facts), TP: tpID, FP: fpID, FN: fnID,
		Precision: precision, Recall: recall, F1: f1,
		StrictPrecision: strictP, PerType: sortedTypes(perType),
		FPs: fps,
	})
	const gate = 0.70
	if precision >= gate {
		fmt.Printf("\nGATE: PASS â€” precision %.3f >= %.2f\n", precision, gate)
		return
	}
	fmt.Printf("\nGATE: FAIL â€” precision %.3f < %.2f (do not wire; improve extractor first)\n", precision, gate)
	os.Exit(2)
}

// --- testable seams ----------------------------------------------------------

func newBenchExtractor() kg.EntityExtractor {
	return kg.NewExtractor(kg.ExtractorConfig{UseLLM: false})
}

func defaultGoldPath() string { return "benchmarks/fixtures/extraction-gold-v1.jsonl" }

// resolveGoldPath finds the corpus walking up from the working directory, so
// `go run ./cmd/graymatter/extractionbench` works from the repo root while
// package-local tests still locate it from cmd/graymatter/....
func resolveGoldPath() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, defaultGoldPath())
		if _, statErr := os.Stat(p); statErr == nil {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// scoreFact runs the extractor over one labeled fact and folds the outcome
// into the running counters: ID-level TP/FP/FN for the gate, strict
// (ID+type-correct) hits, per-type fidelity stats, and a capped FP sample.
func scoreFact(ex kg.EntityExtractor, f goldFact, tpID, fpID, fnID, tpStrict *int, perType map[string]*typeStats, fps *[]fpSample) {
	nodes, _, err := ex.Extract(f.Text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: extract: %v\n", f.ID, err)
		os.Exit(1)
	}

	goldSet := map[string]goldEntity{}
	for _, g := range f.Gold {
		goldSet[strings.ToLower(g.ID)] = g
	}
	exSet := map[string]string{}
	for _, n := range nodes {
		exSet[strings.ToLower(n.ID)] = n.EntityType
	}

	for id, exType := range exSet {
		g, ok := goldSet[id]
		if !ok {
			*fpID++
			if len(*fps) < 15 {
				*fps = append(*fps, fpSample{Fact: f.ID, ID: id, Type: exType})
			}
			continue
		}
		*tpID++
		ts := typeStatFor(perType, g.Type)
		ts.gold++
		if exType == g.Type {
			*tpStrict++
			ts.typed++
		} else {
			ts.typeMismatch++
		}
	}
	for _, g := range f.Gold {
		if _, ok := exSet[strings.ToLower(g.ID)]; !ok {
			*fnID++
			ts := typeStatFor(perType, g.Type)
			ts.missed++
		}
	}
}

// --- reporting ---------------------------------------------------------------

type fpSample struct{ Fact, ID, Type string }

type typeStats struct {
	gold, typed, typeMismatch, missed int
}

func typeStatFor(m map[string]*typeStats, t string) *typeStats {
	if m[t] == nil {
		m[t] = &typeStats{}
	}
	return m[t]
}

type report struct {
	Facts           int
	TP, FP, FN      int
	Precision       float64
	Recall          float64
	F1              float64
	StrictPrecision float64
	PerType         []typedRow
	FPs             []fpSample
}

type typedRow struct {
	Name                    string
	Gold, TypedOK, Mismatch int
	Missed                  int
}

func printReport(r report) {
	fmt.Println("Extraction precision â€” regex extractor vs hand-labeled corpus")
	fmt.Println("Matcher: canonical-ID exact match (lowercased); type reported separately")
	fmt.Printf("Corpus: %d labeled facts\n\n", r.Facts)
	fmt.Printf("ID-level:   TP %d   FP %d   FN %d\n", r.TP, r.FP, r.FN)
	fmt.Printf("Precision   %.3f\n", r.Precision)
	fmt.Printf("Recall      %.3f\n", r.Recall)
	fmt.Printf("F1          %.3f\n", r.F1)
	fmt.Printf("Strict precision (ID + type correct): %.3f\n\n", r.StrictPrecision)

	fmt.Println("Per gold type (coverage and typing fidelity):")
	fmt.Println("  type            gold  typed-ok  type-wrong  missed")
	for _, row := range r.PerType {
		fmt.Printf("  %-14s %5d %9d %11d %7d\n", row.Name, row.Gold, row.TypedOK, row.Mismatch, row.Missed)
	}
	fmt.Println("\nFirst false positives (up to 15):")
	for _, fp := range r.FPs {
		fmt.Printf("  [%s] %q (typed %s)\n", fp.Fact, fp.ID, fp.Type)
	}
	if len(r.FPs) == 0 {
		fmt.Println("  none")
	}
}

func sortedTypes(m map[string]*typeStats) []typedRow {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]typedRow, 0, len(keys))
	for _, k := range keys {
		s := m[k]
		out = append(out, typedRow{Name: k, Gold: s.gold, TypedOK: s.typed, Mismatch: s.typeMismatch, Missed: s.missed})
	}
	return out
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func ratio2(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return num / den
}
