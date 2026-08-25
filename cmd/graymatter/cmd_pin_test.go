package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
)

// W1 (ADR-010): the CLI pin/unpin commands flip the flag, status reports the
// count, and pinning a missing fact is a loud error rather than silence.
func TestPinCommandAndStatusCount(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	oldDir := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = oldDir })

	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const fact = "ARCHITECTURE DECISION: single-writer write path."
	if err := mem.Remember(context.Background(), "arch", fact); err != nil {
		t.Fatal(err)
	}
	_ = mem.Close()

	runPinCmd(t, "pin", "arch", fact)

	jsonOut = true
	t.Cleanup(func() { jsonOut = false })
	out := runStatusCmd(t)
	var payload struct {
		Pinned int `json:"pinned"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("status json: %v\n%s", err, out)
	}
	if payload.Pinned != 1 {
		t.Errorf("status pinned = %d, want 1\n%s", payload.Pinned, out)
	}

	runPinCmd(t, "unpin", "arch", fact)

	out = runStatusCmd(t)
	payload.Pinned = -1
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Pinned != 0 {
		t.Errorf("status pinned after unpin = %d, want 0", payload.Pinned)
	}

	// Missing fact: loud error, not silence.
	var errBuf bytes.Buffer
	cmd := pinCmd()
	cmd.SetOut(&errBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"arch", "no such fact"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("pin of missing fact should fail loudly; got %v", err)
	}
}

func runPinCmd(t *testing.T, verb, agent, text string) {
	t.Helper()
	var buf bytes.Buffer
	var cmd = pinCmd()
	if verb == "unpin" {
		cmd = unpinCmd()
	}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{agent, text})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s: %v\n%s", verb, err, buf.String())
	}
}
