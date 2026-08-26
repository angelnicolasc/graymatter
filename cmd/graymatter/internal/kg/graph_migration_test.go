package kg

import (
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Node IDs are type-scoped since scheme v2. A store written under v1 (bare
// lowercased labels) must be wiped on open - nodes are derivable state,
// re-extracted from facts via the consolidation watermark - and the wipe
// must be idempotent: a v2 store reopened loses nothing.

func openRawGraph(t *testing.T, dir string) (*bolt.DB, func()) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, "test.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	return db, func() { _ = db.Close() }
}

func TestOpen_WipesV1GraphAndWatermark(t *testing.T) {
	db, cleanup := openRawGraph(t, t.TempDir())
	defer cleanup()

	// Seed a v1-shaped store by hand: untyped node/edge IDs plus extraction
	// watermarks, and no kg_meta bucket at all.
	if err := db.Update(func(tx *bolt.Tx) error {
		nb, err := tx.CreateBucketIfNotExists(bucketNodes)
		if err != nil {
			return err
		}
		if err := nb.Put([]byte("maria rodriguez"), []byte(`{"id":"maria rodriguez","label":"Maria Rodriguez","entity_type":"person"}`)); err != nil {
			return err
		}
		eb, err := tx.CreateBucketIfNotExists(bucketEdges)
		if err != nil {
			return err
		}
		if err := eb.Put([]byte("maria rodriguez|acme|co_mentioned"), []byte(`{}`)); err != nil {
			return err
		}
		wb, err := tx.CreateBucketIfNotExists([]byte("kg_extracted"))
		if err != nil {
			return err
		}
		return wb.Put([]byte("agent\x00fact-1"), []byte("deadbeef"))
	}); err != nil {
		t.Fatalf("seed v1 store: %v", err)
	}

	g, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	nodes, _ := g.AllNodes()
	if len(nodes) != 0 {
		t.Fatalf("v1 nodes survived the scheme migration: %v", nodes)
	}
	var watermarkKeys int
	_ = db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte("kg_extracted")); b != nil {
			watermarkKeys = b.Stats().KeyN
		}
		return nil
	})
	if watermarkKeys != 0 {
		t.Errorf("extraction watermark survived: %d keys; facts would never be re-extracted", watermarkKeys)
	}

	// The scheme marker is now recorded.
	err = db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketKGMeta).Get([]byte(metaKeyScheme))
		if string(v) != idSchemeVersion {
			t.Errorf("scheme marker = %q, want %q", v, idSchemeVersion)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpen_V2StoreIsPreservedAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db, cleanup := openRawGraph(t, dir)
	defer cleanup()

	g, err := Open(db)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := g.Upsert(Node{ID: "person:maria", Label: "Maria", EntityType: "person"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Reopen through a fresh Graph over the same db: the scheme matches, so
	// the v2 node must survive.
	g2, err := Open(db)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	nodes, _ := g2.AllNodes()
	found := false
	for _, n := range nodes {
		if n.ID == "person:maria" {
			found = true
		}
	}
	if !found {
		t.Fatalf("v2 node lost on reopen: %v", nodes)
	}
}
