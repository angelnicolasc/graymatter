package daemon

import (
	"context"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestCitationDaemonAdapterPreservesEvidence(t *testing.T) {
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
	if !b.SupportsCitationEvidence() {
		t.Fatal("citation capability not exposed")
	}
	source, quote := "文档:3", "é\r\n中文\t\"literal\""
	facts, err := b.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "deployment assertion", Options: memory.WriteOptions{Source: &source, Quote: &quote}}})
	if err != nil || len(facts) != 1 {
		t.Fatalf("write %+v %v", facts, err)
	}
	result, err := b.SearchScoped(context.Background(), "a", []string{"deployment"}, 8, true, memory.RecallOptions{})
	if err != nil || len(result.Facts) != 1 {
		t.Fatalf("search %+v %v", result, err)
	}
	f := result.Facts[0]
	got, err := b.FetchCitation(context.Background(), "a", f.ID, f.ContentHash)
	if err != nil || got.Evidence == nil || got.Evidence.Source != source || got.Evidence.Quote != quote {
		t.Fatalf("fetch %+v %v", got, err)
	}
	if _, err := b.RecallForHooks(context.Background(), "a", "deployment", 8); err != nil {
		t.Fatal(err)
	}
}
