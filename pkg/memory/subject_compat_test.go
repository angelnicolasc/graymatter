package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A skipped run is not evidence for U408. Validation supplies the frozen r4
// executable and records its SHA, independently of the new build under test.
func TestSubjectPriorExecutablePreservesHeadsAndExactEvidence(t *testing.T) {
	oldBinary := os.Getenv("GRAYMATTER_SUBJECT_OLD_BINARY")
	if oldBinary == "" {
		t.Skip("set GRAYMATTER_SUBJECT_OLD_BINARY to the actual frozen r4 executable")
	}
	data, err := os.ReadFile(oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prior executable SHA-256 %x", sha256.Sum256(data))
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.now = func() time.Time { return now.Add(-365 * 24 * time.Hour) }
	old, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "previous policy", WriteOptions{Subject: subjectString("policy"), Source: subjectString("memo-old"), Quote: subjectString("old quote\r\n漢字")})
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "old bridge", WriteOptions{Subject: subjectString("bridge")})
	if err != nil {
		t.Fatal(err)
	}
	dangling, err := s.PutWithOptionsReturningFact(ctx, "dangling-subject", "expired", WriteOptions{Subject: subjectString("missing")})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	current, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "current policy", WriteOptions{Subject: subjectString("policy"), Source: subjectString("memo-current"), Quote: subjectString("current quote\n\tá")})
	if err != nil {
		t.Fatal(err)
	}
	var headsBefore map[string]subjectHead
	if err := s.db.View(func(tx *bolt.Tx) error { var err error; headsBefore, err = subjectHeadsTx(tx, subjectNS); return err }); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	runOld := func(args ...string) string {
		t.Helper()
		commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		base := []string{"--no-daemon", "--quiet", "--dir", dir}
		cmd := exec.CommandContext(commandCtx, oldBinary, append(base, args...)...)
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			upper := strings.ToUpper(key)
			if strings.HasPrefix(upper, "GRAYMATTER_") || strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "VOYAGE_") || strings.HasPrefix(upper, "OLLAMA_") {
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		cmd.Env = append(cmd.Env, "GRAYMATTER_OLLAMA_URL=disabled://subject-old-binary", "GRAYMATTER_KG=0")
		output, err := cmd.CombinedOutput()
		t.Logf("prior %v: %s", args, output)
		if err != nil {
			t.Fatal(err)
		}
		return string(output)
	}
	help := runOld("revise", "--help")
	if !strings.Contains(help, "old-fact") || !strings.Contains(help, "new-fact") {
		t.Fatalf("prior executable does not expose the expected revise contract: %s", help)
	}
	runOld("revise", subjectNS, bridge.Text, "live bridge successor")
	for _, ns := range []string{subjectNS, "dangling-subject"} {
		runOld("consolidate", ns)
	}
	s, err = Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, f := range []Fact{old, bridge, dangling} {
		if _, found := s.loadFact(f.AgentID, f.ID); found {
			t.Fatalf("prior binary did not prune %s", f.ID)
		}
	}
	rows, err := s.List(subjectNS)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("live rows %+v", rows)
	}
	for _, f := range rows {
		if len(f.Subjects) != 1 || f.Subjects[0].Revisions != 1 || f.Weight >= 1 {
			t.Fatalf("old writer lost metadata or failed to rewrite live row %+v", f)
		}
		if f.ID != current.ID && f.Text != "live bridge successor" {
			t.Fatal("unexpected live ID")
		}
	}
	for _, f := range []Fact{old, current} {
		got, err := s.FetchCitation(ctx, subjectNS, f.ID, NewEvidenceSnapshot(f).ContentHash)
		if err != nil || got.Evidence == nil || got.Evidence.Source != f.Evidence.Source || got.Evidence.Quote != f.Evidence.Quote {
			t.Fatalf("citation changed %+v %v", got, err)
		}
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		after, err := subjectHeadsTx(tx, subjectNS)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(headsBefore, after) {
			t.Fatal("old writer rewrote subject bucket")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := revisionDBSnapshot(t, s)
	_, err = s.PutWithOptionsReturningFact(ctx, "dangling-subject", "must fail", WriteOptions{Subject: subjectString("missing")})
	if !errors.Is(err, ErrSubjectHeadUnavailable) || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("missing anchor resurrected %v", err)
	}
	if _, err = s.PutWithOptionsReturningFact(ctx, subjectNS, "confirmed current", WriteOptions{Subject: subjectString("bridge")}); err != nil {
		t.Fatal(err)
	}
}

func TestSubjectBackupRestoreOwnersCitationsAndClosedMarker(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	ctx := context.Background()
	old, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "policy old", WriteOptions{Subject: subjectString("policy"), Source: subjectString("old-source"), Quote: subjectString("old\r\nquote")})
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "policy new", WriteOptions{Subject: subjectString("policy"), Source: subjectString("new-source"), Quote: subjectString("new\nquote")})
	if err != nil {
		t.Fatal(err)
	}
	closed := putSubject(t, s, "closed", "closed assertion").Fact
	if err = s.Delete(subjectNS, closed.ID); err != nil {
		t.Fatal(err)
	}
	var image bytes.Buffer
	if err = s.Backup(ctx, &image); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = RestoreLineageBackup(ctx, bytes.NewReader(image.Bytes()), filepath.Join(dir, filepath.Base(s.db.Path()))); err != nil {
		t.Fatal(err)
	}
	restored := openAtomicRevisionStore(t, StoreConfig{DataDir: dir})
	for _, f := range []Fact{old, current} {
		got, err := restored.FetchCitation(ctx, subjectNS, f.ID, NewEvidenceSnapshot(f).ContentHash)
		if err != nil || got.Evidence == nil || got.Evidence.Source != f.Evidence.Source || got.Evidence.Quote != f.Evidence.Quote {
			t.Fatalf("restored citation %+v %v", got, err)
		}
	}
	if err = restored.db.View(func(tx *bolt.Tx) error {
		if err := validateSubjectBackupTx(ctx, tx); err != nil {
			return err
		}
		idx, err := resolveSubjectIndexTx(tx, subjectNS)
		if err != nil {
			return err
		}
		if !idx.Heads["closed"].Closed || idx.Heads["policy"].Fact.ID != current.ID {
			t.Fatal("wrong restored heads")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = restored.sweepLineage(subjectNS, restored.now().Add(evidenceOrphanGrace+time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []Fact{old, current} {
		if _, err := restored.FetchCitation(ctx, subjectNS, f.ID, NewEvidenceSnapshot(f).ContentHash); err != nil {
			t.Fatalf("reachable evidence GC %v", err)
		}
	}
	if _, err := restored.InspectLineage(ctx, subjectNS, closed.ID, NewEvidenceSnapshot(closed).ContentHash); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("closed subject created infinite history %v", err)
	}
	archive, err := restored.ExportLineage(ctx, subjectNS)
	if err != nil || len(archive.SubjectHeads) != 2 {
		t.Fatalf("closed marker vanished %+v %v", archive, err)
	}
}

func TestSubjectBatchEvidenceQuotaRollsBackAllHeads(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	before := revisionDBSnapshot(t, s)
	_, err := s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{
		{Text: strings.Repeat("a", evidenceTransactionBytes/2), Options: WriteOptions{Subject: subjectString("A")}},
		{Text: strings.Repeat("b", evidenceTransactionBytes/2), Options: WriteOptions{Subject: subjectString("B")}},
	})
	if !errors.Is(err, ErrEvidenceQuota) || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("quota failed to roll back transaction %v", err)
	}
}
