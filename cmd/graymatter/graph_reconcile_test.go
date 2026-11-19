package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

const (
	reconcileTestFrom     = "concept:a"
	reconcileTestTo       = "concept:b"
	reconcileTestRelation = "reconcile"
	reconcileTestKey      = reconcileTestFrom + "|" + reconcileTestTo + "|" + reconcileTestRelation
)

type reconcileTestRun func(...string) (string, error)

func seedReconcileLegacy(t *testing.T, earlier int, partial bool) (string, []memory.Fact) {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	var facts []memory.Fact
	for _, body := range []string{"A supports the relationship.", "B corroborates the relationship."} {
		fact, err := store.PutReturningFact(context.Background(), "graph-reconcile", body)
		if err != nil {
			t.Fatal(err)
		}
		facts = append(facts, fact)
	}
	put := func(b *bolt.Bucket, key string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), raw)
	}
	err = store.DB().Update(func(tx *bolt.Tx) error {
		nodes, err := tx.CreateBucket([]byte("kg_nodes"))
		if err != nil {
			return err
		}
		for _, id := range []string{reconcileTestFrom, reconcileTestTo} {
			if err := put(nodes, id, kg.Node{ID: id, Label: id, EntityType: "concept", Weight: 1}); err != nil {
				return err
			}
		}
		edges, err := tx.CreateBucket([]byte("kg_edges"))
		if err != nil {
			return err
		}
		var first string
		edge := kg.Edge{From: reconcileTestFrom, To: reconcileTestTo, Weight: 1}
		for i := 0; i < earlier; i++ {
			edge.Relation = fmt.Sprintf("earlier-%03d", i)
			key := edge.From + "|" + edge.To + "|" + edge.Relation
			if i == 0 {
				first = key
				if partial {
					edge.EdgeProvenance = &kg.EdgeProvenance{ProvenanceVersion: 1, Reason: "legacy_no_sources"}
				}
			}
			if err := put(edges, key, edge); err != nil {
				return err
			}
			edge.EdgeProvenance = nil
		}
		edge.Relation = reconcileTestRelation
		edge.Sources = []string{facts[0].ID, facts[1].ID}
		if err := put(edges, reconcileTestKey, edge); err != nil {
			return err
		}
		if !partial {
			return nil
		}
		meta, err := tx.CreateBucket([]byte("kg_meta"))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte("id_scheme"), []byte("2")); err != nil {
			return err
		}
		return put(meta, "provenance_migration", map[string]any{
			"version": 1, "after": []byte(first), "examined": 1, "complete": false,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir, facts
}

func inspectReconcileTestEdge(t *testing.T, run reconcileTestRun, relation string) (kg.Edge, kg.ProvenanceReport) {
	t.Helper()
	out, err := run("inspect", reconcileTestFrom, reconcileTestTo, relation)
	if err != nil {
		t.Fatalf("kg inspect: %v\n%s", err, out)
	}
	var view struct {
		Provenance *kg.ProvenanceReport `json:"provenance"`
		Edges      []kg.EdgeInspection  `json:"edges"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil || view.Provenance == nil || len(view.Edges) != 1 {
		t.Fatalf("invalid edge inspection: %v\n%s", err, out)
	}
	return view.Edges[0].Edge, *view.Provenance
}

func writeReconcileTestProof(t *testing.T, edge kg.Edge, facts []memory.Fact) string {
	t.Helper()
	supports := make([]kg.SourceEvidence, 0, len(facts))
	for _, f := range facts {
		supports = append(supports, kg.SourceEvidence{
			Namespace: f.AgentID, FactID: f.ID, Kind: f.Kind,
			TextSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(f.Text))),
		})
	}
	proof, err := json.Marshal(map[string]any{
		"expected": edge, "manual_support": true, "supports": supports,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "proof.json")
	if err := os.WriteFile(path, proof, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func applyFirstReconcileTestProof(t *testing.T, run reconcileTestRun, facts []memory.Fact) {
	t.Helper()
	before, checkpoint := inspectReconcileTestEdge(t, run, reconcileTestRelation)
	if before.EdgeProvenance != nil || checkpoint.MigrationComplete {
		t.Fatalf("fixture did not leave the target edge outside migration: %+v / %+v", before, checkpoint)
	}
	proof := writeReconcileTestProof(t, before, facts)
	if out, err := run("reconcile", proof); err != nil {
		t.Fatalf("first reconciliation failed without an intervening writer: %v\n%s", err, out)
	}
	after, report := inspectReconcileTestEdge(t, run, reconcileTestRelation)
	if after.EdgeProvenance == nil || !after.SourcesComplete || !after.ManualSupport || len(after.Supports) != len(facts) {
		t.Fatalf("reconciliation did not retain exact and manual support: %+v", after)
	}
	if report.MigrationVersion != checkpoint.MigrationVersion ||
		report.MigrationExamined != checkpoint.MigrationExamined ||
		report.MigrationComplete != checkpoint.MigrationComplete {
		t.Fatalf("reconciliation changed migration progress: before %+v / after %+v", checkpoint, report)
	}
	if out, err := run("reconcile", proof); err == nil || !strings.Contains(err.Error(), "changed since inspection") {
		t.Fatalf("stale proof after reconciliation: %v\n%s", err, out)
	}
	if out, err := run("reconcile", writeReconcileTestProof(t, after, facts)); err != nil {
		t.Fatalf("reconciliation with refreshed proof: %v\n%s", err, out)
	}
	replayed, _ := inspectReconcileTestEdge(t, run, reconcileTestRelation)
	if !reflect.DeepEqual(replayed, after) {
		t.Fatalf("fresh proof changed reconciled support: before %+v / after %+v", after, replayed)
	}
}

type reconcileTestStoreState struct {
	tree map[string]doctorTreeEntry
	size int64
	txID int
}

func snapshotReconcileTestStore(t *testing.T, dir string) reconcileTestStoreState {
	t.Helper()
	path := filepath.Join(dir, "gray.db")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var txID int
	err = db.View(func(tx *bolt.Tx) error {
		txID = tx.ID()
		return nil
	})
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatalf("snapshot reconcile database: read=%v close=%v", err, closeErr)
	}

	tree := snapshotDoctorTree(t, dir)
	// On Windows, bbolt's writable mmap truncates gray.db to its current
	// size. Even a rejected write can therefore update this one timestamp.
	entry := tree["gray.db"]
	entry.mtime = time.Time{}
	tree["gray.db"] = entry
	return reconcileTestStoreState{tree: tree, size: info.Size(), txID: txID}
}

func TestGraphReconcileK09LegacyFirst(t *testing.T) {
	for _, auto := range []string{"0", "1"} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("kg=%s/partial=%t", auto, partial), func(t *testing.T) {
				dir, facts := seedReconcileLegacy(t, 2, partial)
				graphProvenanceCLI(t, dir)
				t.Setenv("GRAYMATTER_KG", auto)
				run := func(args ...string) (string, error) {
					return runGraphProvenanceCommand(kgCmd(), args...)
				}
				tree := snapshotDoctorTree(t, dir)
				_, checkpoint := inspectReconcileTestEdge(t, run, reconcileTestRelation)
				wantExamined := 0
				if partial {
					wantExamined = 1
				}
				if checkpoint.MigrationExamined != wantExamined {
					t.Fatalf("wrong migration checkpoint: %+v", checkpoint)
				}
				assertDoctorTreeUnchanged(t, dir, tree)
				sibling, _ := inspectReconcileTestEdge(t, run, "earlier-001")
				beforeStore := snapshotReconcileTestStore(t, dir)
				applyFirstReconcileTestProof(t, run, facts)
				afterStore := snapshotReconcileTestStore(t, dir)
				if afterStore.txID <= beforeStore.txID || afterStore.tree["gray.db"].hash == beforeStore.tree["gray.db"].hash {
					t.Fatal("successful reconciliation did not change the persisted transaction and database bytes")
				}
				after, _ := inspectReconcileTestEdge(t, run, "earlier-001")
				if !reflect.DeepEqual(after, sibling) {
					t.Fatalf("unrelated legacy edge changed: before %+v / after %+v", sibling, after)
				}
			})
		}
	}
}

func TestGraphReconcileK09RejectsStaleOrIncompleteEvidence(t *testing.T) {
	for _, scenario := range []struct{ name, want string }{
		{"changed edge", "changed since inspection"},
		{"changed source", "does not match the inspected kind and text"},
		{"incomplete proof", "remains unverified"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir, facts := seedReconcileLegacy(t, 0, false)
			graphProvenanceCLI(t, dir)
			run := func(args ...string) (string, error) {
				return runGraphProvenanceCommand(kgCmd(), args...)
			}
			edge, _ := inspectReconcileTestEdge(t, run, reconcileTestRelation)
			if scenario.name == "incomplete proof" {
				facts = facts[:1]
			}
			proof := writeReconcileTestProof(t, edge, facts)
			switch scenario.name {
			case "changed edge":
				db, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, &bolt.Options{Timeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				edge.Weight = 0.5
				raw, err := json.Marshal(edge)
				if err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				err = db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte("kg_edges")).Put([]byte(reconcileTestKey), raw)
				})
				if closeErr := db.Close(); err != nil || closeErr != nil {
					t.Fatalf("modify edge: write=%v close=%v", err, closeErr)
				}
			case "changed source":
				store, err := memory.Open(memory.StoreConfig{DataDir: dir})
				if err != nil {
					t.Fatal(err)
				}
				facts[1].Text = "This source changed after the proof was prepared."
				err = store.UpdateFact(facts[1].AgentID, facts[1])
				if closeErr := store.Close(); err != nil || closeErr != nil {
					t.Fatalf("modify source: write=%v close=%v", err, closeErr)
				}
			}
			beforeEdge, beforeReport := inspectReconcileTestEdge(t, run, reconcileTestRelation)
			before := snapshotReconcileTestStore(t, dir)
			if out, err := run("reconcile", proof); err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("expected %q, got %v\n%s", scenario.want, err, out)
			}
			afterEdge, afterReport := inspectReconcileTestEdge(t, run, reconcileTestRelation)
			if !reflect.DeepEqual(beforeEdge, afterEdge) || !reflect.DeepEqual(beforeReport, afterReport) {
				t.Fatalf("rejected reconciliation changed edge or provenance: before %+v / %+v; after %+v / %+v",
					beforeEdge, beforeReport, afterEdge, afterReport)
			}
			after := snapshotReconcileTestStore(t, dir)
			if before.txID != after.txID || before.size != after.size {
				t.Fatalf("rejected reconciliation changed database transaction or size: before %d / %d; after %d / %d",
					before.txID, before.size, after.txID, after.size)
			}
			if !reflect.DeepEqual(before.tree, after.tree) {
				t.Fatalf("rejected reconciliation changed the persisted tree\nbefore: %#v\nafter: %#v", before.tree, after.tree)
			}
		})
	}
}

func TestGraphReconcileK09RequiresExistingDatabase(t *testing.T) {
	proof := writeReconcileTestProof(t, kg.Edge{From: reconcileTestFrom, To: reconcileTestTo}, nil)
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			dir := t.TempDir()
			graphProvenanceCLI(t, dir)
			if empty {
				if err := os.WriteFile(filepath.Join(dir, "gray.db"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotDoctorTree(t, dir)
			if out, err := runGraphProvenanceCommand(kgCmd(), "reconcile", proof); err == nil {
				t.Fatalf("reconciled a missing or empty database: %s", out)
			}
			assertDoctorTreeUnchanged(t, dir, before)
		})
	}
}

func TestGraphReconcileK09DaemonRouting(t *testing.T) {
	bin := buildE2EBinary(t)
	for _, auto := range []string{"0", "1"} {
		for _, active := range []bool{false, true} {
			t.Run(fmt.Sprintf("kg=%s/active=%t", auto, active), func(t *testing.T) {
				t.Setenv("GRAYMATTER_NO_DAEMON", "0")
				t.Setenv("GRAYMATTER_KG", auto)
				t.Setenv("ANTHROPIC_API_KEY", "")
				t.Setenv("VOYAGE_API_KEY", "")
				t.Setenv("OPENAI_API_KEY", "")
				t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://graph-reconcile-test")
				// Startup inventories a batch of 128 earlier edges. The
				// target is still legacy when the running daemon sees it.
				earlier := 2
				if active {
					earlier = 129
				}
				dir, facts := seedReconcileLegacy(t, earlier, false)
				cwd := t.TempDir()
				run := func(args ...string) (string, error) {
					cli := append([]string{"--dir", dir, "--json", "kg"}, args...)
					out, code := runE2E(t, bin, cwd, "", cli...)
					if code != 0 {
						return out, fmt.Errorf("exit %d: %s", code, out)
					}
					return out, nil
				}
				if active {
					process := confidenceStartDaemon(t, bin, dir)
					applyFirstReconcileTestProof(t, run, facts)
					process.stop()
					return
				}
				t.Cleanup(func() {
					if daemon.ReadPIDFile(dir) != 0 {
						stopE2EDaemon(t, bin, cwd, dir)
					}
				})
				applyFirstReconcileTestProof(t, run, facts)
				if daemon.ReadPIDFile(dir) != 0 {
					t.Fatal("cold reconciliation started a daemon before comparing the proof")
				}
				if out, code := runE2E(t, bin, cwd, "", "--dir", dir, "remember", "graph-reconcile", "Start the ordinary store owner."); code != 0 {
					t.Fatalf("ordinary daemon auto-start: exit %d\n%s", code, out)
				}
				edge, report := inspectReconcileTestEdge(t, run, reconcileTestRelation)
				if daemon.ReadPIDFile(dir) == 0 || !report.MigrationComplete ||
					edge.EdgeProvenance == nil || !edge.SourcesComplete || len(edge.Supports) != len(facts) {
					t.Fatalf("ordinary daemon startup did not preserve reconciled support: %+v / %+v", edge, report)
				}
			})
		}
	}
}
