package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

// captureStdout redirects the process stdout while fn runs and returns what
// was written. printNextSteps writes there directly rather than through
// cobra's output writers.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = old
		_ = r.Close()
	}()

	fn()
	_ = w.Close()
	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(captured)
}

func TestInit_KGFlagWritesSentinelAndSaysSo(t *testing.T) {
	project := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldDataDir := dataDir
	dataDir = filepath.Join(project, ".graymatter")
	t.Cleanup(func() {
		dataDir = oldDataDir
		_ = os.Chdir(oldWd)
	})
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}

	var cobraOut bytes.Buffer
	cmd := initCmd()
	cmd.SilenceUsage = true
	cmd.SetOut(&cobraOut)
	cmd.SetErr(&cobraOut)
	cmd.SetArgs([]string{"--kg", "--skip-instructions", "--only", "claudecode", "--no-path"})

	var execErr error
	stdout := captureStdout(t, func() {
		execErr = cmd.Execute()
	})
	if execErr != nil {
		t.Fatalf("init --kg: %v\n%s%s", execErr, stdout, cobraOut.String())
	}

	if fl := cmd.Flags().Lookup("kg"); fl == nil {
		t.Fatal("kg flag not registered")
	} else if !fl.Changed {
		t.Fatalf("--kg not applied by SetArgs; value=%v args=%v", fl.Value.String(), cmd.Flags().Args())
	}

	sentinel := filepath.Join(dataDir, daemon.KGSentinelFile)
	if _, err := os.Stat(sentinel); err != nil {
		entries, _ := os.ReadDir(dataDir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("sentinel missing after init --kg: %v\ndata dir contents: %v\noutput: %s",
			err, names, cobraOut.String())
	}
	out := stdout + cobraOut.String()
	if !strings.Contains(out, daemon.KGSentinelFile) || !strings.Contains(out, "restart") {
		t.Errorf("next steps should tie KG activation to the restart:\n%s", out)
	}
}
