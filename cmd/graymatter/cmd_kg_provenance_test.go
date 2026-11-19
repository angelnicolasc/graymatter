package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func seedGraphProvenanceInspection(t *testing.T) string {
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
	graph, err := kg.Open(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, relation := range []string{"manual", "legacy"} {
		if err := graph.Link(kg.Edge{From: "concept:a", To: "concept:b", Relation: relation}); err != nil {
			t.Fatal(err)
		}
	}
	// Model an old writer that left an edge with no versioned provenance.
	if err := store.DB().Update(func(tx *bolt.Tx) error {
		edges := tx.Bucket([]byte("kg_edges"))
		key := []byte("concept:a|concept:b|legacy")
		var edge kg.Edge
		if err := json.Unmarshal(edges.Get(key), &edge); err != nil {
			return err
		}
		edge.EdgeProvenance = nil
		data, err := json.Marshal(edge)
		if err != nil {
			return err
		}
		return edges.Put(key, data)
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func graphProvenanceCLI(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	t.Setenv("GRAYMATTER_KG", "0")
	oldDir, oldJSON := dataDir, jsonOut
	dataDir, jsonOut = dir, true
	t.Cleanup(func() { dataDir, jsonOut = oldDir, oldJSON })
}

func runGraphProvenanceCommand(cmd *cobra.Command, args ...string) (string, error) {
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestKGProvenanceK09ReadOnlySurfaces(t *testing.T) {
	dir := seedGraphProvenanceInspection(t)
	graphProvenanceCLI(t, dir)
	t.Chdir(t.TempDir())
	before := snapshotDoctorTree(t, dir)

	const want = "1 complete, 1 incomplete, 1 pending"
	if check := checkKG(dir); check.Status != "ok" || !strings.Contains(check.Detail, want) {
		t.Fatalf("setup doctor graph check = %+v", check)
	}

	out, err := runGraphProvenanceCommand(doctorCmd(), "--graph")
	if err != nil {
		t.Fatalf("doctor --graph: %v\n%s", err, out)
	}
	var doctor struct {
		Edges      int                  `json:"edges"`
		Provenance *kg.ProvenanceReport `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(out), &doctor); err != nil {
		t.Fatalf("doctor graph JSON: %v\n%s", err, out)
	}
	if p := doctor.Provenance; doctor.Edges != 2 || p == nil || p.Examined != 2 ||
		p.Complete != 1 || p.Incomplete != 1 || p.Pending != 1 || len(p.Reasons) != 1 {
		t.Fatalf("doctor graph inventory = %+v, provenance=%+v", doctor, p)
	}
	if _, err := os.Stat("kg-graph.html"); !os.IsNotExist(err) {
		t.Fatalf("doctor --graph wrote implicit HTML: stat err = %v", err)
	}

	out, err = runGraphProvenanceCommand(kgCmd(), "inspect", "concept:a", "concept:b", "legacy")
	if err != nil {
		t.Fatalf("kg inspect: %v\n%s", err, out)
	}
	var inspection struct {
		Edges []struct {
			Edge          kg.Edge `json:"edge"`
			PendingReason string  `json:"pending_reason"`
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(out), &inspection); err != nil || len(inspection.Edges) != 1 ||
		inspection.Edges[0].Edge.Relation != "legacy" || doctor.Provenance.Reasons[inspection.Edges[0].PendingReason] != 1 {
		t.Fatalf("inspect pending edge = %+v, %v\n%s", inspection, err, out)
	}
	assertDoctorTreeUnchanged(t, dir, before)

	// Status also reports manually linked graphs with no fact namespace.
	out, err = runGraphProvenanceCommand(statusCmd())
	if err != nil {
		t.Fatalf("status JSON: %v\n%s", err, out)
	}
	var status struct {
		KG *daemon.KGStateResponse `json:"kg"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil || status.KG == nil ||
		status.KG.Edges != 2 || status.KG.Provenance == nil || status.KG.Provenance.Pending != 1 {
		t.Fatalf("status graph inventory = %+v, %v\n%s", status, err, out)
	}
	jsonOut = false
	out, err = runGraphProvenanceCommand(statusCmd())
	if err != nil || !strings.Contains(out, want) {
		t.Fatalf("status text: %v\n%s", err, out)
	}
}

func TestKGProvenanceK09ReconcileRequiresCurrentEdge(t *testing.T) {
	dir := seedGraphProvenanceInspection(t)
	graphProvenanceCLI(t, dir)

	out, err := runGraphProvenanceCommand(kgCmd(), "inspect", "concept:a", "concept:b", "legacy")
	if err != nil {
		t.Fatalf("kg inspect: %v\n%s", err, out)
	}
	var inspected struct {
		Edges []struct {
			Edge json.RawMessage `json:"edge"`
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(out), &inspected); err != nil || len(inspected.Edges) != 1 {
		t.Fatalf("expected one edge snapshot: %v\n%s", err, out)
	}
	proofPath := filepath.Join(t.TempDir(), "proof.json")
	proof, err := json.Marshal(map[string]any{
		"expected": inspected.Edges[0].Edge, "manual_support": true, "supports": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proofPath, proof, 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := runGraphProvenanceCommand(kgCmd(), "reconcile", proofPath); err != nil {
		t.Fatalf("reconcile legacy manual edge: %v\n%s", err, out)
	}
	if check := checkKG(dir); !strings.Contains(check.Detail, "2 complete, 0 incomplete, 0 pending") {
		t.Fatalf("reconciled graph = %+v", check)
	}
	if out, err := runGraphProvenanceCommand(kgCmd(), "reconcile", proofPath); err == nil || !strings.Contains(err.Error(), "changed since inspection") {
		t.Fatalf("reconciliation replay with stale evidence: %v\n%s", err, out)
	}
}

func TestKGProvenanceK09DoesNotInitializeMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	graphProvenanceCLI(t, dir)
	for _, cmd := range []struct {
		command *cobra.Command
		args    []string
	}{
		{doctorCmd(), []string{"--graph"}},
		{kgCmd(), []string{"inspect"}},
	} {
		if out, err := runGraphProvenanceCommand(cmd.command, cmd.args...); err != nil {
			t.Fatalf("%s %v: %v\n%s", cmd.command.Name(), cmd.args, err, out)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("read-only graph commands initialized %s: %v", dir, err)
	}
}

func TestDirectKGProvenanceMaintenanceWhenAutoOff(t *testing.T) {
	t.Setenv("GRAYMATTER_KG", "0")
	dir := t.TempDir()
	ctx := context.Background()
	remaining := func() memory.Fact {
		store, err := memory.Open(memory.StoreConfig{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		graph, err := kg.Open(store.DB())
		if err != nil {
			t.Fatal(err)
		}
		var facts []memory.Fact
		for _, endpoint := range []string{"concept:queued", "concept:remaining"} {
			fact, err := store.PutReturningFact(ctx, "graph-maintenance", "support for "+endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DB().Update(func(tx *bolt.Tx) error {
				support, err := memory.RegisterGraphSupportTx(tx, fact.AgentID, fact.ID)
				if err != nil {
					return err
				}
				edge := kg.Edge{From: "concept:source", To: endpoint, Relation: "co_mentioned"}
				return graph.LinkDerivedTx(tx, edge, kg.SourceSupport(support))
			}); err != nil {
				t.Fatal(err)
			}
			facts = append(facts, fact)
		}
		if err := store.Delete(facts[0].AgentID, facts[0].ID); err != nil {
			t.Fatal(err)
		}
		report, err := graph.ProvenanceReport()
		if err != nil || report.CleanupPending != 1 {
			t.Fatalf("expected cleanup queued with no graph writer: %+v, %v", report, err)
		}
		return facts[1]
	}()

	direct, err := openDirectStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = direct.Close() }()
	edges, err := direct.KGEdges()
	if err != nil || len(edges) != 1 || edges[0].To != "concept:remaining" {
		t.Fatalf("startup should drain exactly the old receipt: %+v, %v", edges, err)
	}
	state, err := direct.KGState()
	if err != nil || state.AutoPopulate || state.Provenance == nil || state.Provenance.CleanupPending != 0 {
		t.Fatalf("auto-off graph state = %+v, %v", state, err)
	}
	if err := direct.Delete(remaining.AgentID, remaining.ID); err != nil {
		t.Fatal(err)
	}
	state, err = direct.KGState()
	if err != nil || state.Edges != 0 || state.Provenance == nil || state.Provenance.CleanupPending != 0 {
		t.Fatalf("direct deletion left derived support pending: %+v, %v", state, err)
	}
}
