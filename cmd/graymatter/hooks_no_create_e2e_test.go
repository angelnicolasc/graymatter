package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHooksInstall_NoCreatePersistedCommandE2E(t *testing.T) {
	bin, home := buildE2EBinary(t), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if out, code := runE2E(t, bin, t.TempDir(), "", "hooks", "install", "--scope", "global"); code != 0 {
		t.Fatalf("global install: exit=%d out=%s", code, out)
	}
	command := persistedHookCommand(t, filepath.Join(home, ".claude", "settings.json"), hooksEventPreCompact)
	cleanDir := t.TempDir()
	defer stopE2EDaemon(t, bin, cleanDir, filepath.Join(cleanDir, ".graymatter"))
	if out, code := runPersistedHook(t, command, cleanDir, hookStdin(cleanDir, "")); code != 0 || out != "" {
		t.Fatalf("global persisted hook: exit=%d output=%q", code, out)
	}
	if entries, err := os.ReadDir(cleanDir); err != nil || len(entries) != 0 {
		t.Fatalf("global hook polluted unrelated cwd: entries=%v err=%v", entries, err)
	}
	project := t.TempDir()
	if out, code := runE2E(t, bin, project, "", "hooks", "install"); code != 0 {
		t.Fatalf("project install: exit=%d out=%s", code, out)
	}
	projectCommand := persistedHookCommand(t, filepath.Join(project, ".claude", "settings.json"), hooksEventPreCompact)
	projectStore := filepath.Join(project, ".graymatter")
	defer stopE2EDaemon(t, bin, project, projectStore)
	if out, code := runPersistedHook(t, projectCommand, project, hookStdin(project, "")); code != 0 || out != "" {
		t.Fatalf("project persisted hook: exit=%d output=%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(projectStore, "gray.db")); err != nil {
		t.Fatalf("project first-use creation regressed: %v", err)
	}
}

func stopE2EDaemon(t *testing.T, bin, dir, store string) {
	_, _ = runE2E(t, bin, dir, "", "--dir", store, "daemon", "stop")
	time.Sleep(300 * time.Millisecond)
}

func persistedHookCommand(t *testing.T, path, event string) string {
	t.Helper()
	groups := hookGroups(t, readSettings(t, path), event)
	if len(groups) == 0 {
		t.Fatalf("no managed command for %s in %s", event, path)
	}
	return groupCommands(t, groups[0])[0]
}

func runPersistedHook(t *testing.T, command, dir, stdin string) (string, int) {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Dir, cmd.Stdin = dir, strings.NewReader(stdin)
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(output), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("execute persisted command %q: %v", command, err)
	}
	return string(output), 0
}
