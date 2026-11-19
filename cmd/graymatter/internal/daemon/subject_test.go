package daemon

import (
	"context"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestSubjectDaemonAdapterKeepsReplacementAndCitation(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	b := rememberBackend{mem: mem, AdvancedStore: mem.Advanced()}
	if !b.SupportsSubjectHeads() {
		t.Fatal("subject support hidden")
	}
	ctx := context.Background()
	subject, quote := "retention", "exact\r\n中文"
	old, err := b.PutManyReturningReceipts(ctx, "a", []memory.WriteFact{{Text: "old retention", Options: memory.WriteOptions{Subject: &subject, Quote: &quote}}})
	if err != nil || len(old) != 1 {
		t.Fatalf("old=%+v %v", old, err)
	}
	next, err := b.PutManyReturningReceipts(ctx, "a", []memory.WriteFact{{Text: "new retention", Options: memory.WriteOptions{Subject: &subject}}})
	if err != nil || len(next) != 1 || len(next[0].Replaced) != 1 || next[0].Replaced[0].ID != old[0].Fact.ID {
		t.Fatalf("next=%+v %v", next, err)
	}
	targets, err := b.ResolveFactTargets(ctx, "a", next[0].Fact.ID, "id", false)
	if err != nil || len(targets) != 1 || targets[0].ID != next[0].Fact.ID {
		t.Fatalf("targets=%+v %v", targets, err)
	}
	suggestions, err := b.SuggestSimilarFacts(ctx, "a", "new retention")
	if err != nil || suggestions.Total != 1 {
		t.Fatalf("suggestions=%+v %v", suggestions, err)
	}
	cited, err := b.FetchCitation(ctx, "a", old[0].Fact.ID, memory.NewEvidenceSnapshot(old[0].Fact).ContentHash)
	if err != nil || cited.Evidence == nil || cited.Evidence.Quote != quote {
		t.Fatalf("citation=%+v %v", cited, err)
	}
}
