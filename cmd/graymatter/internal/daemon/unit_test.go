package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func TestReadPIDFile(t *testing.T) {
	dir := t.TempDir()

	if pid := ReadPIDFile(dir); pid != 0 {
		t.Errorf("ReadPIDFile on empty dir = %d, want 0", pid)
	}

	if err := os.WriteFile(rpc.PIDFilePath(dir), []byte("4321\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid := ReadPIDFile(dir); pid != 4321 {
		t.Errorf("ReadPIDFile = %d, want 4321", pid)
	}

	// Garbage pid file reads as 0, not a panic.
	if err := os.WriteFile(rpc.PIDFilePath(dir), []byte("not-a-number"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid := ReadPIDFile(dir); pid != 0 {
		t.Errorf("ReadPIDFile on garbage = %d, want 0", pid)
	}
}

func TestIdleExitLabel(t *testing.T) {
	if got := idleExitLabel(0); got != "disabled" {
		t.Errorf("idleExitLabel(0) = %q, want disabled", got)
	}
	if got := idleExitLabel(0 - 1); got != "disabled" {
		t.Errorf("idleExitLabel(negative) = %q, want disabled", got)
	}
	if got := idleExitLabel(2_000_000_000); got == "" || got == "disabled" {
		t.Errorf("idleExitLabel(2s) = %q, want a duration", got)
	}
}

func TestDaemonLogTail(t *testing.T) {
	dir := t.TempDir()

	// Missing log → empty tail.
	if got := daemonLogTail(dir); got != "" {
		t.Errorf("daemonLogTail on missing log = %q, want empty", got)
	}

	lines := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n"
	if err := os.WriteFile(filepath.Join(dir, "daemon.log"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	got := daemonLogTail(dir)
	if got == "" {
		t.Fatal("daemonLogTail returned empty for a non-empty log")
	}
	// Keeps only the last few lines.
	if want := "l8"; !strings.Contains(got, want) {
		t.Errorf("tail %q missing last line %q", got, want)
	}
	if notWant := "l1"; strings.Contains(got, notWant) {
		t.Errorf("tail %q should not include the oldest line %q", got, notWant)
	}
}
