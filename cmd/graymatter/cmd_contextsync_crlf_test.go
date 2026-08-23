package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/contextblock"
)

// TestContextSync_CRLFHostFile pins behaviour on Windows-style host files:
// a majority-CRLF AGENTS.md must not turn into a perpetual manual-edit
// warning nor defeat idempotence.
func TestContextSync_CRLFHostFile(t *testing.T) {
	s, path := setupSyncStore(t)
	dir := filepath.Dir(path)

	// Pre-existing CRLF file, like a checkout with autocrlf produces.
	user := "# Project\r\n\r\nNotes.\r\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := syncContextBlock(s, path, "proj", contextblock.DefaultBudgetTokens)
	if err != nil {
		t.Fatal(err)
	}
	if res.ManualEditDetected {
		t.Fatalf("first sync on CRLF file flagged manual edit: %+v", res)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "# Project") {
		t.Fatal("user content lost")
	}
	if _, _, verified, found := contextblock.Parse(content); !found || !verified {
		t.Fatalf("CRLF-written block does not verify (found=%v verified=%v)", found, verified)
	}
	_ = context.Background()
	_ = dir
}
