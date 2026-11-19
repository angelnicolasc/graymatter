package graymatter

import (
	"context"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestCitationQuoteLimitFromConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = EmbeddingKeyword
	cfg.AsyncConsolidate = false
	cfg.MaxQuoteBytes = 12
	mem, err := NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	store := mem.Advanced().(memory.UtilityStore)
	quote := "中文中文"
	if _, err := store.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "a statement", Options: memory.WriteOptions{Quote: &quote}}}); err != nil {
		t.Fatalf("exact 12 UTF-8 bytes rejected: %v", err)
	}
	quote += "!"
	if _, err := store.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "another statement", Options: memory.WriteOptions{Quote: &quote}}}); err == nil || !strings.Contains(err.Error(), "12") {
		t.Fatalf("expected visible configured byte limit, got %v", err)
	}
}
