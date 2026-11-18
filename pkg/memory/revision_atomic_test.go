package memory

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	atomicRevisionAgent = "atomic-revision"
	atomicReplacement   = "revisionmarker corrected"
)

type atomicRevisionMethod string

const (
	reviseByIDs      atomicRevisionMethod = "revise"
	reviseByCuration atomicRevisionMethod = "curate"
)

var atomicRevisionMethods = []atomicRevisionMethod{reviseByIDs, reviseByCuration}

func (m atomicRevisionMethod) run(ctx context.Context, s *Store, text string, sources []Fact) (Fact, error) {
	if m == reviseByCuration {
		f := sources[0]
		return s.CurateFact(ctx, atomicRevisionAgent, f.ID, "revise", text, f)
	}
	return s.ReviseFactsWithOptions(ctx, atomicRevisionAgent, text, WriteOptions{}, sources...)
}

type atomicRevisionEmbedder struct {
	before func(string)
	value  float32
}

func (e atomicRevisionEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if e.before != nil {
		e.before(text)
	}
	if text == atomicReplacement {
		return []float32{1, e.value}, nil
	}
	return []float32{1, 0}, nil
}
func (atomicRevisionEmbedder) Dimensions() int { return 2 }
func (atomicRevisionEmbedder) Name() string    { return "atomic-revision-fixture" }

func openAtomicRevisionStore(t *testing.T, cfg StoreConfig) *Store {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	cfg.VectorBackend = newFlakyVectorStore(0)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func atomicRevisionSources(t *testing.T, s *Store, m atomicRevisionMethod) []Fact {
	t.Helper()
	sources := []Fact{mustConfidenceFact(t, s, atomicRevisionAgent, "revisionmarker original first", "verified")}
	if m == reviseByIDs {
		sources = append(sources, mustConfidenceFact(t, s, atomicRevisionAgent, "revisionmarker original second", "unverified"))
	}
	return sources
}

// A logical snapshot covers every canonical, index, metadata and outbox write
// without depending on bbolt page layout or free-list bytes.
type revisionBucketEntry struct {
	path, key, value string
	isBucket         bool
}

func revisionDBSnapshot(t *testing.T, s *Store) []revisionBucketEntry {
	t.Helper()
	var entries []revisionBucketEntry
	if err := s.db.View(func(tx *bolt.Tx) error {
		var visit func(string, *bolt.Bucket) error
		visit = func(path string, b *bolt.Bucket) error {
			return b.ForEach(func(k, v []byte) error {
				entries = append(entries, revisionBucketEntry{path, string(k), string(v), v == nil})
				if v == nil {
					return visit(path+"/"+string(k), b.Bucket(k))
				}
				return nil
			})
		}
		return tx.ForEach(func(k []byte, b *bolt.Bucket) error {
			entries = append(entries, revisionBucketEntry{key: string(k), isBucket: true})
			return visit(string(k), b)
		})
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func replaceRevisionValueWithBucket(b *bolt.Bucket, key []byte) error {
	if err := b.Delete(key); err != nil {
		return err
	}
	_, err := b.CreateBucket(key)
	return err
}

func TestRevisionAtomicRollsBackRealWriteFailures(t *testing.T) {
	cases := []struct {
		name     string
		poison   func(*bolt.Tx, []Fact) error
		badValue bool
	}{
		{
			name: "last-source-recency-removal",
			poison: func(tx *bolt.Tx, sources []Fact) error {
				last := sources[len(sources)-1]
				return replaceRevisionValueWithBucket(idxBucketRO(tx, bucketIdxRecency, last.AgentID), recencyKey(last.CreatedAt, last.ID))
			},
		},
		{
			name:     "replacement-serialization",
			badValue: true,
		},
		{
			name: "replacement-agent-marker",
			poison: func(tx *bolt.Tx, _ []Fact) error {
				return replaceRevisionValueWithBucket(tx.Bucket(bucketAgents), []byte(atomicRevisionAgent))
			},
		},
		{
			name: "replacement-keyword-frequency",
			poison: func(tx *bolt.Tx, _ []Fact) error {
				return replaceRevisionValueWithBucket(idxBucketRO(tx, bucketIdxTerms, atomicRevisionAgent), []byte("corrected"))
			},
		},
		{
			name: "index-metadata",
			poison: func(tx *bolt.Tx, _ []Fact) error {
				return replaceRevisionValueWithBucket(tx.Bucket(bucketIdxMeta), []byte(atomicRevisionAgent))
			},
		},
		{
			name: "vector-outbox",
			poison: func(tx *bolt.Tx, _ []Fact) error {
				b := tx.Bucket(bucketPendingVector)
				key := []byte(atomicRevisionAgent)
				if b.Bucket(key) != nil {
					if err := b.DeleteBucket(key); err != nil {
						return err
					}
				}
				return b.Put(key, []byte("not an outbox bucket"))
			},
		},
	}
	for _, m := range atomicRevisionMethods {
		for _, tc := range cases {
			t.Run(string(m)+"/"+tc.name, func(t *testing.T) {
				var e atomicRevisionEmbedder
				if tc.badValue {
					e.value = float32(math.NaN())
				}
				s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: true, Embedder: e})
				sources := atomicRevisionSources(t, s, m)
				if tc.poison != nil {
					if err := s.db.Update(func(tx *bolt.Tx) error { return tc.poison(tx, sources) }); err != nil {
						t.Fatal(err)
					}
				}
				before := revisionDBSnapshot(t, s)
				puts := 0
				s.cfg.OnPut = func(string, string, time.Duration) { puts++ }
				f, err := m.run(context.Background(), s, atomicReplacement, sources)
				if tc.badValue {
					if err == nil || !strings.Contains(err.Error(), "unsupported value") {
						t.Errorf("serialization failure = %v, want unsupported value", err)
					}
				} else if !errors.Is(err, bolt.ErrIncompatibleValue) {
					t.Errorf("write failure = %v, want incompatible bbolt value", err)
				}
				if f.ID != "" || puts != 0 {
					t.Errorf("failed revision returned ID %q and fired %d observers", f.ID, puts)
				}
				if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
					t.Fatal("failed revision changed canonical rows, indexes, metadata or vector intent")
				}
			})
		}
	}
}

type atomicRevisionResult struct {
	fact Fact
	err  error
}

func awaitAtomicRevision[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(15 * time.Second):
		t.Fatal("revision did not reach the expected barrier")
		var zero T
		return zero
	}
}

func TestRevisionAtomicRevalidatesAfterEmbedding(t *testing.T) {
	for _, m := range atomicRevisionMethods {
		for _, field := range []string{"confidence", "pin", "text", "retired", "deleted", "access"} {
			t.Run(string(m)+"/"+field, func(t *testing.T) {
				entered, release := make(chan struct{}, 1), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: true, Embedder: atomicRevisionEmbedder{
					before: func(text string) {
						if text == atomicReplacement {
							entered <- struct{}{}
							<-release
						}
					},
				}})
				ctx := context.Background()
				sources := atomicRevisionSources(t, s, m)
				done := make(chan atomicRevisionResult, 1)
				go func() {
					f, err := m.run(ctx, s, atomicReplacement, sources)
					done <- atomicRevisionResult{f, err}
				}()
				defer unblock()
				awaitAtomicRevision(t, entered)
				changed := sources[len(sources)-1]
				var changeErr error
				switch field {
				case "confidence":
					changed.Confidence = "inferred"
					changeErr = s.UpdateFact(atomicRevisionAgent, changed)
				case "pin":
					changeErr = s.SetPinned(atomicRevisionAgent, true, changed)
				case "text":
					changed.Text = "revisionmarker independently corrected"
					changeErr = s.UpdateFact(atomicRevisionAgent, changed)
				case "retired":
					changeErr = s.Retire(atomicRevisionAgent, changed)
				case "deleted":
					changeErr = s.Delete(atomicRevisionAgent, changed.ID)
				case "access":
					changed.AccessCount = 13
					changed.AccessedAt = changed.AccessedAt.Add(time.Hour)
					changed.Weight = 0.8
					changeErr = s.UpdateFact(atomicRevisionAgent, changed)
				}
				before := revisionDBSnapshot(t, s)
				unblock()
				result := awaitAtomicRevision(t, done)
				if changeErr != nil {
					t.Fatal(changeErr)
				}
				if field == "access" {
					if result.err != nil || result.fact.ID == "" {
						t.Fatalf("access churn caused conflict: %+v", result)
					}
					current, ok := s.loadFact(atomicRevisionAgent, changed.ID)
					changed.SupersededBy = result.fact.ID
					if !ok || !reflect.DeepEqual(changed, current) {
						t.Fatalf("access churn lost on retirement: got %+v, want %+v", current, changed)
					}
					return
				}
				if result.fact.ID != "" || !errors.Is(result.err, ErrFactChanged) {
					t.Errorf("changed source returned %+v, want zero fact and ErrFactChanged", result)
				}
				if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
					t.Fatal("stale revision changed the independently updated state")
				}
			})
		}
	}
}

func checkAtomicRevisionVisibility(t *testing.T, s *Store, sources []Fact, replacementID string) {
	t.Helper()
	facts, err := s.List(atomicRevisionAgent)
	if err != nil {
		t.Fatal(err)
	}
	expected := len(sources)
	if replacementID != "" {
		expected++
	}
	if len(facts) != expected {
		t.Errorf("found %d canonical rows, want %d", len(facts), expected)
	}
	for _, original := range sources {
		current, ok := s.loadFact(atomicRevisionAgent, original.ID)
		original.SupersededBy = replacementID
		if !ok || !reflect.DeepEqual(current, original) {
			t.Errorf("source state %+v, want %+v", current, original)
		}
	}
	receipts, err := s.RecallPreview(context.Background(), atomicRevisionAgent, "revisionmarker", 8)
	if err != nil {
		t.Fatal(err)
	}
	if replacementID == "" {
		want := make(map[string]bool)
		for _, f := range sources {
			want[f.ID] = true
		}
		for _, receipt := range receipts {
			if !want[receipt.Provenance.FactID] {
				t.Errorf("pre-commit preview exposed %q", receipt.Provenance.FactID)
			}
			delete(want, receipt.Provenance.FactID)
		}
		if len(want) != 0 {
			t.Errorf("pre-commit preview omitted %v", want)
		}
		return
	}
	if len(receipts) != 1 || receipts[0].Text != atomicReplacement || receipts[0].Provenance.FactID != replacementID {
		t.Errorf("post-commit preview = %+v, want only replacement %s", receipts, replacementID)
	}
}

func TestRevisionAtomicObserverAndConcurrentReader(t *testing.T) {
	for _, m := range atomicRevisionMethods {
		t.Run(string(m), func(t *testing.T) {
			embedEntered, embedRelease := make(chan struct{}, 1), make(chan struct{})
			observerEntered, observerRelease := make(chan string, 1), make(chan struct{})
			unblockEmbed := sync.OnceFunc(func() { close(embedRelease) })
			unblockObserver := sync.OnceFunc(func() { close(observerRelease) })
			s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: true, Embedder: atomicRevisionEmbedder{
				before: func(text string) {
					if text == atomicReplacement {
						embedEntered <- struct{}{}
						<-embedRelease
					}
				},
			}})
			sources := atomicRevisionSources(t, s, m)
			s.cfg.OnPut = func(_ string, id string, _ time.Duration) {
				observerEntered <- id
				<-observerRelease
			}
			done := make(chan atomicRevisionResult, 1)
			go func() {
				f, err := m.run(context.Background(), s, atomicReplacement, sources)
				done <- atomicRevisionResult{f, err}
			}()
			defer unblockEmbed()
			defer unblockObserver()
			awaitAtomicRevision(t, embedEntered)
			checkAtomicRevisionVisibility(t, s, sources, "")
			unblockEmbed()
			replacementID := awaitAtomicRevision(t, observerEntered)
			checkAtomicRevisionVisibility(t, s, sources, replacementID)
			unblockObserver()
			result := awaitAtomicRevision(t, done)
			if result.err != nil || result.fact.ID != replacementID {
				t.Fatalf("revision outcome %+v, want observer ID %s", result, replacementID)
			}
		})
	}
}

func TestRevisionAtomicCancellation(t *testing.T) {
	for _, m := range atomicRevisionMethods {
		for _, phase := range []string{"before-commit", "after-commit"} {
			t.Run(string(m)+"/"+phase, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: true, Embedder: atomicRevisionEmbedder{
					before: func(text string) {
						if text == atomicReplacement && phase == "before-commit" {
							cancel()
						}
					},
				}})
				sources := atomicRevisionSources(t, s, m)
				before := revisionDBSnapshot(t, s)
				puts := 0
				s.cfg.OnPut = func(string, string, time.Duration) {
					puts++
					cancel()
				}
				f, err := m.run(ctx, s, atomicReplacement, sources)
				if phase == "before-commit" {
					if f.ID != "" || puts != 0 || !errors.Is(err, context.Canceled) {
						t.Fatalf("pre-commit cancellation returned %+v / %v with %d observers", f, err, puts)
					}
					if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
						t.Fatal("pre-commit cancellation changed the store")
					}
					return
				}
				if err != nil || f.ID == "" || puts != 1 || !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("post-commit cancellation returned %+v / %v with %d observers", f, err, puts)
				}
				checkAtomicRevisionVisibility(t, s, sources, f.ID)
				committed := revisionDBSnapshot(t, s)
				if retry, retryErr := m.run(context.Background(), s, atomicReplacement, sources); retry.ID != "" || retryErr == nil || puts != 1 {
					t.Fatalf("stale duplicate revision returned %+v / %v with %d observers", retry, retryErr, puts)
				}
				if !reflect.DeepEqual(committed, revisionDBSnapshot(t, s)) {
					t.Fatal("stale retry changed committed state")
				}
			})
		}
	}
}

func TestRevisionAtomicRejectsInvalidInput(t *testing.T) {
	for _, m := range atomicRevisionMethods {
		cases := []string{"missing", "foreign", "empty-text", "blank-text"}
		if m == reviseByIDs {
			cases = append(cases, "mixed-kind", "empty-sources", "invalid-confidence")
		}
		for _, name := range cases {
			t.Run(string(m)+"/"+name, func(t *testing.T) {
				s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: true})
				ctx := context.Background()
				source := mustConfidenceFact(t, s, atomicRevisionAgent, "revisionmarker original", "unverified")
				sources := []Fact{source}
				text := atomicReplacement
				switch name {
				case "missing":
					sources[0].ID = "absent-source-id"
				case "foreign":
					sources[0] = mustConfidenceFact(t, s, "another-agent", "private source", "verified")
				case "empty-text":
					text = ""
				case "blank-text":
					text = " \t"
				case "mixed-kind":
					alias, err := s.PutAlias(ctx, atomicRevisionAgent, "broker", []string{"acquiring"})
					if err != nil {
						t.Fatal(err)
					}
					sources = []Fact{alias, source}
				case "empty-sources":
					sources = nil
				}
				before := revisionDBSnapshot(t, s)
				var f Fact
				var err error
				if name == "invalid-confidence" {
					f, err = s.ReviseFactsWithOptions(ctx, atomicRevisionAgent, text, confidenceOptions(" verified"), sources...)
				} else {
					f, err = m.run(ctx, s, text, sources)
				}
				if err == nil || f.ID != "" {
					t.Errorf("invalid revision returned %+v / %v", f, err)
				}
				if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
					t.Fatal("invalid revision changed the store")
				}
			})
		}
	}
}

func TestRevisionAtomicAuthorizationAndReopen(t *testing.T) {
	for _, m := range atomicRevisionMethods {
		t.Run(string(m), func(t *testing.T) {
			dir := t.TempDir()
			cfg := StoreConfig{DataDir: dir, CandidateRetrieval: true}
			s := openAtomicRevisionStore(t, cfg)
			source := mustConfidenceFact(t, s, atomicRevisionAgent, "revisionmarker original", "unverified")
			if err := s.SetPinned(atomicRevisionAgent, true, source); err != nil {
				t.Fatal(err)
			}
			source, _ = s.loadFact(atomicRevisionAgent, source.ID)
			sibling := mustConfidenceFact(t, s, atomicRevisionAgent, source.Text, "verified")
			f, err := m.run(context.Background(), s, atomicReplacement, []Fact{source})
			if err != nil || f.ID == "" || f.Confidence != "unverified" || f.Pinned || !f.PinnedAt.IsZero() {
				t.Fatalf("explicit pinned revision = %+v / %v", f, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openAtomicRevisionStore(t, cfg)
			retired, ok := reopened.loadFact(atomicRevisionAgent, source.ID)
			source.SupersededBy = f.ID
			if !ok || !reflect.DeepEqual(retired, source) {
				t.Errorf("reopened source = %+v, want %+v", retired, source)
			}
			other, ok := reopened.loadFact(atomicRevisionAgent, sibling.ID)
			if !ok || !reflect.DeepEqual(other, sibling) {
				t.Errorf("exact target selection changed equal-text sibling: %+v", other)
			}
			replacement, ok := reopened.loadFact(atomicRevisionAgent, f.ID)
			if !ok || replacement.IsSuperseded() || replacement.Text != f.Text || replacement.Confidence != f.Confidence {
				t.Errorf("reopened replacement = %+v, want %+v", replacement, f)
			}
		})
	}
}

func TestRevisionAtomicCrashRecovery(t *testing.T) {
	const (
		childDir    = "GRAYMATTER_REVISION_TEST_DIR"
		childPhase  = "GRAYMATTER_REVISION_TEST_PHASE"
		childMethod = "GRAYMATTER_REVISION_TEST_METHOD"
		crashCode   = 73
	)
	if dir := os.Getenv(childDir); dir != "" {
		cfg := StoreConfig{DataDir: dir, CandidateRetrieval: true}
		switch os.Getenv(childPhase) {
		case "before-commit":
			cfg.Embedder = atomicRevisionEmbedder{before: func(text string) {
				if text == atomicReplacement {
					os.Exit(crashCode)
				}
			}}
		case "after-commit":
			cfg.OnPut = func(string, string, time.Duration) { os.Exit(crashCode) }
		default:
			t.Fatal("invalid crash fixture phase")
		}
		s := openAtomicRevisionStore(t, cfg)
		sources, err := s.List(atomicRevisionAgent)
		if err != nil || len(sources) == 0 {
			t.Fatalf("load crash fixture: %v / %v", sources, err)
		}
		_, err = atomicRevisionMethod(os.Getenv(childMethod)).run(context.Background(), s, atomicReplacement, sources)
		t.Fatalf("fixture did not crash at its barrier: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range atomicRevisionMethods {
		for _, phase := range []string{"before-commit", "after-commit"} {
			t.Run(string(m)+"/"+phase, func(t *testing.T) {
				dir := t.TempDir()
				cfg := StoreConfig{DataDir: dir, CandidateRetrieval: true}
				s := openAtomicRevisionStore(t, cfg)
				sources := atomicRevisionSources(t, s, m)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRevisionAtomicCrashRecovery$")
				cmd.Env = append(os.Environ(), childDir+"="+dir, childPhase+"="+phase, childMethod+"="+string(m))
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != crashCode {
					t.Fatalf("crash fixture returned %v: %s", err, output)
				}
				reopened := openAtomicRevisionStore(t, cfg)
				replacementID := ""
				if phase == "after-commit" {
					facts, err := reopened.List(atomicRevisionAgent)
					if err != nil {
						t.Fatal(err)
					}
					for _, f := range facts {
						if f.Text == atomicReplacement {
							replacementID = f.ID
							break
						}
					}
					if replacementID == "" {
						t.Fatal("committed replacement missing after crash")
					}
				}
				checkAtomicRevisionVisibility(t, reopened, sources, replacementID)
			})
		}
	}
}
