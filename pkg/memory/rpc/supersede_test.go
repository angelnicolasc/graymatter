package rpc

import (
	"context"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Daemon mode is the default: the MCP server, the TUI and the CLI all reach
// the store through this RPC boundary rather than opening bbolt themselves. A
// tombstone that survived an in-process UpdateFact but was dropped in transit
// would leave the fix working in tests and failing in production, so the wire
// gets its own regression test.
func TestSupersedeSurvivesRPCRoundTrip(t *testing.T) {
	dataDir, _ := startServer(t, "test-token")
	c := dialT(t, dataDir)
	defer c.Close()

	ctx := context.Background()
	const (
		dead = "Deployments go through the legacy Jenkins pipeline"
		live = "Deployments go through GitHub Actions"
	)

	if err := c.Put(ctx, "rpc-agent", dead); err != nil {
		t.Fatalf("Put dead: %v", err)
	}
	if err := c.Put(ctx, "rpc-agent", live); err != nil {
		t.Fatalf("Put live: %v", err)
	}

	facts, err := c.List("rpc-agent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var replacementID string
	for _, f := range facts {
		if f.Text == live {
			replacementID = f.ID
		}
	}
	if replacementID == "" {
		t.Fatal("replacement fact not found over RPC")
	}

	for _, f := range facts {
		if f.Text != dead {
			continue
		}
		f.SupersededBy = replacementID
		if err := c.UpdateFact("rpc-agent", f); err != nil {
			t.Fatalf("UpdateFact over RPC: %v", err)
		}
	}

	// The tombstone has to have been carried to the store, not just accepted.
	back, err := c.List("rpc-agent")
	if err != nil {
		t.Fatalf("List after supersede: %v", err)
	}
	var checked bool
	for _, f := range back {
		if f.Text != dead {
			continue
		}
		checked = true
		if f.SupersededBy != replacementID {
			t.Errorf("SupersededBy did not survive the round-trip: got %q, want %q",
				f.SupersededBy, replacementID)
		}
	}
	if !checked {
		t.Fatal("the superseded fact vanished from List; it should be tombstoned, not deleted")
	}

	// And the store the daemon owns must actually exclude it from recall.
	got, err := c.Recall(ctx, "rpc-agent", "deployment pipeline", 8)
	if err != nil {
		t.Fatalf("Recall over RPC: %v", err)
	}
	for _, g := range got {
		if strings.Contains(g, "Jenkins") {
			t.Errorf("superseded fact recalled through the daemon: %v", got)
		}
	}
}

// TestSupersedeMarkerConstantCrossesRPC covers the forget case, whose marker
// is a package constant rather than a fact ID.
func TestSupersedeMarkerConstantCrossesRPC(t *testing.T) {
	dataDir, _ := startServer(t, "test-token")
	c := dialT(t, dataDir)
	defer c.Close()

	ctx := context.Background()
	if err := c.Put(ctx, "marker", "a fact the agent later drops"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	facts, err := c.List("marker")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	facts[0].SupersededBy = memory.SupersededByAgent
	if err := c.UpdateFact("marker", facts[0]); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	got, err := c.Recall(ctx, "marker", "fact", 8)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("forgotten fact still recalled through the daemon: %v", got)
	}
}
