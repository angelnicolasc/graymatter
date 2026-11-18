package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestPrepareRejectsInvalidURLBeforeOpeningStore(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://dummy:URL-CANARY@127.0.0.1")
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://prepared-run-test")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := filepath.Join(t.TempDir(), "absent")
	cfg := RunConfig{AgentFile: agentFile(t, simpleAgentContent), DataDir: dir, Stdout: io.Discard}
	_, err := Run(context.Background(), cfg)
	var invalid *providerurl.ConfigError
	if !errors.As(err, &invalid) || invalid.Provider != "anthropic" || strings.Contains(err.Error(), "URL-CANARY") {
		t.Fatalf("Run error = %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("preparation opened data dir: %v", err)
	}

	cfg.llmDoer = func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage("Synthetic injected response"), nil
	}
	if result, err := Run(context.Background(), cfg); err != nil || result.FinalReply != "Synthetic injected response" {
		t.Fatalf("Run with an injected provider = %+v, %v", result, err)
	}
}

func TestPreparedRunKeepsAgentAndURLAcrossAttempts(t *testing.T) {
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://prepared-run-test")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "fake-prepared-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	const model = "synthetic-pinned-model"
	const prompt = "Synthetic prepared task"
	const sentinel = "Synthetic provider response"
	var callsA, callsB atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"messages"`
		}
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/gateway/v1/messages" ||
			r.Header.Get("x-api-key") != "fake-prepared-key" || req.Model != model ||
			len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != prompt {
			t.Errorf("unexpected prepared Messages request: %s %s %+v, %v", r.Method, r.URL.Path, req, err)
		}
		if callsA.Add(1) == 1 {
			http.Error(w, "synthetic first-attempt failure", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_synthetic", "type": "message", "role": "assistant", "model": model,
			"content": []map[string]string{{"type": "text", "text": sentinel}},
		})
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callsB.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer b.Close()

	t.Setenv("ANTHROPIC_BASE_URL", a.URL+"/gateway")
	path := agentFile(t, "---\nname: prepared-agent\nmodel: "+model+"\n---\n\n## Task\n{{task}}\n")
	inputs := map[string]string{"task": prompt}
	dir := filepath.Join(t.TempDir(), "absent")
	prepared, err := Prepare(RunConfig{
		AgentFile: path, DataDir: dir, Inputs: inputs, MaxRetries: 2, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Prepare created data dir: %v", err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", b.URL)
	inputs["task"] = "unprepared input"
	if err := os.WriteFile(path, []byte("changed, invalid agent file"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenLocalStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Run(context.Background(), store)
	if err != nil || result.FinalReply != sentinel || result.Attempts != 2 || callsA.Load() != 2 || callsB.Load() != 0 {
		_ = store.Close()
		t.Fatalf("prepared run = %+v, %v; A/B calls = %d/%d", result, err, callsA.Load(), callsB.Load())
	}
	checkpoint, err := store.CheckpointResume("prepared-agent")
	closeErr := store.Close()
	if err != nil || checkpoint == nil || len(checkpoint.Messages) != 2 || checkpoint.Messages[1].Content != sentinel {
		t.Fatalf("prepared checkpoint = %+v, %v", checkpoint, err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	// A fallback store also needs the prepared URL: its own consolidation
	// configuration would otherwise pick up the newly malformed environment.
	t.Setenv("ANTHROPIC_BASE_URL", "https://dummy:URL-CANARY@127.0.0.1")
	result, err = prepared.Run(context.Background(), nil)
	if err != nil || result.FinalReply != sentinel || result.Attempts != 1 || callsA.Load() != 3 || callsB.Load() != 0 {
		t.Fatalf("fallback prepared run = %+v, %v; A/B calls = %d/%d", result, err, callsA.Load(), callsB.Load())
	}
}
