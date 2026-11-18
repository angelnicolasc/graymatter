package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConsolidateReportSIGPIPE(t *testing.T) {
	bin := buildE2EBinary(t)
	root := t.TempDir()
	fixture := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(fixture.Close)

	command := func(ctx context.Context, store string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, bin, "consolidate", "audit", "--dir", filepath.Join(root, store), "--json")
		cmd.Dir = root
		cmd.Env = []string{
			"HOME=" + root,
			"PATH=" + os.Getenv("PATH"),
			"GRAYMATTER_NO_DAEMON=1",
			"GRAYMATTER_OLLAMA_URL=" + fixture.URL,
			"HTTP_PROXY=" + fixture.URL, "HTTPS_PROXY=" + fixture.URL, "NO_PROXY=127.0.0.1",
		}
		return cmd
	}

	t.Run("open stdout control", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := command(ctx, "open")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil || stderr.Len() != 0 {
			t.Fatalf("open stdout: %v, stdout=%q stderr=%q", err, output, stderr.String())
		}
		var result struct {
			SchemaVersion int `json:"schema_version"`
			Summary       struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"summary"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("stdout is not one JSON result: %v: %q", err, output)
		}
		if result.SchemaVersion != 1 || result.Summary.Status != "skipped" || result.Summary.Reason != "no_facts" ||
			!bytes.Equal(result.Error, []byte("null")) || !strings.HasSuffix(string(output), "\n") || bytes.Count(output, []byte{'\n'}) != 1 {
			t.Fatalf("unexpected report %s", output)
		}
	})

	t.Run("closed stdout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		cmd := command(ctx, "closed")
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = writer, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = writer.Close()
		var exit *exec.ExitError
		if err := cmd.Wait(); !errors.As(err, &exit) || ctx.Err() != nil {
			t.Fatalf("closed stdout did not terminate by signal: %v (context %v), stderr=%q", err, ctx.Err(), stderr.String())
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGPIPE || stderr.Len() != 0 {
			t.Fatalf("closed stdout status=%+v stderr=%q", exit.Sys(), stderr.String())
		}
	})
}
