package kg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

func graphSupport(i int) SourceSupport {
	return SourceSupport{Namespace: "agent", FactID: fmt.Sprintf("source-%02d", i), Digest: "generation-1"}
}

func requireOneEdge(t *testing.T, g *Graph) Edge {
	t.Helper()
	edges, err := g.AllEdges()
	if err != nil || len(edges) != 1 {
		t.Fatalf("edges = %+v, error = %v, want one edge", edges, err)
	}
	return edges[0]
}

func TestGraphProvenanceK01SaturationPreservesTraversal(t *testing.T) {
	for _, group := range []int{1, 9, 11} {
		t.Run(fmt.Sprintf("group=%d", group), func(t *testing.T) {
			ctx := context.Background()
			cfg := memory.StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword()}
			store, err := memory.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			db := store.DB()
			g, err := Open(db)
			if err != nil {
				t.Fatal(err)
			}
			facts := make([]memory.Fact, 0, 11)
			for i := 0; i < 10; i++ {
				alias, err := store.PutAlias(ctx, "agent", fmt.Sprintf("broker-%02d", i), []string{"acquiring"})
				if err != nil {
					t.Fatal(err)
				}
				facts = append(facts, alias)
			}
			real, err := store.PutReturningFact(ctx, "agent", "Acquiring processes merchant payments")
			if err != nil {
				t.Fatal(err)
			}
			facts = append(facts, real)

			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			pair := Edge{From: "a", To: "b", Relation: "co_mentioned", CreatedAt: now}
			for start := 0; start < len(facts); start += group {
				for retry := 0; retry < 2; retry++ {
					if err := db.Update(func(tx *bolt.Tx) error {
						for _, fact := range facts[start:min(start+group, len(facts))] {
							source, err := memory.RegisterGraphSupportTx(tx, "agent", fact.ID)
							if err != nil {
								return err
							}
							if err := g.LinkDerivedTx(tx, pair, SourceSupport(source)); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := requireOneEdge(t, g)
			if before.PendingReason() != "sources_truncated" || !before.SourcesTruncated || before.SourcesComplete ||
				len(before.Sources) != 10 || slices.Contains(before.Sources, real.ID) {
				t.Fatalf("receipt overflow = %+v, %+v", before, before.EdgeProvenance)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketKGMeta).Delete(provenanceMigrationKey)
			}); err != nil {
				t.Fatal(err)
			}
			if err := g.MigrateProvenance(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := memory.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			store = reopened
			g, err = Open(store.DB())
			if err != nil {
				t.Fatal(err)
			}
			for cycle := 1; cycle <= 3; cycle++ {
				if err := g.DecayGraphAt(now.Add(time.Duration(cycle)*9000*time.Hour), time.Hour); err != nil {
					t.Fatal(err)
				}
				after := requireOneEdge(t, g)
				nodes, edges, err := g.Neighbors("a", 1)
				if err != nil || len(nodes) != 1 || len(edges) != 1 || !after.SourcesTruncated ||
					len(after.Supports) != 0 || after.ManualSupport {
					t.Fatalf("cycle %d lost the unresolved relationship: %+v, %+v, %+v, %v", cycle, after, nodes, edges, err)
				}
			}
		})
	}
}

func TestGraphProvenanceK02ManualAndLegacyMerge(t *testing.T) {
	for _, order := range []string{"manual-first", "derived-first", "legacy-empty"} {
		t.Run(order, func(t *testing.T) {
			g, cleanup := openTestGraph(t)
			defer cleanup()
			edge := Edge{From: "a", To: "b", Relation: "co_mentioned"}
			source := graphSupport(1)
			if order == "manual-first" {
				if err := g.Link(edge); err != nil {
					t.Fatal(err)
				}
			}
			if order == "legacy-empty" {
				if err := g.LinkLegacy(edge); err != nil {
					t.Fatal(err)
				}
				if got := requireOneEdge(t, g); got.PendingReason() == "" || got.ManualSupport {
					t.Fatalf("empty legacy receipt acquired support: %+v", got.EdgeProvenance)
				}
			}
			if err := g.LinkDerived(edge, source); err != nil {
				t.Fatal(err)
			}
			if order == "derived-first" {
				if err := g.Link(edge); err != nil {
					t.Fatal(err)
				}
			}
			before := requireOneEdge(t, g)
			if len(before.Supports) != 1 || before.ManualSupport != (order != "legacy-empty") ||
				before.SourcesComplete != (order != "legacy-empty") {
				t.Fatalf("merge provenance = %+v", before.EdgeProvenance)
			}
			if err := g.RemoveSupport(source); err != nil {
				t.Fatal(err)
			}
			after := requireOneEdge(t, g)
			if len(after.Sources) != 0 || len(after.Supports) != 0 || after.ManualSupport != before.ManualSupport ||
				after.PendingReason() != before.PendingReason() {
				t.Fatalf("remove changed unrelated support: before=%+v after=%+v", before.EdgeProvenance, after.EdgeProvenance)
			}
		})
	}
}

func TestGraphProvenanceK03K07ExactRemovalPreservesMetadata(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	edge := Edge{From: "a", To: "b", Relation: "co_mentioned", CreatedAt: created, Weight: 0.002}
	old := graphSupport(1)
	current := old
	current.Digest = "generation-2"
	foreign := current
	foreign.Namespace = "other"
	for _, source := range []SourceSupport{old, current, foreign} {
		if err := g.LinkDerived(edge, source); err != nil {
			t.Fatal(err)
		}
	}
	key := []byte(edgeKey(edge.From, edge.To, edge.Relation))
	if err := g.db.Update(func(tx *bolt.Tx) error {
		edges := tx.Bucket(bucketEdges)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(edges.Get(key), &raw); err != nil {
			return err
		}
		raw["extra"] = json.RawMessage(`{"uint":18446744073709551615}`)
		data, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		return edges.Put(key, data)
	}); err != nil {
		t.Fatal(err)
	}

	for _, source := range []SourceSupport{old, old, foreign} {
		if err := g.RemoveSupport(source); err != nil {
			t.Fatal(err)
		}
	}
	remaining := requireOneEdge(t, g)
	if remaining.From != edge.From || remaining.To != edge.To || remaining.Relation != edge.Relation ||
		!remaining.CreatedAt.Equal(created) || remaining.Weight != edge.Weight ||
		!remaining.SourcesComplete || !slices.Equal(remaining.Supports, []SourceSupport{current}) ||
		!slices.Equal(remaining.Sources, []string{current.FactID}) {
		t.Fatalf("cleanup overwrote metadata or a new support: %+v, %+v", remaining, remaining.EdgeProvenance)
	}
	if err := g.db.View(func(tx *bolt.Tx) error {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(tx.Bucket(bucketEdges).Get(key), &raw); err != nil {
			return err
		}
		if string(raw["extra"]) != `{"uint":18446744073709551615}` {
			t.Fatalf("cleanup erased opaque metadata: %s", raw["extra"])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveSupport(current); err != nil {
		t.Fatal(err)
	}
	nodes, edges, err := g.Neighbors("a", 1)
	if err != nil || len(nodes) != 0 || len(edges) != 0 {
		t.Fatalf("removed relationship remained reachable: %v, %v, %v", nodes, edges, err)
	}
	requireNodePresence(t, g, "b", true)
}

func TestGraphProvenanceK05CapacityAndOlderWriter(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()
	edge := Edge{From: "a", To: "b", Relation: "co_mentioned"}
	for i := 0; i < 10; i++ {
		if err := g.LinkDerived(edge, graphSupport(i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if err := g.LinkDerived(edge, graphSupport(i)); err != nil {
			t.Fatal(err)
		}
	}
	complete := requireOneEdge(t, g)
	if !complete.SourcesComplete || complete.SourcesTruncated || len(complete.Supports) != 10 {
		t.Fatalf("exactly ten unique and ten repeated supports = %+v", complete.EdgeProvenance)
	}
	if err := g.LinkDerived(edge, graphSupport(10)); err != nil {
		t.Fatal(err)
	}
	overflow := requireOneEdge(t, g)
	if overflow.SourcesComplete || !overflow.SourcesTruncated || len(overflow.Supports) != 10 {
		t.Fatalf("eleventh distinct source = %+v", overflow.EdgeProvenance)
	}

	// Model a legacy writer that deserializes only the old edge fields.
	if err := g.db.Update(func(tx *bolt.Tx) error {
		raw, err := json.Marshal(struct {
			From     string   `json:"from"`
			To       string   `json:"to"`
			Relation string   `json:"relation"`
			Sources  []string `json:"sources"`
		}{edge.From, edge.To, edge.Relation, overflow.Sources})
		if err != nil {
			return err
		}
		return tx.Bucket(bucketEdges).Put([]byte(edgeKey(edge.From, edge.To, edge.Relation)), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveSupport(graphSupport(0)); err != nil {
		t.Fatal(err)
	}
	downgraded := requireOneEdge(t, g)
	if downgraded.EdgeProvenance != nil || !slices.Equal(downgraded.Sources, overflow.Sources) {
		t.Fatalf("legacy write acquired deletion authority: %+v", downgraded)
	}
	if err := g.LinkDerived(edge, graphSupport(10)); err != nil {
		t.Fatal(err)
	}
	upgraded := requireOneEdge(t, g)
	if upgraded.SourcesComplete || upgraded.PendingReason() == "" || !slices.Equal(upgraded.Sources, overflow.Sources) {
		t.Fatalf("later source displaced older unverified evidence: %+v, %+v", upgraded, upgraded.EdgeProvenance)
	}
}

func TestGraphProvenanceK04K09ResumableInventory(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alias, err := store.PutAlias(context.Background(), "agent", "broker", []string{"acquiring"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Retire("agent", alias); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	g, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < provenanceBatchSize+2; i++ {
		edge := Edge{From: fmt.Sprintf("a-%03d", i), To: "b", Relation: "co_mentioned"}
		switch i {
		case provenanceBatchSize:
			edge.Sources = []string{alias.ID}
		case provenanceBatchSize + 1:
			edge.Sources = []string{"missing-fact"}
		}
		if err := g.LinkLegacy(edge); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketKGMeta).Delete(provenanceMigrationKey)
	}); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort after classification and cursor update")
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := migrateProvenanceTx(tx, 1); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("rollback error = %v", err)
	}
	before, err := g.ProvenanceReport()
	if err != nil || before.MigrationVersion != 0 || before.MigrationExamined != 0 ||
		before.Examined != provenanceBatchSize+2 || before.Reasons["legacy_alias_reference"] != 1 ||
		before.Reasons["missing_source"] != 1 {
		t.Fatalf("inventory after rollback = %+v, %v", before, err)
	}
	g, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	part, err := g.ProvenanceReport()
	if err != nil || part.MigrationComplete || part.MigrationExamined != provenanceBatchSize {
		t.Fatalf("first bounded batch = %+v, %v", part, err)
	}
	g, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	full, err := g.ProvenanceReport()
	if err != nil || !full.MigrationComplete || full.MigrationExamined != full.Examined ||
		full.Pending != full.Incomplete || full.Complete+full.Incomplete != full.Examined ||
		full.Reasons["legacy_alias_reference"] != 1 || full.Reasons["missing_source"] != 1 {
		t.Fatalf("resumed inventory = %+v, %v", full, err)
	}
	if err := g.MigrateProvenance(); err != nil {
		t.Fatal(err)
	}
	repeated, err := g.ProvenanceReport()
	if err != nil || !reflect.DeepEqual(full, repeated) {
		t.Fatalf("repeat changed inventory: %+v, %+v, %v", full, repeated, err)
	}
}

func TestGraphProvenanceK03ExplicitReconciliation(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alias, err := store.PutAlias(context.Background(), "agent", "broker", []string{"acquiring"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := Open(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	edge := Edge{From: "a", To: "b", Relation: "co_mentioned", Sources: []string{alias.ID}}
	if err := g.LinkLegacy(edge); err != nil {
		t.Fatal(err)
	}
	expected := requireOneEdge(t, g)
	evidence := SourceEvidence{Namespace: "agent", FactID: alias.ID, Kind: alias.Kind, TextSHA256: "wrong"}
	if err := g.ReconcileProvenance(expected, false, []SourceEvidence{evidence}); err == nil {
		t.Fatal("invalid evidence resolved a legacy source")
	}
	if got := requireOneEdge(t, g); !reflect.DeepEqual(got, expected) {
		t.Fatalf("failed reconciliation changed edge: %+v", got)
	}
	digest := sha256.Sum256([]byte(alias.Text))
	evidence.TextSHA256 = hex.EncodeToString(digest[:])
	if err := g.ReconcileProvenance(expected, false, []SourceEvidence{evidence}); err != nil {
		t.Fatal(err)
	}
	edges, err := g.AllEdges()
	if err != nil || len(edges) != 0 {
		t.Fatalf("verified alias-only edge remained: %+v, %v", edges, err)
	}
}

func TestGraphProvenanceK03MigrationRemovesOnlyExactAliasSupport(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	alias, err := store.PutAlias(ctx, "agent", "broker", []string{"acquiring"})
	if err != nil {
		t.Fatal(err)
	}
	fact, err := store.PutReturningFact(ctx, "agent", "Acquiring processes merchant payments")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Retire("agent", alias); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	g, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	aliasOnly := Edge{From: "a", To: "b", Relation: "alias_only"}
	mixed := Edge{From: "a", To: "c", Relation: "mixed"}
	var retained SourceSupport
	if err := db.Update(func(tx *bolt.Tx) error {
		aliasSupport, err := memory.RegisterGraphSupportTx(tx, "agent", alias.ID)
		if err != nil {
			return err
		}
		factSupport, err := memory.RegisterGraphSupportTx(tx, "agent", fact.ID)
		if err != nil {
			return err
		}
		retained = SourceSupport(factSupport)
		for _, edge := range []Edge{aliasOnly, mixed} {
			if err := g.LinkDerivedTx(tx, edge, SourceSupport(aliasSupport)); err != nil {
				return err
			}
		}
		if err := g.LinkDerivedTx(tx, mixed, retained); err != nil {
			return err
		}
		return tx.Bucket(bucketKGMeta).Delete(provenanceMigrationKey)
	}); err != nil {
		t.Fatal(err)
	}

	if err := g.MigrateProvenance(); err != nil {
		t.Fatal(err)
	}
	got := requireOneEdge(t, g)
	if got.Relation != mixed.Relation || !got.SourcesComplete || !slices.Equal(got.Supports, []SourceSupport{retained}) {
		t.Fatalf("exact alias cleanup lost or replaced the real support: %+v, %+v", got, got.EdgeProvenance)
	}
	neighbors, edges, err := g.Neighbors("a", 1)
	if err != nil || len(neighbors) != 1 || neighbors[0].ID != "c" || len(edges) != 1 || edges[0].Relation != mixed.Relation {
		t.Fatalf("traversal after migration = %+v, %+v, %v", neighbors, edges, err)
	}
	report, err := g.ProvenanceReport()
	if err != nil || report.MigrationExamined != 2 || report.Complete != 1 || report.Pending != 0 {
		t.Fatalf("post-cleanup inventory = %+v, %v", report, err)
	}
}

func TestGraphProvenanceK06RemovalRereadsConcurrentEdge(t *testing.T) {
	g, cleanup := openTestGraph(t)
	defer cleanup()
	edge := Edge{From: "a", To: "b", Relation: "co_mentioned"}
	old := graphSupport(1)
	next := old
	next.Digest = "generation-2"
	if err := g.LinkDerived(edge, old); err != nil {
		t.Fatal(err)
	}
	updated := make(chan struct{})
	commit := make(chan struct{})
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- g.db.Update(func(tx *bolt.Tx) error {
			err := g.LinkDerivedTx(tx, edge, next)
			close(updated)
			<-commit
			return err
		})
	}()
	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		close(commit)
		t.Fatal("concurrent edge writer did not reach the barrier")
	}
	removeResult := make(chan error, 1)
	go func() { removeResult <- g.RemoveSupport(old) }()
	close(commit)
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	if err := <-removeResult; err != nil {
		t.Fatal(err)
	}
	got := requireOneEdge(t, g)
	if !got.SourcesComplete || !slices.Equal(got.Supports, []SourceSupport{next}) {
		t.Fatalf("cleanup applied a stale edge snapshot: %+v", got.EdgeProvenance)
	}
}

func TestGraphProvenanceK09ReconcileBeforeReverseIndexExists(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fact, err := store.PutReturningFact(context.Background(), "agent", "Acquiring processes merchant payments")
	if err != nil {
		t.Fatal(err)
	}
	g, err := Open(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	edge := Edge{From: "a", To: "b", Relation: "co_mentioned", Sources: []string{fact.ID}}
	if err := g.LinkLegacy(edge); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket(bucketSupportEdges)
	}); err != nil {
		t.Fatal(err)
	}
	g, err = OpenRead(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(fact.Text))
	evidence := SourceEvidence{Namespace: "agent", FactID: fact.ID, Kind: fact.Kind, TextSHA256: hex.EncodeToString(digest[:])}
	if err := g.ReconcileProvenance(requireOneEdge(t, g), false, []SourceEvidence{evidence}); err != nil {
		t.Fatal(err)
	}
	got := requireOneEdge(t, g)
	if !got.SourcesComplete || len(got.Supports) != 1 || got.Supports[0].FactID != fact.ID {
		t.Fatalf("legacy reconciliation did not establish exact provenance: %+v", got.EdgeProvenance)
	}
	if err := g.RemoveSupport(got.Supports[0]); err != nil {
		t.Fatal(err)
	}
	edges, err := g.AllEdges()
	if err != nil || len(edges) != 0 {
		t.Fatalf("reconciliation did not index the new support: %+v, %v", edges, err)
	}
}

func TestGraphProvenanceK06K09UnreadableIdentityPreservesEvidence(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alias, err := store.PutAlias(context.Background(), "agent", "broker", []string{"acquiring"})
	if err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	g, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b"} {
		if err := g.Upsert(Node{ID: id, Label: id, LastSeen: stale}); err != nil {
			t.Fatal(err)
		}
	}
	edge := Edge{From: "a", To: "b", Relation: "alias_only", CreatedAt: stale}
	key := []byte(edgeKey(edge.From, edge.To, edge.Relation))
	var support SourceSupport
	var corrupt []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		source, err := memory.RegisterGraphSupportTx(tx, "agent", alias.ID)
		if err != nil {
			return err
		}
		support = SourceSupport(source)
		if err := g.LinkDerivedTx(tx, edge, support); err != nil {
			return err
		}
		edges := tx.Bucket(bucketEdges)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(edges.Get(key), &payload); err != nil {
			return err
		}
		payload["to"] = json.RawMessage(`"c"`)
		corrupt, err = json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := edges.Put(key, corrupt); err != nil {
			return err
		}
		return tx.Bucket(bucketKGMeta).Delete(provenanceMigrationKey)
	}); err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func() {
		t.Helper()
		if err := db.View(func(tx *bolt.Tx) error {
			if !bytes.Equal(tx.Bucket(bucketEdges).Get(key), corrupt) {
				return errors.New("unreadable edge bytes changed")
			}
			index := tx.Bucket(bucketSupportEdges).Bucket(support.indexKey())
			if index == nil || index.Get(key) == nil {
				return errors.New("unreadable edge receipt was discarded")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		report, err := g.ProvenanceReport()
		if err != nil || report.Examined != 1 || report.Pending != 1 || report.Reasons["unreadable_edge"] != 1 {
			t.Fatalf("unreadable edge inventory = %+v, %v", report, err)
		}
	}
	assertUnchanged()
	if err := g.MigrateProvenance(); err != nil {
		t.Fatal(err)
	}
	assertUnchanged()
	if err := g.RemoveSupport(support); err != nil {
		t.Fatal(err)
	}
	assertUnchanged()
	if err := g.DecayGraphAt(stale.Add(365*24*time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	assertUnchanged()
	requireNodePresence(t, g, "a", true)
	requireNodePresence(t, g, "b", true)
}
