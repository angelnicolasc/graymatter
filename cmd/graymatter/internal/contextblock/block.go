// Package contextblock projects the store's live facts into the managed
// context block that `graymatter init` teaches agents to read on every run.
//
// The block is opt-in (`graymatter context-sync`) and additive: nothing in
// pkg/memory changes, Recall never reads this block, and every byte outside
// the markers belongs to the user. The contract is the one the instructions
// block already publishes — edits inside the markers are overwritten, by
// design — with one addition this package implements: a sync header records
// the SHA-256 of the body as last written, so a hand edit is detected and can
// be warned about by `doctor` before the next sync replaces it. Detection
// never blocks; the backup file is what makes overwriting safe.
//
// Everything here is deterministic given a store state. The only wall-clock
// value in the whole block is the `synced=` timestamp in the header comment,
// which no consumer parses for content; golden tests therefore pin the body,
// not the header.
package contextblock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Managed-block markers. Deliberately distinct from the instructions block's
// markers so both blocks can coexist in one file without a scanner ever
// pairing one's begin with the other's end.
const (
	BeginMarker = "<!-- graymatter:context:begin — managed by `graymatter context-sync`; edits inside this block are overwritten -->"
	EndMarker   = "<!-- graymatter:context:end -->"
)

// DefaultBudgetTokens is the projection budget the playbook specifies as the
// 400–512 band default. The whole rendered body — not per fact — must fit it.
const DefaultBudgetTokens = 512

// MinBudgetTokens rejects budgets too small to hold even the heading plus one
// short fact; below this the block would render empty and look broken.
const MinBudgetTokens = 64

// syncPrefix introduces the machine-readable header line inside the block.
const syncPrefix = "<!-- graymatter:context:sync"

// heading is the single fixed line of the body. It costs budget like any
// other line and is part of every golden.
const heading = "## Memory context (GrayMatter)"

// Stats reports what a selection did, for command output and tests.
type Stats struct {
	Considered int // live, non-empty facts ranked before budgeting
	Selected   int // facts that fit the budget
	TokensUsed int // approx tokens of the rendered body
	Skipped    int // ranked but left out: budget exhausted or oversized
}

// Selection ranks live facts by weight (descending), breaks ties oldest-first
// then by ID — the same total order the retrieval contract documents — and
// keeps the prefix of that ranking that fits the token budget. A fact larger
// than what remains is skipped, and smaller ones behind it may still fit:
// rank decides priority, not admission.
func Select(facts []memory.Fact, budgetTokens int) ([]memory.Fact, Stats) {
	var st Stats
	type cand struct {
		f memory.Fact
	}
	cands := make([]cand, 0, len(facts))
	for _, f := range facts {
		if f.IsSuperseded() || strings.TrimSpace(f.Text) == "" {
			continue
		}
		line := bullet(f.Text)
		if strings.TrimSpace(strings.TrimPrefix(line, "- ")) == "" {
			continue // nothing survives sanitation: nothing can be projected
		}
		st.Considered++
		cands = append(cands, cand{f: f})
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].f, cands[j].f
		switch {
		case a.Weight != b.Weight:
			return a.Weight > b.Weight
		case !a.CreatedAt.Equal(b.CreatedAt):
			return a.CreatedAt.Before(b.CreatedAt)
		default:
			return a.ID < b.ID
		}
	})

	var chosen []memory.Fact
	var lines []string
	for _, c := range cands {
		trial := append(append([]string{}, lines...), bullet(c.f.Text))
		if tokens.Approx(renderedBody(trial)) <= budgetTokens {
			chosen = append(chosen, c.f)
			lines = trial
		} else {
			st.Skipped++
		}
	}
	body := RenderBody(chosen)
	st.Selected = len(chosen)
	// The counter is measured off the rendered body, not accumulated per line:
	// Approx truncates, and per-line sums truncate away fractions a single
	// whole-body call keeps, which let an accumulating counter promise a
	// budget the rendered file then broke.
	st.TokensUsed = tokens.Approx(body)
	return chosen, st
}

// renderedBody builds the would-be body for budget measurement.
func renderedBody(lines []string) string {
	return heading + "\n" + strings.Join(lines, "\n")
}

func bullet(text string) string { return "- " + sanitize(text) }

// sanitize strips everything that would let fact text impersonate block
// structure: newlines (one fact, one line — also what keeps diff accounting
// meaningful) and any substring able to close or forge a managed marker.
func sanitize(text string) string {
	out := strings.ReplaceAll(text, "\r\n", " ")
	out = strings.ReplaceAll(out, "\n", " ")
	out = strings.ReplaceAll(out, "\r", " ")
	out = strings.ReplaceAll(out, BeginMarker, "[filtered]")
	out = strings.ReplaceAll(out, EndMarker, "[filtered]")
	out = strings.ReplaceAll(out, syncPrefix, "[filtered]")
	return strings.TrimSpace(out)
}

// RenderBody renders the projected body: heading plus one bullet per selected
// fact, selection order preserved. Pure function of its input — goldens pin
// it, and two calls with equal input return equal bytes.
func RenderBody(facts []memory.Fact) string {
	var b strings.Builder
	b.WriteString(heading)
	b.WriteString("\n")
	for _, f := range facts {
		b.WriteString(bullet(f.Text))
		b.WriteString("\n")
	}
	return b.String()
}

// SyncMeta is the machine-readable state recorded in the block header.
type SyncMeta struct {
	SHA256   string // hex digest of the body exactly as written
	Facts    int
	SyncedAt time.Time
}

// HeaderLine renders the sync comment. Exported for doctor's parser tests.
func (m SyncMeta) HeaderLine() string {
	return fmt.Sprintf("%s v1 sha256=%s facts=%d synced=%s -->",
		syncPrefix, m.SHA256, m.Facts, m.SyncedAt.UTC().Format(time.RFC3339))
}

// RenderBlock assembles begin marker, sync header, body and end marker.
func RenderBlock(body string, meta SyncMeta) string {
	var b strings.Builder
	b.WriteString(BeginMarker)
	b.WriteString("\n")
	b.WriteString(meta.HeaderLine())
	b.WriteString("\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(EndMarker)
	b.WriteString("\n")
	return b.String()
}

// HashBody digests a body for SyncMeta.SHA256.
func HashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// spanScanner returns [start,end) byte ranges of every well-formed context
// block, non-greedy: an orphaned begin marker is skipped rather than paired
// with some later end marker, because the bytes between would be user content
// that was never inside a managed region. Same algorithm the instructions
// scanner uses; duplicated rather than shared because that one lives in
// package main and importing main is impossible.
func spanScanner(content string) [][2]int {
	var spans [][2]int
	for off := 0; off < len(content); {
		rel := strings.Index(content[off:], BeginMarker)
		if rel < 0 {
			break
		}
		begin := off + rel
		after := begin + len(BeginMarker)

		endRel := strings.Index(content[after:], EndMarker)
		if endRel < 0 {
			break
		}
		end := after + endRel + len(EndMarker)

		if nextRel := strings.Index(content[after:], BeginMarker); nextRel >= 0 && after+nextRel < end {
			off = after
			continue
		}
		spans = append(spans, [2]int{begin, end})
		off = end
	}
	return spans
}

// Parse extracts the first well-formed block's metadata and body from file
// content. found=false means the file carries no context block at all;
// found=true with an unparsable or missing header still yields the raw body
// so Verify can report the mismatch instead of silently losing it.
func Parse(content string) (body string, meta SyncMeta, verified, found bool) {
	spans := spanScanner(content)
	if len(spans) == 0 {
		return "", SyncMeta{}, false, false
	}
	// Span ends are exclusive past the END MARKER; the body lives strictly
	// before that marker, so cut at its start, not its end.
	inner := content[spans[0][0]+len(BeginMarker) : spans[0][1]-len(EndMarker)]
	inner = strings.TrimPrefix(inner, "\n")
	// The trailing newline is part of the body and must survive: hashes are
	// taken over the body exactly as written, and trimming here would fail
	// verification on an untouched round trip.

	// First line should be the sync header; whatever follows is the body.
	nl := strings.Index(inner, "\n")
	header := inner
	body = ""
	if nl >= 0 {
		header = inner[:nl]
		body = inner[nl+1:]
	} else {
		body = ""
	}

	meta, ok := parseHeader(header)
	if !ok {
		// No usable header: expose everything as body, unverified.
		return inner, SyncMeta{}, false, true
	}
	verified = HashBody(body) == meta.SHA256
	return body, meta, verified, true
}

func parseHeader(line string) (SyncMeta, bool) {
	if !strings.HasPrefix(line, syncPrefix) || !strings.HasSuffix(line, "-->") {
		return SyncMeta{}, false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(line, syncPrefix), "-->")
	var m SyncMeta
	hasHash := false
	for _, field := range strings.Fields(inner) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "sha256":
			m.SHA256 = kv[1]
			hasHash = true
		case "facts":
			if n, err := strconv.Atoi(kv[1]); err == nil {
				m.Facts = n
			}
		case "synced":
			if t, err := time.Parse(time.RFC3339, kv[1]); err == nil {
				m.SyncedAt = t
			}
		}
	}
	if !hasHash {
		return SyncMeta{}, false
	}
	return m, true
}

// Splice replaces every existing context block with next, inserting where the
// first one stood, or appends after the existing content when the file has
// none yet. Bytes outside the managed regions pass through untouched; the
// caller owns line-ending adaptation and backup, exactly as `init` does.
func Splice(content, next string) string {
	spans := spanScanner(content)
	if len(spans) == 0 {
		if content == "" {
			return next
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return content + "\n" + next
	}
	at := spans[0][0]
	stripped := content
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		stripped = stripped[:s[0]] + stripped[s[1]:]
	}
	return stripped[:at] + next + stripped[at:]
}
