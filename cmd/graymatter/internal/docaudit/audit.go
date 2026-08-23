// Package docaudit implements the free auditor behind `doctor --audit`: it
// reads instruction documents (CLAUDE.md / AGENTS.md — any markdown file it is
// pointed at), requires nothing else from the project, and reports what a
// prompt pays for the file and where that content has gone wrong.
//
// Scope discipline, v1: measurement and explicit-marker conflicts only. No
// semantic analysis of any kind — the tool never claims two sentences
// contradict each other, because a public false positive there would cost
// more than every true positive is worth. What it does check, with thresholds
// declared in every report so nobody has to trust the number:
//
//   - cost: approx tokens per prompt, counted by internal/tokens (whitespace
//     words × 1.33) — the SAME approximation the benchmarks and the
//     context-sync budget use, deliberately NOT pkg/memory's tokenize(),
//     which exists for retrieval scoring and undercounts prompt cost by
//     43–56% (retrieval scoring and cost estimation are different jobs)
//   - duplication: near-duplicate paragraphs, word-5-shingle Jaccard >= 0.8
//   - staleness: git-blame line ages, bucketed 30d / 90d / uncommitted
//   - size: line-count alerts at declared thresholds
//   - markers: structural conflicts in managed blocks only — unterminated or
//     duplicated graymatter regions, nesting across kinds, and context-block
//     hashes that no longer verify against their recorded value
package docaudit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/contextblock"
	"github.com/angelnicolasc/graymatter/internal/tokens"
)

// Declared thresholds. They ship in every Report so a reader can audit the
// auditor instead of trusting it.
const (
	SizeWarnLines   = 500  // a prompt carrying this many lines deserves a look
	SizeFailLines   = 1500 // three times that: the file is the product now
	DupThreshold    = 0.8  // conservative Jaccard bar for near-duplicate text
	MedianStaleDays = 90   // median line age past this warns
	ShingleSize     = 5    // words per shingle
)

type Severity string

const (
	SevInfo Severity = "info"
	SevWarn Severity = "warn"
	SevFail Severity = "fail"
)

type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Detail   string   `json:"detail"`
}

type Block struct {
	Kind     string `json:"kind"` // instructions | context
	Begin    int    `json:"begin_line"`
	End      int    `json:"end_line"`
	Verified *bool  `json:"verified,omitempty"` // context blocks only
}

type DuplicatePair struct {
	ALine int     `json:"a_line"`
	BLine int     `json:"b_line"`
	Score float64 `json:"jaccard"`
}

type Staleness struct {
	Available     bool    `json:"available"`
	Reason        string  `json:"reason_if_unavailable,omitempty"`
	Recent        int     `json:"lines_le_30d"`
	Aging         int     `json:"lines_31_90d"`
	Stale         int     `json:"lines_gt_90d"`
	Uncommitted   int     `json:"lines_uncommitted"`
	MedianAgeDays float64 `json:"median_age_days"`
}

type FileReport struct {
	Path       string          `json:"path"`
	Lines      int             `json:"lines"`
	Tokens     int             `json:"tokens_approx"`
	Blocks     []Block         `json:"managed_blocks,omitempty"`
	Duplicates []DuplicatePair `json:"near_duplicates,omitempty"`
	Staleness  *Staleness      `json:"staleness,omitempty"`
}

type Report struct {
	Root       string       `json:"root"`
	Tokenizer  string       `json:"tokenizer"`
	Thresholds []string     `json:"declared_thresholds"`
	Files      []FileReport `json:"files"`
	Findings   []Finding    `json:"findings"`
	FailCount  int          `json:"-"`
	WarnCount  int          `json:"-"`
}

// Options carries the injected clock for reproducible runs.
type Options struct {
	Now time.Time
}

// AuditPath audits root: a directory is scanned for CLAUDE.md / AGENTS.md,
// a file path is audited directly. It never opens a store and never writes;
// a directory with no .graymatter/ audits exactly the same as one with it.
func AuditPath(root string, opts Options) (*Report, error) {
	rep := &Report{
		Root: root,
		Tokenizer: fmt.Sprintf(
			"approx tokens = whitespace words x %.2f (GPT-4-class estimate; shared with benchmarks and context-sync budgets)",
			tokens.PerWord),
		Thresholds: []string{
			fmt.Sprintf("size: warn > %d lines, fail > %d lines", SizeWarnLines, SizeFailLines),
			fmt.Sprintf("duplication: word-%d-shingle Jaccard >= %.2f between paragraphs", ShingleSize, DupThreshold),
			fmt.Sprintf("staleness: median line age > %d days warns", MedianStaleDays),
		},
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}

	var paths []string
	if !info.IsDir() {
		paths = []string{root}
	} else {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			p := filepath.Join(root, name)
			if _, err := os.Stat(p); err == nil {
				paths = append(paths, p)
			}
		}
	}

	for _, p := range paths {
		fr, finds, err := auditFile(p, opts)
		if err != nil {
			return nil, err
		}
		rep.Files = append(rep.Files, fr)
		rep.Findings = append(rep.Findings, finds...)
	}
	for _, f := range rep.Findings {
		switch f.Severity {
		case SevWarn:
			rep.WarnCount++
		case SevFail:
			rep.FailCount++
		}
	}
	return rep, nil
}

func auditFile(path string, opts Options) (FileReport, []Finding, error) {
	rep := FileReport{Path: filepath.ToSlash(path)}
	var finds []Finding

	data, err := os.ReadFile(path)
	if err != nil {
		return rep, nil, err
	}
	content := string(data)
	lines := splitLines(content)
	rep.Lines = len(lines)
	rep.Tokens = tokens.Approx(content)

	// --- managed blocks ---------------------------------------------------
	// Marker scanning runs over a copy with fenced code regions blanked:
	// documents that quote the marker syntax inside ``` fences are healthy,
	// and flagging them would be a public false positive with exit code 1.
	blocks, markerFinds := auditMarkers(path, maskFenced(content))
	rep.Blocks = blocks
	finds = append(finds, markerFinds...)

	// --- duplication ------------------------------------------------------
	pairs := findDuplicates(lines)
	rep.Duplicates = pairs
	for _, d := range pairs {
		finds = append(finds, Finding{Check: "duplicates", Severity: SevWarn, File: path,
			Detail: fmt.Sprintf("near-duplicate paragraphs at lines %d and %d (Jaccard %.2f)", d.ALine, d.BLine, d.Score)})
	}

	// --- size -------------------------------------------------------------
	switch {
	case rep.Lines > SizeFailLines:
		finds = append(finds, Finding{Check: "size", Severity: SevFail, File: path,
			Detail: fmt.Sprintf("%d lines exceeds the %d-line failure threshold", rep.Lines, SizeFailLines)})
	case rep.Lines > SizeWarnLines:
		finds = append(finds, Finding{Check: "size", Severity: SevWarn, File: path,
			Detail: fmt.Sprintf("%d lines exceeds the %d-line warning threshold", rep.Lines, SizeWarnLines)})
	}

	// --- staleness --------------------------------------------------------
	st := stalenessReport(path, lines, opts)
	rep.Staleness = st
	if st.Available && st.MedianAgeDays > MedianStaleDays {
		finds = append(finds, Finding{Check: "staleness", Severity: SevWarn, File: path,
			Detail: fmt.Sprintf("median line age %.0f days exceeds %d days (%.0f%% of lines older than %dd)",
				st.MedianAgeDays, MedianStaleDays, pct(st.Stale+st.Aging, rep.Lines), 30)})
	}
	return rep, finds, nil
}

// --- markers -----------------------------------------------------------------

type markerSpan struct {
	kind  string
	start int // byte offset
	end   int // byte offset, exclusive, past the end marker
}

// auditMarkers inventories every managed region, flags structural conflicts,
// and verifies context-block hashes. Only explicit-marker conflicts are in
// scope; semantic contradiction checking is out of scope by design.
func auditMarkers(path, content string) ([]Block, []Finding) {
	var blocks []Block
	var finds []Finding

	kinds := []struct {
		name   string
		begin  string
		end    string
		verify bool
	}{
		{"instructions", "<!-- graymatter:instructions:begin", "<!-- graymatter:instructions:end -->", false},
		{"context", contextblock.BeginMarker, contextblock.EndMarker, true},
	}

	type rawSpan struct {
		kind  string
		begin int // byte offset just past the begin marker
		end   int // byte offset of end-marker start
	}
	var all []rawSpan

	for _, k := range kinds {
		spans, orphans, unterminated := scanPairs(content, k.begin, k.end)
		if orphans > 0 {
			finds = append(finds, Finding{Check: "markers", Severity: SevFail, File: path,
				Detail: fmt.Sprintf("%d orphaned %q begin marker(s) without an end marker", orphans, k.name)})
		}
		if unterminated {
			finds = append(finds, Finding{Check: "markers", Severity: SevFail, File: path,
				Detail: fmt.Sprintf("an unterminated %q block runs to end of file", k.name)})
		}
		if len(spans) > 1 {
			finds = append(finds, Finding{Check: "markers", Severity: SevWarn, File: path,
				Detail: fmt.Sprintf("%d duplicate %q managed blocks; tools will rewrite the first and leave the rest stale", len(spans), k.name)})
		}
		for i, s := range spans {
			b := Block{Kind: k.name, Begin: byteToLine(content, s[0]), End: byteToLine(content, s[1])}
			if k.verify {
				v := verifyContextBlock(content[s[0]:s[1]])
				b.Verified = &v
				if !v {
					finds = append(finds, Finding{Check: "markers", Severity: SevWarn, File: path,
						Detail: fmt.Sprintf("context block #%d was edited by hand since its last sync (hash mismatch)", i+1)})
				}
			}
			blocks = append(blocks, b)
			all = append(all, rawSpan{kind: k.name, begin: s[0], end: s[1]})
		}
	}

	// Nesting across kinds: one managed region inside another means two tools
	// believe they own overlapping bytes and will fight on rewrite.
	sort.Slice(all, func(i, j int) bool { return all[i].begin < all[j].begin })
	for i := 1; i < len(all); i++ {
		if all[i].begin < all[i-1].end {
			finds = append(finds, Finding{Check: "markers", Severity: SevFail, File: path,
				Detail: fmt.Sprintf("nested managed blocks: %q region sits inside a %q region",
					all[i].kind, all[i-1].kind)})
		}
	}
	return blocks, finds
}

// scanPairs pairs begin/end markers non-greedily and reports how many begins
// were orphaned plus whether any begin ran to EOF without an end.
func scanPairs(content, beginMarker, endMarker string) (spans [][2]int, orphans int, unterminated bool) {
	for off := 0; off < len(content); {
		rel := strings.Index(content[off:], beginMarker)
		if rel < 0 {
			break
		}
		beginStart := off + rel
		after := beginStart + len(beginMarker)

		endRel := strings.Index(content[after:], endMarker)
		if endRel < 0 {
			unterminated = true
			break
		}
		endStart := after + endRel
		endEnd := endStart + len(endMarker)

		if nextRel := strings.Index(content[after:], beginMarker); nextRel >= 0 && after+nextRel < endStart {
			orphans++
			off = after
			continue
		}
		spans = append(spans, [2]int{beginStart, endEnd})
		off = endEnd
	}
	return spans, orphans, unterminated
}

// verifyContextBlock re-checks the recorded SHA-256 of a context block body
// using the same parser context-sync wrote it with.
func verifyContextBlock(inner string) bool {
	_, _, verified, found := contextblock.Parse(contextblock.BeginMarker + inner)
	if !found {
		return false
	}
	return verified
}

func byteToLine(content string, off int) int {
	return 1 + strings.Count(content[:off], "\n")
}

// --- duplication ---------------------------------------------------------------

type paragraph struct {
	startLine int
	shingles  map[string]struct{}
}

// findDuplicates splits the document into blank-line-separated paragraphs and
// reports every pair whose word-shingle Jaccard similarity reaches the
// threshold. Paragraphs too short to produce a single shingle do not
// participate at all: repeated one-liners (headings, bullet labels, fence
// markers) are normal in instruction files, and flagging them would spend the
// tool's credibility on noise.
func findDuplicates(lines []string) []DuplicatePair {
	paras := buildParagraphs(lines)
	var out []DuplicatePair
	for i := 0; i < len(paras); i++ {
		for j := i + 1; j < len(paras); j++ {
			score := jaccard(paras[i], paras[j])
			if score >= DupThreshold {
				out = append(out, DuplicatePair{ALine: paras[i].startLine, BLine: paras[j].startLine, Score: score})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ALine < out[b].ALine })
	return out
}

func buildParagraphs(lines []string) []paragraph {
	var paras []paragraph
	var cur []string
	start := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		p := makeParagraph(start, cur)
		if p != nil {
			paras = append(paras, *p)
		}
		cur = nil
	}
	for idx, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		if len(cur) == 0 {
			start = idx + 1
		}
		cur = append(cur, ln)
	}
	flush()
	return paras
}

func makeParagraph(startLine int, lines []string) *paragraph {
	text := normalizeWords(strings.Join(lines, " "))
	words := strings.Fields(text)
	if len(words) < ShingleSize {
		return nil // too short to shingle: excluded, never flagged
	}
	p := &paragraph{startLine: startLine}
	p.shingles = make(map[string]struct{}, len(words))
	for i := 0; i+ShingleSize <= len(words); i++ {
		p.shingles[strings.Join(words[i:i+ShingleSize], " ")] = struct{}{}
	}
	return p
}

func jaccard(a, b paragraph) float64 {
	inter := 0
	for s := range a.shingles {
		if _, ok := b.shingles[s]; ok {
			inter++
		}
	}
	union := len(a.shingles) + len(b.shingles) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// normalizeWords lowercases and strips punctuation so "Deploy." matches
// "deploy".
func normalizeWords(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			return unicode.ToLower(r)
		default:
			return ' '
		}
	}, s)
}

// --- staleness ---------------------------------------------------------------

func stalenessReport(path string, lines []string, opts Options) *Staleness {
	st := &Staleness{}
	dates, reason, err := blameDates(path)
	// blameDates signals "cannot measure honestly" through a non-empty
	// reason with a nil error (git missing, not a repository, untracked
	// file); a non-nil error means blame itself failed. Both must stop the
	// report from claiming measurable staleness — an empty bucket set would
	// read as "everything is fresh", which is exactly the lie this package
	// exists to prevent.
	if err != nil || reason != "" {
		st.Reason = reason
		if st.Reason == "" {
			st.Reason = "git blame failed"
		}
		return st
	}

	var ages []float64
	for _, t := range dates {
		if t.IsZero() {
			st.Uncommitted++
			continue
		}
		ageDays := opts.Now.Sub(t).Hours() / 24
		ages = append(ages, ageDays)
		switch {
		case ageDays <= 30:
			st.Recent++
		case ageDays <= 90:
			st.Aging++
		default:
			st.Stale++
		}
	}
	st.Available = true
	if len(ages) > 0 {
		sort.Float64s(ages)
		st.MedianAgeDays = ages[len(ages)/2]
	}
	return st
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func splitLines(content string) []string {
	s := strings.ReplaceAll(content, "\r\n", "\n")
	return strings.Split(s, "\n")
}
