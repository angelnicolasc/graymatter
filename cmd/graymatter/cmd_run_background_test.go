package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
)

const backgroundTestAgent = `---
name: background-test-agent
model: claude-opus-4-6
---

## System Prompt
You are a test agent.

## Task
Wait for the test server.
`

func runBackgroundCLI(bin, dir string, env, args []string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func printedBackgroundSessionID(output string) string {
	const (
		prefix = "Session "
		suffix = " started in background."
	)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
			return strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix)
		}
	}
	return ""
}

func backgroundOutputField(output, prefix string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func TestRunBackgroundPersistsPrintedSessionAndSupportsControl(t *testing.T) {
	bin := buildE2EBinary(t)
	projectDir := t.TempDir()
	storeDir := filepath.Join(projectDir, ".graymatter")
	agentFile := filepath.Join(projectDir, "agent.md")
	if err := os.WriteFile(agentFile, []byte(backgroundTestAgent), 0o600); err != nil {
		t.Fatalf("write agent: %v", err)
	}

	requestBlocked := make(chan struct{})
	requestDone := make(chan struct{})
	releaseRequest := make(chan struct{})
	var requestCount atomic.Int32
	var blockedOnce, doneOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			http.Error(w, "intentional first-attempt failure", http.StatusBadRequest)
			return
		}
		blockedOnce.Do(func() { close(requestBlocked) })
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(w)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				doneOnce.Do(func() { close(requestDone) })
				return
			case <-releaseRequest:
				return
			case <-ticker.C:
				_, writeErr := w.Write([]byte(" "))
				if writeErr == nil {
					writeErr = controller.Flush()
				}
				if writeErr != nil {
					doneOnce.Do(func() { close(requestDone) })
					return
				}
			}
		}
	}))

	env := []string{
		"ANTHROPIC_API_KEY=background-e2e",
		"ANTHROPIC_AUTH_TOKEN=",
		"ANTHROPIC_BASE_URL=" + server.URL,
		"OPENAI_API_KEY=",
		"VOYAGE_API_KEY=",
		"GRAYMATTER_OLLAMA_URL=disabled://background-session-e2e",
		"GRAYMATTER_NO_DAEMON=",
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"all_proxy=",
		"NO_PROXY=127.0.0.1,localhost",
		"PATH=",
	}
	printedID := ""
	backgroundPID := 0
	t.Cleanup(func() {
		if printedID != "" {
			_, _ = runBackgroundCLI(bin, projectDir, env,
				[]string{"--dir", storeDir, "sessions", "kill", printedID})
		}
		select {
		case <-requestDone:
		default:
			if backgroundPID > 0 {
				if process, err := os.FindProcess(backgroundPID); err == nil {
					_ = process.Kill()
				}
			}
		}
		close(releaseRequest)
		server.Close()
		daemonPID := daemon.ReadPIDFile(storeDir)
		_, _ = runBackgroundCLI(bin, projectDir, env,
			[]string{"--dir", storeDir, "daemon", "stop"})
		deadline := time.Now().Add(5 * time.Second)
		for daemon.ReadPIDFile(storeDir) != 0 && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
		if daemon.ReadPIDFile(storeDir) != 0 && daemonPID > 0 {
			if process, err := os.FindProcess(daemonPID); err == nil {
				_ = process.Kill()
			}
		}
	})

	out, err := runBackgroundCLI(bin, projectDir, env,
		[]string{"--dir", storeDir, "run", agentFile, "--background", "--max-retries", "2"})
	if err != nil {
		t.Fatalf("run --background: %v\n%s", err, out)
	}
	printedID = printedBackgroundSessionID(out)
	if printedID == "" {
		t.Fatalf("background output has no session ID:\n%s", out)
	}
	backgroundPID, err = strconv.Atoi(backgroundOutputField(out, "PID:"))
	if err != nil || backgroundPID <= 0 {
		t.Fatalf("background output has no valid PID:\n%s", out)
	}
	printedLog := backgroundOutputField(out, "Log:")
	if printedLog == "" {
		t.Fatalf("background output has no log path:\n%s", out)
	}

	select {
	case <-requestBlocked:
	case <-time.After(10 * time.Second):
		t.Fatal("background child did not reach the blocking second attempt")
	}

	listOut, err := runBackgroundCLI(bin, projectDir, env,
		[]string{"--dir", storeDir, "--json", "sessions", "list"})
	if err != nil {
		t.Fatalf("sessions list: %v\n%s", err, listOut)
	}
	var sessions []harness.HarnessSession
	if err := json.Unmarshal([]byte(listOut), &sessions); err != nil {
		t.Fatalf("decode sessions list: %v\n%s", err, listOut)
	}
	var persisted *harness.HarnessSession
	for i := range sessions {
		if sessions[i].ID == printedID {
			persisted = &sessions[i]
			break
		}
	}
	if persisted == nil {
		t.Fatalf("printed session %q was not persisted: %+v", printedID, sessions)
	}
	if persisted.PID <= 0 {
		t.Fatalf("background session has no PID: %+v", *persisted)
	}
	if persisted.PID != backgroundPID {
		t.Fatalf("persisted PID = %d, printed PID = %d", persisted.PID, backgroundPID)
	}
	expectedLog := harness.LogFilePath(storeDir, printedID)
	if filepath.Clean(printedLog) != filepath.Clean(expectedLog) {
		t.Fatalf("printed log file = %q, want %q", printedLog, expectedLog)
	}
	if filepath.Clean(persisted.LogFile) != filepath.Clean(expectedLog) {
		t.Fatalf("log file = %q, want %q", persisted.LogFile, expectedLog)
	}
	if pid, err := harness.ReadPIDFile(harness.PIDFilePath(storeDir, printedID)); err != nil {
		t.Fatalf("read PID file: %v", err)
	} else if pid != persisted.PID {
		t.Fatalf("PID file = %d, persisted PID = %d", pid, persisted.PID)
	}

	logsOut, err := runBackgroundCLI(bin, projectDir, env,
		[]string{"--dir", storeDir, "sessions", "logs", printedID})
	if err != nil {
		t.Fatalf("sessions logs: %v\n%s", err, logsOut)
	}
	if !strings.Contains(logsOut, "attempt 1/2 failed") {
		t.Fatalf("sessions logs omitted child diagnostics:\n%s", logsOut)
	}
	if killOut, err := runBackgroundCLI(bin, projectDir, env,
		[]string{"--dir", storeDir, "sessions", "kill", printedID}); err != nil {
		t.Fatalf("sessions kill: %v\n%s", err, killOut)
	}

	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sessions kill returned but the background process stayed connected")
	}

	listOut, err = runBackgroundCLI(bin, projectDir, env,
		[]string{"--dir", storeDir, "--json", "sessions", "list"})
	if err != nil {
		t.Fatalf("sessions list after kill: %v\n%s", err, listOut)
	}
	sessions = nil
	if err := json.Unmarshal([]byte(listOut), &sessions); err != nil {
		t.Fatalf("decode sessions after kill: %v\n%s", err, listOut)
	}
	for _, session := range sessions {
		if session.ID == printedID && session.Status == "killed" {
			return
		}
	}
	t.Fatalf("session %q was not marked killed: %+v", printedID, sessions)
}
