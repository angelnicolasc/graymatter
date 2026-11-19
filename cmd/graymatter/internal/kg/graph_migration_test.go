package kg

import (
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Typed extraction is scheduled when a graph predates the node ID scheme.
// Legacy rows survive because re-extraction cannot reproduce all support.

func openRawGraph(t *testing.T, dir string) (*bolt.DB, func()) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, "test.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	return db, func() { _ = db.Close() }
}

func TestOpen_PreservesV1GraphAndResetsWatermark(t *testing.T) {
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
		if err := eb.Put([]byte("maria rodriguez|acme|co_mentioned"), []byte(`{"from":"maria rodriguez","to":"acme","relation":"co_mentioned","sources":["fact-1"]}`)); err != nil {
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
	if len(nodes) != 1 || nodes[0].ID != "maria rodriguez" {
		t.Fatalf("legacy node was lost: %v", nodes)
	}
	edges, err := g.AllEdges()
	if err != nil || len(edges) != 1 || len(edges[0].Sources) != 1 || edges[0].Sources[0] != "fact-1" ||
		edges[0].PendingReason() == "" {
		t.Fatalf("legacy edge or its unresolved source was lost: %+v, %v", edges, err)
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
