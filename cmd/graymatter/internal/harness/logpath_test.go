package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReadSessionLog_RefusesPathsOutsideLogsDir is the H-14 regression test.
// The log path is read back out of bbolt, so anything that can write a session
// record turned `graymatter sessions logs` into an arbitrary-file reader with
// the user's own permissions.
func TestReadSessionLog_RefusesPathsOutsideLogsDir(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, ".graymatter")
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	// A file the user can read but this command has no business printing.
	secret := filepath.Join(base, "secrets.txt")
	if err := os.WriteFile(secret, []byte("ssh private key"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	paths := []string{
		secret,                                          // absolute, outside
		filepath.Join(dataDir, "logs", "..", "gray.db"), // traversal back out
		filepath.Join(dataDir, "logs", "..", "..", "secrets.txt"),
		"../../secrets.txt",
		"..\\..\\secrets.txt",
		filepath.Join(dataDir, "logs"), // the directory itself
		"",
	}
	if runtime.GOOS != "windows" {
		paths = append(paths, "/etc/passwd")
	}

	for _, p := range paths {
		data, err := ReadSessionLog(dataDir, p)
		if err == nil {
			t.Errorf("ReadSessionLog(%q) returned %d bytes; it must refuse", p, len(data))
		}
		if strings.Contains(string(data), "ssh private key") {
			t.Errorf("ReadSessionLog(%q) leaked the file contents", p)
		}
	}
}

// TestReadSessionLog_ReadsRealLogs guards the happy path: the check must not
// break the command it protects.
func TestReadSessionLog_ReadsRealLogs(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".graymatter")
	logs := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	want := "agent output line 1\nagent output line 2\n"
	path := LogFilePath(dataDir, "01SESSIONID")
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := ReadSessionLog(dataDir, path)
	if err != nil {
		t.Fatalf("ReadSessionLog: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A relative path that still resolves inside logs/ is fine — the check is
	// about where it lands, not how it is spelled.
	rel, err := filepath.Rel(mustGetwd(t), path)
	if err == nil {
		if got, err := ReadSessionLog(dataDir, rel); err != nil {
			t.Errorf("relative path inside logs/ was refused: %v", err)
		} else if string(got) != want {
			t.Errorf("relative read got %q, want %q", got, want)
		}
	}
}

// TestReadSessionLog_MissingFileReportsCleanly — a session whose log was
// rotated away should say so, not look like a containment failure.
func TestReadSessionLog_MissingFileReportsCleanly(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".graymatter")
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	_, err := ReadSessionLog(dataDir, LogFilePath(dataDir, "01GONE"))
	if err == nil {
		t.Fatal("reading a missing log returned nil error")
	}
	if strings.Contains(err.Error(), "refusing") {
		t.Errorf("a missing file was reported as a containment failure: %v", err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}
