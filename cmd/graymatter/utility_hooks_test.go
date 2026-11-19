package main

import (
	"context"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// U208 checks the real hook projection, retaining its existing namespace
// budgets rather than changing the hook policy to mimic the MCP formatter.
func TestUtilityHooksIncludeSharedAndExcludeOtherProjects(t *testing.T) {
	mem, err := graymatter.NewWithConfig(lexicalStoreConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	store := &directStore{mem: mem, store: mem.Advanced()}
	ctx := context.Background()
	const project = "Project parcel dispatch uses the west entrance."
	const shared = "Shared parcel dispatch requires a signed manifest."
	const foreign = "FOREIGN parcel dispatch is private to another project."
	for _, row := range []struct{ namespace, text string }{{"parcel", project}, {memory.SharedAgentID, shared}, {"other", foreign}} {
		if err := store.Remember(ctx, row.namespace, row.text); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("native", func(t *testing.T) {
		block, err := hookRecallBlock(store, "parcel", "parcel dispatch", 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(block, project) || !strings.Contains(block, shared) || strings.Contains(block, foreign) {
			t.Fatalf("namespace projection: %q", block)
		}
	})
	t.Run("lexical", func(t *testing.T) {
		block, receipt, err := hookRecallLexicalBlock(ctx, store, "parcel", "parcel dispatch")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(block, project) || !strings.Contains(block, shared) || strings.Contains(block, foreign) {
			t.Fatalf("namespace projection: %q", block)
		}
		if receipt.Project.Selected != 1 || receipt.Shared.Selected != 1 {
			t.Fatalf("unexpected namespace counts: %+v", receipt)
		}
		if receipt.Project.Bytes > hookPacketBytes || receipt.Shared.Bytes > hookPacketBytes {
			t.Fatalf("hook budget changed: %+v", receipt)
		}
	})
}
