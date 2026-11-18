package memory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func runA1CompatibilityCLI(t *testing.T, executable, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, append([]string{"--no-daemon", "--quiet", "--dir", dir}, args...)...)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "GRAYMATTER_") || strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "VOYAGE_") || strings.HasPrefix(key, "OLLAMA_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "GRAYMATTER_OLLAMA_URL=disabled://a1-compat", "GRAYMATTER_KG=0")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	t.Logf("command %q, error %v, output %s", cmd.Args, err, output)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

// Covers the aged prune rows of A106 and the live citation control of A107.
// The external binaries perform actual maintenance. Only fixture creation and
// subsequent candidate operations run in-process, with the test's fixed clock.
func TestA1LegacyAgedStoreCompatibility(t *testing.T) {
	type previous struct {
		name    string
		pathEnv string
		shaEnv  string
		winSHA  string
	}
	versions := []previous{
		{"published", "GRAYMATTER_A1_PUBLISHED_BINARY", "GRAYMATTER_A1_PUBLISHED_SHA256", "46f93ea8a3b7948d2669a216d0a5568bd551e8feb17c4dea4b8e408cd9bb4bda"},
		{"r4", "GRAYMATTER_A1_R4_BINARY", "GRAYMATTER_A1_R4_SHA256", "8b11054cb1d19a96b98024104c7f3faa784cafb72508b76c4fa11489295f0ae2"},
	}
	if os.Getenv(versions[0].pathEnv) == "" && os.Getenv(versions[1].pathEnv) == "" {
		t.Skip("set both real prior executables for A106/A107; a skipped run is not evidence")
	}
	candidate := os.Getenv("GRAYMATTER_A1_CANDIDATE_BINARY")
	candidateBytes, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatalf("set GRAYMATTER_A1_CANDIDATE_BINARY for the real doctor and recall checks: %v", err)
	}
	candidateSHA := fmt.Sprintf("%x", sha256.Sum256(candidateBytes))
	if candidateSHA != os.Getenv("GRAYMATTER_A1_CANDIDATE_SHA256") {
		t.Fatalf("candidate executable SHA mismatch: got %s", candidateSHA)
	}
	t.Logf("candidate executable %s SHA256 %s", candidate, candidateSHA)
	for _, version := range versions {
		t.Run(version.name, func(t *testing.T) {
			old := os.Getenv(version.pathEnv)
			data, err := os.ReadFile(old)
			if err != nil {
				t.Fatal(err)
			}
			expectedSHA := os.Getenv(version.shaEnv)
			if runtime.GOOS == "windows" && expectedSHA != version.winSHA {
				t.Fatalf("wrong sealed Windows prior SHA for %s", version.name)
			}
			if runtime.GOOS == "linux" && version.name == "r4" && expectedSHA != "b75d2d3fe4f3d8c81c00d1049d2c5b1acd9633f7cff8cf01a084595854c80da0" {
				t.Fatal("wrong sealed Linux r4 SHA")
			}
			actualSHA := fmt.Sprintf("%x", sha256.Sum256(data))
			if actualSHA != expectedSHA {
				t.Fatalf("prior executable SHA mismatch: got %s want %s", actualSHA, expectedSHA)
			}
			t.Logf("prior executable %s SHA256 %s", old, expectedSHA)
			dir := t.TempDir()
			const ns = "a1-aged"
			const subject = "deploy window"
			const source = "source/retained"
			const quote = "Literal café 漢字 \"quote\"\r\n\tkept"
			ctx := context.Background()
			now := time.Now().UTC()
			s, err := Open(StoreConfig{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if s != nil {
					_ = s.Close()
				}
			})
			put := func(text string, options WriteOptions) Fact {
				t.Helper()
				fact, err := s.PutWithOptionsReturningFact(ctx, ns, text, options)
				if err != nil {
					t.Fatal(err)
				}
				return fact
			}
			s.now = func() time.Time { return now.Add(-365 * 24 * time.Hour) }
			head := put("Ancient deploy window", WriteOptions{Subject: subjectString(subject), Source: subjectString("head/source"), Quote: subjectString("Ancient literal")})
			stale := put("Ancient citation to prune", WriteOptions{Source: subjectString("stale/source"), Quote: subjectString("Stale literal")})
			s.now = func() time.Time { return now.Add(-7 * 24 * time.Hour) }
			live := put("Current cache policy", WriteOptions{Source: subjectString(source), Quote: subjectString(quote)})
			var originalHead subjectHead
			if err = s.db.View(func(tx *bolt.Tx) error {
				heads, err := subjectHeadsTx(tx, ns)
				originalHead = heads[subject]
				return err
			}); err != nil || originalHead.Reference.FactID != head.ID {
				t.Fatalf("fixture head missing: %+v %v", originalHead, err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}

			runA1CompatibilityCLI(t, old, dir, "consolidate", ns)
			s, err = Open(StoreConfig{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return now }
			if _, found := s.loadFact(ns, head.ID); found {
				t.Fatal("prior binary did not prune the aged subject fact")
			}
			if _, found := s.loadFact(ns, stale.ID); found {
				t.Fatal("prior binary did not prune the aged cited fact")
			}
			rows, err := s.List(ns)
			if err != nil || len(rows) != 1 || rows[0].ID != live.ID || rows[0].Weight <= 0 || rows[0].Weight >= live.Weight || len(rows[0].Subjects) != 0 {
				t.Fatalf("A106/A107 expected only the decayed live fact, without inferred subject: %+v %v", rows, err)
			}
			checkCitation := func() {
				t.Helper()
				citation, err := s.FetchCitation(ctx, ns, live.ID, NewEvidenceSnapshot(live).ContentHash)
				if err != nil || citation.Evidence == nil || citation.Evidence.Source != source || citation.Evidence.Quote != quote {
					t.Fatalf("A107 live citation changed: %+v %v", citation, err)
				}
			}
			checkCitation()
			before := revisionDBSnapshot(t, s)
			if _, err := s.PutWithOptionsReturningFact(ctx, ns, "Unqualified recovery", WriteOptions{Subject: subjectString(subject)}); !errors.Is(err, ErrSubjectHeadUnavailable) || !strings.Contains(err.Error(), "replaces") {
				t.Fatalf("A105 unqualified write did not direct explicit recovery: %v", err)
			}
			if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
				t.Fatal("A105 rejected write changed the snapshot")
			}
			if err := s.Consolidate(ctx, ns, &testConsolidateCfg{halfLife: 720 * time.Hour}); err != nil {
				t.Fatalf("A107 candidate maintenance failed: %v", err)
			}
			checkCitation()
			s.now = func() time.Time { return now.Add(evidenceOrphanGrace + time.Second) }
			if err := s.Consolidate(ctx, ns, &testConsolidateCfg{halfLife: 720 * time.Hour}); err != nil {
				t.Fatalf("A106 candidate maintenance after evidence grace failed: %v", err)
			}
			checkCitation()
			if err := s.db.View(func(tx *bolt.Tx) error {
				heads, err := subjectHeadsTx(tx, ns)
				if heads[subject] != originalHead {
					t.Fatalf("A106 candidate maintenance rewrote the unresolved head: %+v", heads[subject])
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.InspectLineage(ctx, ns, head.ID, NewEvidenceSnapshot(head).ContentHash); err != nil {
				t.Fatalf("A106 unresolved head lost retained evidence: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = nil
			storeFile := filepath.Join(dir, "gray.db")
			beforeDoctor, err := os.ReadFile(storeFile)
			if err != nil {
				t.Fatal(err)
			}
			doctor := runA1CompatibilityCLI(t, candidate, dir, "doctor")
			if !strings.Contains(doctor, ns) || !strings.Contains(doctor, subject) || !strings.Contains(doctor, "replaces") {
				t.Fatalf("A106 doctor omitted project, subject or recovery path: %s", doctor)
			}
			afterDoctor, err := os.ReadFile(storeFile)
			if err != nil || sha256.Sum256(beforeDoctor) != sha256.Sum256(afterDoctor) {
				t.Fatalf("A106 doctor changed the store: %v", err)
			}
			recall := runA1CompatibilityCLI(t, candidate, dir, "recall", ns, "deploy window cache policy")
			if strings.Contains(recall, head.Text) {
				t.Fatal("A106 recall revived the pruned head")
			}
			s, err = Open(StoreConfig{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return now.Add(evidenceOrphanGrace + time.Second) }
			unrelated := put("Reviews need two approvals", WriteOptions{})
			otherSubject := put("Current oncall window", WriteOptions{Subject: subjectString("oncall window")})
			if err := s.SetPinned(ns, true, unrelated); err != nil {
				t.Fatal(err)
			}
			if err := s.SetPinned(ns, false, unrelated); err != nil {
				t.Fatal(err)
			}
			revised, err := s.ReviseWithOptions(ctx, ns, unrelated.Text, "Reviews need three approvals", WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Retire(ns, revised); err != nil {
				t.Fatal(err)
			}
			recovered := put("Recovered deploy window", WriteOptions{Subject: subjectString(subject), Replaces: []string{head.ID}})
			if len(recovered.Subjects) != 1 || recovered.Subjects[0].Name != subject || recovered.Subjects[0].Revisions != 1 {
				t.Fatalf("A106 explicit absent-head recovery did not continue revisions: %+v", recovered)
			}
			result, err := s.SearchScoped(ctx, ns, []string{"cache deploy"}, 8, false, RecallOptions{})
			if err != nil || len(result.Facts) != 3 {
				t.Fatalf("A106 recovery lost or revived rows: %+v %v", result, err)
			}
			for _, fact := range result.Facts {
				if fact.ID != recovered.ID && fact.ID != live.ID && fact.ID != otherSubject.ID {
					t.Fatalf("A106 unexpected fact after recovery: %+v", fact)
				}
			}
		})
	}
}
