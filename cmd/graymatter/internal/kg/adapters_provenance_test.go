package kg

import (
	"slices"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

func TestGraphAdapterExtractionAtomicRollbackAndExactCleanup(t *testing.T) {
	graph, closeGraph := openTestGraph(t)
	defer closeGraph()
	adapter := NewGraphAdapter(graph)
	source := memory.GraphSupport{Namespace: "first", FactID: "fact", Digest: strings.Repeat("a", 64)}
	refs := []memory.EntityRef{
		{ID: "person:casey jones", Label: "Casey Jones", EntityType: "person"},
		{ID: "organization:keystone labs", Label: "Keystone Labs", EntityType: "organization"},
	}
	link := memory.EntityLink{From: refs[0].ID, To: refs[1].ID, Relation: "co_mentioned"}
	broken := []memory.EntityLink{link, {From: link.From, Relation: link.Relation}}
	if err := adapter.WriteGraphExtraction(refs, broken, source); err == nil {
		t.Fatal("invalid second edge unexpectedly committed")
	}
	nodes, err := graph.AllNodes()
	if err != nil || len(nodes) != 0 {
		t.Fatalf("nodes after failed extraction = %+v, %v", nodes, err)
	}
	edges, err := graph.AllEdges()
	if err != nil || len(edges) != 0 {
		t.Fatalf("edges after failed extraction = %+v, %v", edges, err)
	}

	newVersion := source
	newVersion.Digest = strings.Repeat("b", 64)
	otherNamespace := source
	otherNamespace.Namespace = "second"
	for _, receipt := range []memory.GraphSupport{source, source, newVersion, otherNamespace} {
		if err := adapter.WriteGraphExtraction(refs, []memory.EntityLink{link}, receipt); err != nil {
			t.Fatalf("extract %+v: %v", receipt, err)
		}
	}
	edges, err = graph.AllEdges()
	if err != nil || len(edges) != 1 {
		t.Fatalf("edges after retry = %+v, %v", edges, err)
	}
	edge := edges[0]
	if edge.EdgeProvenance == nil || !edge.SourcesComplete || edge.ManualSupport || len(edge.Supports) != 3 {
		t.Fatalf("edge provenance after retry = %+v", edge.EdgeProvenance)
	}
	for _, receipt := range []memory.GraphSupport{source, newVersion, otherNamespace} {
		want := SourceSupport{Namespace: receipt.Namespace, FactID: receipt.FactID, Digest: receipt.Digest}
		if !slices.Contains(edge.Supports, want) {
			t.Errorf("missing receipt %+v from %+v", want, edge.Supports)
		}
	}
	nodes, err = graph.AllNodes()
	if err != nil || len(nodes) != len(refs) {
		t.Fatalf("typed nodes after retry = %+v, %v", nodes, err)
	}
	for _, node := range nodes {
		if node.Label == node.ID || node.EntityType == "unknown" {
			t.Fatalf("extraction used placeholder metadata: %+v", node)
		}
	}
	for _, receipt := range []memory.GraphSupport{source, source, newVersion} {
		if err := adapter.RemoveGraphSupport(receipt); err != nil {
			t.Fatalf("remove %+v: %v", receipt, err)
		}
		nodes, edges, err := graph.Neighbors(link.From, 1)
		if err != nil || len(nodes) != 1 || len(edges) != 1 {
			t.Fatalf("neighbors after removing %+v = %+v / %+v, %v", receipt, nodes, edges, err)
		}
	}
	if err := adapter.RemoveGraphSupport(otherNamespace); err != nil {
		t.Fatal(err)
	}
	nodes, edges, err = graph.Neighbors(link.From, 1)
	if err != nil || len(nodes) != 0 || len(edges) != 0 {
		t.Fatalf("neighbors after removing final receipt = %+v / %+v, %v", nodes, edges, err)
	}
}

func TestGraphAdapterRejectsUnownedAndReadOnlyTransactions(t *testing.T) {
	graph, closeGraph := openTestGraph(t)
	defer closeGraph()
	foreign, closeForeign := openTestGraph(t)
	defer closeForeign()
	adapter := NewGraphAdapter(graph)
	for _, fixture := range []struct {
		run  func(func(*bolt.Tx) error) error
		owns bool
	}{
		{foreign.db.Update, false},
		{graph.db.View, true},
	} {
		if err := fixture.run(func(tx *bolt.Tx) error {
			if adapter.OwnsGraphTx(tx) != fixture.owns {
				t.Errorf("OwnsGraphTx disagrees with fixture: %+v", fixture)
			}
			if err := adapter.WriteGraphExtractionTx(tx, nil, nil, memory.GraphSupport{}); err == nil {
				t.Error("extraction accepted a non-writable or foreign transaction")
			}
			if err := adapter.RemoveGraphSupportTx(tx, memory.GraphSupport{}); err == nil {
				t.Error("cleanup accepted a non-writable or foreign transaction")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGraphAdapterLegacyWriteDoesNotDeclareManualSupport(t *testing.T) {
	graph, closeGraph := openTestGraph(t)
	defer closeGraph()
	adapter := NewGraphAdapter(graph)
	link := memory.EntityLink{From: "person:casey jones", To: "organization:keystone labs", Relation: "co_mentioned"}
	if err := adapter.LinkEdges([]memory.EntityLink{link}, ""); err != nil {
		t.Fatal(err)
	}
	edges, err := graph.AllEdges()
	if err != nil || len(edges) != 1 {
		t.Fatalf("legacy edges = %+v, %v", edges, err)
	}
	if p := edges[0].EdgeProvenance; p != nil && (p.ManualSupport || p.SourcesComplete) {
		t.Fatalf("legacy writer invented provenance: %+v", p)
	}
	if err := adapter.LinkNodes(link.From, link.To, link.Relation); err != nil {
		t.Fatal(err)
	}
	edges, err = graph.AllEdges()
	if err != nil || len(edges) != 1 || edges[0].EdgeProvenance == nil || !edges[0].ManualSupport || edges[0].SourcesComplete {
		t.Fatalf("manual addition must preserve legacy incompleteness: %+v, %v", edges, err)
	}
}

func TestGraphAdapterIdentitySurvivesReopenAndSeparatesDatabases(t *testing.T) {
	dir := t.TempDir()
	db, closeFirst := openRawGraph(t, dir)
	defer closeFirst()
	graph, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewGraphAdapter(graph)
	identity, err := adapter.GraphIdentity()
	if err != nil || identity == "" {
		t.Fatalf("graph identity = %q, %v", identity, err)
	}
	readTxID := func() int {
		t.Helper()
		var id int
		if err := db.View(func(tx *bolt.Tx) error {
			id = tx.ID()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before := readTxID()
	if again, err := adapter.GraphIdentity(); err != nil || again != identity {
		t.Fatalf("graph identity on repeat = %q, %v", again, err)
	}
	if after := readTxID(); before != after {
		t.Fatalf("reading an established graph identity committed a transaction: %d to %d", before, after)
	}

	closeFirst()
	reopened, closeReopened := openRawGraph(t, dir)
	defer closeReopened()
	graph, err = Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := NewGraphAdapter(graph).GraphIdentity(); err != nil || again != identity {
		t.Fatalf("graph identity after reopen = %q, %v", again, err)
	}

	other, closeOther := openTestGraph(t)
	defer closeOther()
	if distinct, err := NewGraphAdapter(other).GraphIdentity(); err != nil || distinct == "" || distinct == identity {
		t.Fatalf("independent graph identity = %q, %v", distinct, err)
	}
}
