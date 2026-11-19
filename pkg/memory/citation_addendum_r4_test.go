package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// r4 can sweep an expired snapshot while preserving its pinned tombstone.
// Its real executable does not know the later citation bucket. Exercise a
// read-only open so startup cleanup cannot hide that mixed-version state.
func TestCitationR4ExpiredRetiredSnapshotReadOnly(t *testing.T) {
	oldBinary := os.Getenv("GRAYMATTER_SUBJECT_OLD_BINARY")
	if oldBinary == "" {
		t.Skip("requires the actual frozen r4 executable")
	}
	hashes := map[string]string{
		"windows": "8b11054cb1d19a96b98024104c7f3faa784cafb72508b76c4fa11489295f0ae2",
		"linux":   "b75d2d3fe4f3d8c81c00d1049d2c5b1acd9633f7cff8cf01a084595854c80da0",
	}
	raw, err := os.ReadFile(oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if digest != hashes[runtime.GOOS] {
		t.Fatalf("r4 SHA on %s: got %s want %s", runtime.GOOS, digest, hashes[runtime.GOOS])
	}
	t.Logf("r4 SHA-256 %s", digest)

	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	namespace := "citation-expiry"
	now := time.Now().UTC()
	s.now = func() time.Time { return now.Add(-2 * evidenceOrphanGrace) }
	old, err := s.PutWithOptionsReturningFact(ctx, namespace, "Historical wording", WriteOptions{Source: subjectString("old-memo"), Quote: subjectString("Previous literal")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned(namespace, true, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(namespace, old); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepLineage(namespace, s.now()); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(-24 * time.Hour) }
	live, err := s.PutWithOptionsReturningFact(ctx, namespace, "Current wording", WriteOptions{Source: subjectString("current-memo"), Quote: subjectString("Exact\r\n\tcafé 漢字")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, oldBinary, "--no-daemon", "--quiet", "--dir", dir, "consolidate", namespace)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		name = strings.ToUpper(name)
		if !strings.HasPrefix(name, "GRAYMATTER_") && !strings.HasPrefix(name, "ANTHROPIC_") &&
			!strings.HasPrefix(name, "OPENAI_") && !strings.HasPrefix(name, "VOYAGE_") && !strings.HasPrefix(name, "OLLAMA_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GRAYMATTER_OLLAMA_URL=disabled://citation-r4-expiry", "GRAYMATTER_KG=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("r4 consolidate: %v\n%s", err, output)
	} else {
		t.Logf("r4 consolidate: %s", output)
	}

	readOnly, err := Open(StoreConfig{DataDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	key := NewEvidenceSnapshot(old).key()
	if err := readOnly.db.View(func(tx *bolt.Tx) error {
		snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
		stale, err := citationEntry(tx, namespace, key)
		if err != nil || stale == nil || snapshots != nil && snapshots.Get([]byte(key)) != nil {
			return fmt.Errorf("r4 fixture did not retain a citation without its snapshot: %v %v", stale, err)
		}
		raw := lineageRowBucket(tx, namespace).Get([]byte(old.ID))
		row, err := unmarshalFactLite(raw)
		if err != nil || !row.Pinned || !row.IsSuperseded() {
			return fmt.Errorf("r4 fixture did not retain the pinned tombstone: %+v %v", row, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := readOnly.List(namespace)
	if err != nil {
		t.Fatalf("r4 expired citation blocked read-only listing: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("wrong canonical rows: %+v", rows)
	}
	for _, row := range rows {
		switch row.ID {
		case old.ID:
			raw, err := json.Marshal(row)
			if err != nil || row.Evidence != nil || !strings.Contains(string(raw), `"citation_unavailable":true`) {
				t.Fatalf("expired citation projected without diagnostic: %s %v", raw, err)
			}
		case live.ID:
			if row.Evidence == nil || *row.Evidence != *live.Evidence || row.Weight >= live.Weight {
				t.Fatalf("r4 lost live citation or did not decay live row: %+v", row)
			}
		default:
			t.Fatalf("unexpected row %s", row.ID)
		}
	}
	if _, err := readOnly.FetchCitation(ctx, namespace, old.ID, NewEvidenceSnapshot(old).ContentHash); err == nil {
		t.Fatal("an unavailable exact citation was returned")
	}
}
