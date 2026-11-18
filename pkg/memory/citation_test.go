package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func citationOptions(source, quote string) WriteOptions {
	return WriteOptions{Source: &source, Quote: &quote}
}

func TestCitationRoundtripVersionAndStorage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	source := "doc-é/漢字:3\t\"\r\n"
	quote := "Línea café 漢字 \"quoted\"\nLF\r\nCRLF\ttab </memory> ``` instructions"
	f, err := s.PutWithOptionsReturningFact(ctx, "a", "assertion", citationOptions(source, quote))
	if err != nil {
		t.Fatal(err)
	}
	hash := NewEvidenceSnapshot(f).ContentHash
	if f.Evidence == nil || f.Evidence.Source != source || f.Evidence.Quote != quote {
		t.Fatalf("receipt %+v", f)
	}
	if err = s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketFacts).Bucket([]byte("a")).Get([]byte(f.ID))
		if bytes.Contains(raw, []byte(`"evidence"`)) {
			t.Fatal("projection persisted in Fact")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.List("a")
	if err != nil || len(rows) != 1 || rows[0].Evidence == nil || rows[0].Evidence.Quote != quote {
		t.Fatalf("reopen %+v %v", rows, err)
	}
	got, err := s.FetchCitation(ctx, "a", f.ID, hash)
	if err != nil || got.Evidence.Source != source || got.Evidence.Quote != quote {
		t.Fatalf("fetch %+v %v", got, err)
	}
	if _, err = s.FetchCitation(ctx, "other", f.ID, hash); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("namespace isolation %v", err)
	}
	if _, err = s.FetchCitation(ctx, "a", f.ID, ""); err == nil {
		t.Fatal("fetch accepted current rather than exact version")
	}
	inspected, err := s.InspectLineage(ctx, "a", f.ID, hash)
	if err != nil || inspected.Evidence == nil || inspected.Evidence.Quote != quote {
		t.Fatalf("inspection %+v %v", inspected, err)
	}
	archive, err := s.ExportLineage(ctx, "a")
	if err != nil || len(archive.Evidence) != 1 || archive.Evidence[0].Evidence == nil || archive.Evidence[0].Evidence.Quote != quote {
		t.Fatalf("archive %+v %v", archive, err)
	}
	rows, _ = s.List("a")
	if rows[0].AccessCount != 0 {
		t.Fatal("fetch touched recall state")
	}
}

func TestCitationAtomicLimitsAndConfiguredQuote(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), MaxQuoteBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, opts := range []WriteOptions{citationOptions(strings.Repeat("s", 513), "ok"), citationOptions("s", "ééééé"), citationOptions("s", string([]byte{0xff}))} {
		if _, err = s.PutManyReturningFacts(context.Background(), "a", []WriteFact{{Text: "valid", Options: citationOptions("s", "ok")}, {Text: "invalid", Options: opts}}); err == nil {
			t.Fatal("invalid evidence accepted")
		}
		rows, _ := s.List("a")
		if len(rows) != 0 {
			t.Fatal("partial batch")
		}
	}
	if _, err = s.PutWithOptionsReturningFact(context.Background(), "a", "exact boundary", citationOptions("s", "éééé")); err != nil {
		t.Fatal(err)
	}
	large, err := Open(StoreConfig{DataDir: t.TempDir(), MaxQuoteBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer large.Close()
	if _, err = large.PutWithOptionsReturningFact(context.Background(), "a", "configured", citationOptions("s", strings.Repeat("q", 3000))); err != nil {
		t.Fatal(err)
	}
}

func TestCitationLexicalScanIndexReopen(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "scan", true: "index"}[indexed], func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(StoreConfig{DataDir: dir, CandidateRetrieval: indexed})
			if err != nil {
				t.Fatal(err)
			}
			f, err := s.PutWithOptionsReturningFact(context.Background(), "a", "ordinary assertion", citationOptions("doc", "zebracitationuniqueterm"))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Put(context.Background(), "a", "unrelated control"); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err = Open(StoreConfig{DataDir: dir, CandidateRetrieval: indexed})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.SearchScoped(context.Background(), "a", []string{"zebracitationuniqueterm"}, 8, false, RecallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Facts) == 0 || got.Facts[0].ID != f.ID || got.Facts[0].Evidence == nil || got.Facts[0].Evidence.Quote != "zebracitationuniqueterm" {
				t.Fatalf("quote-only recall %+v", got)
			}
		})
	}
}

func TestCitationRevisionDoesNotInheritAndHistoryRetained(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	old, err := s.PutWithOptionsReturningFact(ctx, "a", "old assertion", citationOptions("source-old", "literal-old"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.ReviseFactsWithOptions(ctx, "a", "new assertion", WriteOptions{}, old)
	if err != nil {
		t.Fatal(err)
	}
	if next.Evidence != nil {
		t.Fatal("inherited evidence")
	}
	got, err := s.FetchCitation(ctx, "a", old.ID, NewEvidenceSnapshot(old).ContentHash)
	if err != nil || got.Evidence.Quote != "literal-old" {
		t.Fatalf("history %+v %v", got, err)
	}
	newest, err := s.ReviseFactsWithOptions(ctx, "a", "newest assertion", citationOptions("source-new", "literal-new"), next)
	if err != nil || newest.Evidence == nil || newest.Evidence.Quote != "literal-new" {
		t.Fatalf("new citation %+v %v", newest, err)
	}
}

func TestCitationQuotaRollsBackAndGCTracksSnapshots(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), MaxQuoteBytes: 2 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err = s.PutWithOptionsReturningFact(ctx, "a", "too much", citationOptions("s", strings.Repeat("q", 1<<20))); !errors.Is(err, ErrEvidenceQuota) {
		t.Fatalf("quota %v", err)
	}
	rows, _ := s.List("a")
	if len(rows) != 0 {
		t.Fatal("quota left partial fact")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	f, err := s.PutWithOptionsReturningFact(ctx, "a", "temporary", citationOptions("s", "q"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Delete("a", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FetchCitation(ctx, "a", f.ID, NewEvidenceSnapshot(f).ContentHash); err != nil {
		t.Fatalf("lost during grace %v", err)
	}
	now = now.Add(evidenceOrphanGrace + time.Second)
	if err = s.sweepLineage("a", now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FetchCitation(ctx, "a", f.ID, NewEvidenceSnapshot(f).ContentHash); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("expired citation %v", err)
	}
	if err = s.db.View(func(tx *bolt.Tx) error {
		b, _ := evidenceNamespace(tx, bucketCitations, "a", false)
		if b != nil && b.Get([]byte(NewEvidenceSnapshot(f).key())) != nil {
			t.Fatal("orphan citation retained")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCitationImportAndBackup(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	f, err := s.PutWithOptionsReturningFact(ctx, "a", "import assertion", citationOptions("d\r\n\t", "é漢字\"\n\r\n\t"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]Fact{f})
	mapped, err := s.ImportFactsJSON(ctx, "b", bytes.NewReader(raw))
	if err != nil || len(mapped) != 1 || mapped[0].ID == f.ID {
		t.Fatalf("import %+v %v", mapped, err)
	}
	rows, _ := s.List("b")
	if len(rows) != 1 || rows[0].Evidence == nil || rows[0].Evidence.Quote != f.Evidence.Quote || rows[0].Evidence.Source != f.Evidence.Source {
		t.Fatalf("imported %+v", rows)
	}
	bad := f
	bad.SupersededBy = "retired"
	raw, _ = json.Marshal([]Fact{f, bad})
	if _, err = s.ImportFactsJSON(ctx, "c", bytes.NewReader(raw)); err == nil {
		t.Fatal("import revived history")
	}
	rows, _ = s.List("c")
	if len(rows) != 0 {
		t.Fatal("partial import")
	}
	var backup bytes.Buffer
	if err = s.Backup(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "memory.db")
	if err = RestoreLineageBackup(ctx, &backup, dest); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dest, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.View(func(tx *bolt.Tx) error {
		c, err := citationAtTx(tx, "a", NewEvidenceSnapshot(f).key())
		if err != nil {
			return err
		}
		if c == nil || c.Quote != f.Evidence.Quote {
			t.Fatal("backup lost citation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCitationConcurrentRevisionsDoNotCrossEvidence(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan string, 2)
	releases := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	firstRelease := sync.OnceFunc(func() { close(releases["first"]) })
	secondRelease := sync.OnceFunc(func() { close(releases["second"]) })
	defer firstRelease()
	defer secondRelease()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: atomicRevisionEmbedder{
		before: func(text string) {
			if release, ok := releases[text]; ok {
				entered <- text
				<-release
			}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	ctx := context.Background()
	f, err := s.PutWithOptionsReturningFact(ctx, "a", "original", citationOptions("origin", "original quote"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan atomicRevisionResult, 2)
	for _, value := range []string{"first", "second"} {
		go func(v string) {
			next, e := s.ReviseFactsWithOptions(ctx, "a", v, citationOptions(v, v+" quote"), f)
			results <- atomicRevisionResult{next, e}
		}(value)
	}
	// Both calls must capture the same source version before either commits.
	a, b := awaitAtomicRevision(t, entered), awaitAtomicRevision(t, entered)
	if a == b {
		t.Fatalf("writers did not reach distinct barriers: %q, %q", a, b)
	}
	firstRelease()
	winner := awaitAtomicRevision(t, results)
	if winner.err != nil || winner.fact.Text != "first" || winner.fact.Evidence == nil || winner.fact.Evidence.Source != "first" || winner.fact.Evidence.Quote != "first quote" {
		t.Fatalf("winning receipt crossed evidence: %+v", winner)
	}
	secondRelease()
	loser := awaitAtomicRevision(t, results)
	if !errors.Is(loser.err, ErrFactChanged) || loser.fact.ID != "" {
		t.Fatalf("stale writer committed or lost its conflict: %+v", loser)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.List("a")
	if err != nil || len(rows) != 2 {
		t.Fatalf("reopened rows: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row.ID == f.ID {
			if row.SupersededBy != winner.fact.ID || row.Evidence == nil || row.Evidence.Source != "origin" || row.Evidence.Quote != "original quote" {
				t.Fatalf("original history crossed evidence: %+v", row)
			}
		} else if row.ID != winner.fact.ID || row.IsSuperseded() || row.Evidence == nil || row.Evidence.Source != "first" || row.Evidence.Quote != "first quote" {
			t.Fatalf("persisted winner crossed evidence: %+v", row)
		}
	}
	for _, expected := range []Fact{f, winner.fact} {
		got, err := s.FetchCitation(ctx, "a", expected.ID, NewEvidenceSnapshot(expected).ContentHash)
		if err != nil || got.Evidence == nil || got.Evidence.Source != expected.Evidence.Source || got.Evidence.Quote != expected.Evidence.Quote {
			t.Fatalf("version-specific history crossed evidence: %+v %v", got, err)
		}
	}
}

func TestCitationOldWriterDivergenceNeverAttachesOldQuote(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	f, err := s.PutWithOptionsReturningFact(ctx, "a", "original", citationOptions("s", "old quote"))
	if err != nil {
		t.Fatal(err)
	}
	hash := NewEvidenceSnapshot(f).ContentHash
	modified := f
	modified.Confidence = "verified"
	modified.Evidence = nil
	raw, err := modified.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketFacts).Bucket([]byte("a")).Put([]byte(f.ID), raw) }); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List("a")
	if err != nil || len(rows) != 1 || rows[0].Evidence != nil {
		t.Fatalf("cross-version evidence %+v %v", rows, err)
	}
	historical, err := s.FetchCitation(ctx, "a", f.ID, hash)
	if err != nil || historical.Evidence.Quote != "old quote" {
		t.Fatalf("lost old version %+v %v", historical, err)
	}
	stats, err := s.Stats("a")
	if err != nil || stats.LiveEvidenceCount == nil || *stats.LiveEvidenceCount != 0 {
		t.Fatalf("counted historical evidence %+v %v", stats, err)
	}
}

func TestCitationHooksPreserveLegacyAndMarkExactSelection(t *testing.T) {
	ctx := context.Background()
	for _, query := range []string{"", "deployment"} {
		t.Run("query="+query, func(t *testing.T) {
			s, err := Open(StoreConfig{DataDir: t.TempDir(), ConfidenceWeight: 0.5})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
			if err = s.Put(ctx, "a", "deployment local"); err != nil {
				t.Fatal(err)
			}
			if err = s.Put(ctx, SharedAgentID, "deployment shared"); err != nil {
				t.Fatal(err)
			}
			s.SetKG(&confidenceHintGraph{labels: []string{"deployment graph"}}, &recordingExtractor{})
			expected, err := s.Recall(ctx, "a", query, 8)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.RecallForHooks(ctx, "a", query, 8)
			if err != nil {
				t.Fatal(err)
			}
			expectedJSON, _ := json.Marshal(expected)
			gotJSON, _ := json.Marshal(got)
			if !bytes.Equal(expectedJSON, gotJSON) {
				t.Fatalf("legacy changed %s != %s", gotJSON, expectedJSON)
			}
			facts, _ := s.List("a")
			if len(facts) != 1 || facts[0].AccessCount != 2 {
				t.Fatalf("hook access %+v", facts)
			}
		})
	}
	s, err := Open(StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.PutWithOptionsReturningFact(ctx, "a", "deployment evidenced", citationOptions("s", "secret literal")); err != nil {
		t.Fatal(err)
	}
	if err = s.Put(ctx, SharedAgentID, "deployment shared"); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecallForHooks(ctx, "a", "deployment", 8)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "deployment evidenced [evidence]") || strings.Contains(joined, "secret literal") {
		t.Fatalf("hook evidence %q", joined)
	}
	stats, err := s.Stats("a")
	if err != nil || stats.LiveEvidenceCount == nil || *stats.LiveEvidenceCount != 1 {
		t.Fatalf("count %+v %v", stats, err)
	}
}

func TestCitationOptionsSnapshotBeforeEmbedder(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			source, quote := "original source", "original quote"
			provider := &utilityEmbedder{fn: func(string) []float32 { source = "changed"; quote = strings.Repeat("x", 4096); return nil }}
			s, err := Open(StoreConfig{DataDir: t.TempDir(), Embedder: provider})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			opts := WriteOptions{Source: &source, Quote: &quote}
			var f Fact
			if batch {
				var rows []Fact
				rows, err = s.PutManyReturningFacts(context.Background(), "a", []WriteFact{{Text: "assertion", Options: opts}})
				if err == nil {
					f = rows[0]
				}
			} else {
				f, err = s.PutWithOptionsReturningFact(context.Background(), "a", "assertion", opts)
			}
			if err != nil || f.Evidence == nil || f.Evidence.Source != "original source" || f.Evidence.Quote != "original quote" {
				t.Fatalf("mutable options reached commit: %+v %v", f, err)
			}
		})
	}
}

func TestCitationSearchPreservesSameTextVersionsAndQueryAssociations(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("index=%t/shared=%t", indexed, shared), func(t *testing.T) {
				s, err := Open(StoreConfig{DataDir: t.TempDir(), CandidateRetrieval: indexed})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				ctx := context.Background()
				a, err := s.PutWithOptionsReturningFact(ctx, "a", "deployment window", citationOptions("amber-source", "amberunique"))
				if err != nil {
					t.Fatal(err)
				}
				ns := "a"
				if shared {
					ns = SharedAgentID
				}
				b, err := s.PutWithOptionsReturningFact(ctx, ns, "deployment window", citationOptions("cobalt-source", "cobaltunique"))
				if err != nil {
					t.Fatal(err)
				}
				// Controls make quote-only terms discriminative within each namespace.
				if err = s.Put(ctx, "a", "unrelated control"); err != nil {
					t.Fatal(err)
				}
				if shared {
					if err = s.Put(ctx, ns, "other unrelated control"); err != nil {
						t.Fatal(err)
					}
				}
				topK := 1
				if shared {
					topK = 2
				}
				got, err := s.SearchScoped(ctx, "a", []string{"amberunique", "cobaltunique"}, topK, shared, RecallOptions{})
				if err != nil {
					t.Fatal(err)
				}
				index := map[string]int{}
				for i, f := range got.Facts {
					index[f.ID] = i
					if f.ID == a.ID && (f.Namespace != a.AgentID || f.ContentHash != NewEvidenceSnapshot(a).ContentHash || f.Evidence == nil || f.Evidence.Source != "amber-source" || f.Evidence.Quote != "amberunique") {
						t.Fatal("amber evidence crossed")
					}
					if f.ID == b.ID && (f.Namespace != b.AgentID || f.ContentHash != NewEvidenceSnapshot(b).ContentHash || f.Evidence == nil || f.Evidence.Source != "cobalt-source" || f.Evidence.Quote != "cobaltunique") {
						t.Fatal("cobalt evidence crossed")
					}
				}
				ai, aok := index[a.ID]
				bi, bok := index[b.ID]
				if !aok || !bok || ai == bi {
					t.Fatalf("identity collapsed: %+v", got)
				}
				if len(got.PerQuery) != 2 {
					t.Fatalf("query references missing %+v", got)
				}
				if !shared && (len(got.PerQuery[0]) != 1 || len(got.PerQuery[1]) != 1 || got.PerQuery[0][0] != ai || got.PerQuery[1][0] != bi) {
					t.Fatalf("wrong query citations %+v", got)
				}
				if shared && (!slices.Contains(got.PerQuery[0], ai) || !slices.Contains(got.PerQuery[1], bi)) {
					t.Fatalf("shared query points at another citation: %+v", got)
				}
				for _, query := range got.PerQuery {
					for _, i := range query {
						if i < 0 || i >= len(got.Facts) {
							t.Fatal("invalid query index")
						}
					}
				}
				// Full/legacy keeps its text-level contract even with cited duplicates.
				legacy, err := s.RecallAll(ctx, "a", "deployment", 8)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, text := range legacy {
					if text == "deployment window" {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("legacy duplicate contract changed: %v", legacy)
				}
			})
		}
	}
}

// This opt-in integration uses an actual pre-citation executable. A skipped
// invocation is not evidence for U303; the receipt records its exact SHA.
func TestCitationPriorExecutableDoesNotLoseLiveEvidence(t *testing.T) {
	old := os.Getenv("GRAYMATTER_CITATION_OLD_BINARY")
	if old == "" {
		t.Skip("set GRAYMATTER_CITATION_OLD_BINARY to a real pre-U3 executable")
	}
	data, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prior executable SHA-256 %x", sha256.Sum256(data))
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source, quote := "old-executable/source\r\n", "literal café 漢字\"\n\r\n\t"
	live, err := s.PutWithOptionsReturningFact(ctx, "a", "live assertion", citationOptions(source, quote))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().UTC().Add(-365 * 24 * time.Hour) }
	stale, err := s.PutWithOptionsReturningFact(ctx, "a", "stale assertion", citationOptions("old", "retained old quote"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, old, "--no-daemon", "--quiet", "--dir", dir, "consolidate", "a")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GRAYMATTER_") || strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "VOYAGE_") || strings.HasPrefix(upper, "OLLAMA_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "GRAYMATTER_OLLAMA_URL=disabled://citation-old-binary", "GRAYMATTER_KG=0")
	output, err := cmd.CombinedOutput()
	t.Logf("prior consolidate: %s", output)
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.List("a")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rows {
		if f.ID == stale.ID {
			t.Fatal("prior binary did not prune the stale row")
		}
	}
	if len(rows) != 1 || rows[0].ID != live.ID || rows[0].Weight >= live.Weight {
		t.Fatalf("prior binary did not decay live fact: %+v", rows)
	}
	got, err := s.FetchCitation(ctx, "a", live.ID, NewEvidenceSnapshot(live).ContentHash)
	if err != nil || got.Evidence.Source != source || got.Evidence.Quote != quote {
		t.Fatalf("old executable lost evidence %+v %v", got, err)
	}
}
