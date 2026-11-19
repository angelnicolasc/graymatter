package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type confidenceCall func(string, map[string]any) map[string]any

type confidenceProtocolEvent struct {
	Request       json.RawMessage `json:"request"`
	Response      json.RawMessage `json:"response"`
	HTTPStatus    int             `json:"http_status,omitempty"`
	Authenticated *bool           `json:"authenticated,omitempty"`
}

// Capture only synthetic fixture traffic, never authentication headers or
// personal-store contents. The optional output is evidence, not a golden.
func confidenceProtocolRecorder(t *testing.T, name string) func(int, string, any, []byte, int, *bool) {
	t.Helper()
	var events []confidenceProtocolEvent
	outputDir := os.Getenv("GRAYMATTER_CONFIDENCE_PROTOCOL_DIR")
	t.Cleanup(func() {
		if outputDir == "" {
			return
		}
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(events, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outputDir, name+".json"), append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	return func(id int, method string, params any, response []byte, status int, authenticated *bool) {
		if outputDir == "" {
			return
		}
		request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(response) {
			response, _ = json.Marshal(string(response))
		}
		events = append(events, confidenceProtocolEvent{Request: request, Response: append([]byte(nil), response...), HTTPStatus: status, Authenticated: authenticated})
	}
}

func confidenceStructured(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode tool result: %v / %s", err, raw)
	}
	if result["isError"] == true {
		t.Fatalf("tool rejected valid request: %s", raw)
	}
	payload, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("missing structuredContent: %s", raw)
	}
	return payload
}

func confidenceHTTP(t *testing.T, f *issue81Fixture, project, store string, direct bool) (confidenceCall, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	args := []string{"--dir", store, "mcp", "serve", "--tools", "full", "--http", addr, "--token", "confidence-fixture-bearer"}
	if direct {
		args = append(args, "--no-daemon")
	}
	process := exec.CommandContext(ctx, f.bin, args...)
	process.Dir, process.Env = project, f.env
	var stdout, stderr issue81LockedBuffer
	process.Stdout, process.Stderr = &stdout, &stderr
	if err := process.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	var closed sync.Once
	closeProcess := func() { closed.Do(func() { cancel(); _ = process.Wait() }) }
	t.Cleanup(closeProcess)
	client := &http.Client{Timeout: 5 * time.Second}
	session := ""
	id := 0
	record := confidenceProtocolRecorder(t, fmt.Sprintf("http-direct-%v", direct))
	request := func(method string, params any, authenticated bool) ([]byte, int, error) {
		id++
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/mcp", bytes.NewReader(data))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if authenticated {
			req.Header.Set("Authorization", "Bearer confidence-fixture-bearer")
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer response.Body.Close()
		if sid := response.Header.Get("Mcp-Session-Id"); sid != "" {
			session = sid
		}
		body, err := io.ReadAll(response.Body)
		if bytes.Contains(body, []byte("data: ")) {
			for _, line := range bytes.Split(body, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data: ")) {
					body = bytes.TrimPrefix(line, []byte("data: "))
					break
				}
			}
		}
		record(id, method, params, body, response.StatusCode, &authenticated)
		return body, response.StatusCode, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, status, err := request("initialize", map[string]any{}, false)
		if err == nil {
			if status != http.StatusUnauthorized {
				t.Fatalf("unauthenticated HTTP status=%d", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP startup: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	decode := func(method string, params any) []byte {
		body, status, err := request(method, params, true)
		if err != nil || status != http.StatusOK {
			t.Fatalf("HTTP %s status=%d error=%v body=%s", method, status, err, body)
		}
		if bytes.Contains(body, []byte("data: ")) {
			for _, line := range bytes.Split(body, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data: ")) {
					body = bytes.TrimPrefix(line, []byte("data: "))
					break
				}
			}
		}
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  any             `json:"error"`
		}
		if err := json.Unmarshal(body, &response); err != nil || response.Error != nil || len(response.Result) == 0 {
			t.Fatalf("HTTP protocol response: %s / %v", body, err)
		}
		return response.Result
	}
	decode("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "confidence-test", "version": "1"}})
	catalog := decode("tools/list", map[string]any{})
	for _, key := range []string{"confidence", "min_confidence", "confidence_weight", "ranking", "retrieval"} {
		if !bytes.Contains(catalog, []byte(`"`+key+`"`)) {
			t.Fatalf("HTTP catalog lacks %s: %s", key, catalog)
		}
	}
	call := func(name string, args map[string]any) map[string]any {
		return confidenceStructured(t, decode("tools/call", map[string]any{"name": name, "arguments": args}))
	}
	invalid := decode("tools/call", map[string]any{"name": "memory_add", "arguments": map[string]any{"agent_id": "a", "text": "invalid must never persist", "confidence": nil}})
	if !bytes.Contains(invalid, []byte(`"isError":true`)) {
		t.Fatalf("HTTP invalid label accepted: %s", invalid)
	}
	return call, closeProcess
}

func TestConfidenceRealMCPTransportMatrix(t *testing.T) {
	f := newIssue81Fixture(t)
	for _, direct := range []bool{true, false} {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(fmt.Sprintf("%s/direct=%v", transport, direct), func(t *testing.T) {
				project := filepath.Join(f.root, fmt.Sprintf("%s-%v", transport, direct))
				if err := os.MkdirAll(project, 0o755); err != nil {
					t.Fatal(err)
				}
				store := f.registerStore(filepath.Join(project, ".graymatter"))
				var call confidenceCall
				var closeProcess func()
				if transport == "stdio" {
					args := []string{"--dir", store, "mcp", "serve"}
					if direct {
						args = append(args, "--no-daemon")
					}
					record := confidenceProtocolRecorder(t, fmt.Sprintf("stdio-direct-%v", direct))
					m := f.startMCPWithTrace(project, f.bin, args, nil, func(id int, method string, params any, response []byte) {
						record(id, method, params, response, 0, nil)
					})
					id := 2
					call = func(name string, args map[string]any) map[string]any {
						id++
						return confidenceStructured(t, m.request(id, "tools/call", map[string]any{"name": name, "arguments": args}))
					}
					closeProcess = m.close
				} else {
					call, closeProcess = confidenceHTTP(t, f, project, store, direct)
				}
				for _, label := range []string{"verified", "inferred", "unverified"} {
					out := call("memory_add", map[string]any{"agent_id": "a", "text": "gateway decision " + label, "confidence": label})
					if out["confidence"] != label || out["fact_id"] == "" {
						t.Fatalf("write identity/label: %#v", out)
					}
				}
				plain := call("memory_search", map[string]any{"agent_id": "a", "query": "gateway decision", "min_confidence": "verified", "confidence_weight": 0.2})
				if plain["count"] != float64(1) || plain["retrieval"] == nil || plain["facts"].([]any)[0] != "gateway decision verified" {
					t.Fatalf("filtered plain: %#v", plain)
				}
				explain := call("memory_search", map[string]any{"agent_id": "a", "query": "gateway decision", "min_confidence": "verified", "confidence_weight": 0.2, "explain": true})
				receipt := explain["explained"].([]any)[0].(map[string]any)
				if receipt["text"] != plain["facts"].([]any)[0] || receipt["ranking"] == nil {
					t.Fatalf("explain differs: %#v", explain)
				}
				batch := call("memory_search_batch", map[string]any{"agent_id": "a", "queries": []string{"gateway", "decision"}, "min_confidence": "verified", "confidence_weight": 0.0})
				if len(batch["merged"].([]any)) != 1 || batch["retrieval"] == nil {
					t.Fatalf("batch lost confidence: %#v", batch)
				}
				updated := call("memory_reflect", map[string]any{"agent_id": "a", "action": "update", "target": "gateway decision unverified", "text": "gateway corrected"})
				if updated["confidence"] != "unverified" || updated["fact_id"] == "" {
					t.Fatalf("omission promoted confidence: %#v", updated)
				}
				call("memory_reflect", map[string]any{"agent_id": "a", "action": "update", "target": "gateway corrected", "text": "gateway final", "confidence": "verified"})
				call("memory_add", map[string]any{"agent_id": "__shared__", "text": "shared gateway rule", "confidence": "verified"})
				shared := call("memory_search_batch", map[string]any{"agent_id": "__shared__", "queries": []string{"gateway", "rule"}, "min_confidence": "verified", "confidence_weight": 0.0})
				if len(shared["merged"].([]any)) != 1 || shared["merged"].([]any)[0] != "shared gateway rule" {
					t.Fatalf("namespace leak: %#v", shared)
				}
				empty := call("memory_search", map[string]any{"agent_id": "absent", "query": "gateway", "min_confidence": "verified", "confidence_weight": 0.0})
				if empty["count"] != float64(0) || empty["retrieval"] == nil {
					t.Fatalf("empty receipt absent: %#v", empty)
				}
				closeProcess()
				args := []string{"--dir", store, "recall", "a", "gateway", "--explain", "--min-confidence", "verified", "--confidence-weight", "0", "--json"}
				if direct {
					args = append(args, "--no-daemon")
				}
				out := f.run(project, "", args...)
				if out.code != 0 || !strings.Contains(out.stdout, "gateway final") || strings.Contains(out.stdout, "invalid must never persist") {
					t.Fatalf("reopen persistence: %+v", out)
				}
			})
		}
	}
}

func TestConfidenceCLIOptionsScopesAndValidation(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "cli")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	run := func(args ...string) issue81Output {
		return f.run(project, "", append([]string{"--dir", store, "--no-daemon"}, args...)...)
	}
	for _, args := range [][]string{
		{"remember", "a", "gateway old", "--confidence", "unverified", "--json"},
		{"revise", "a", "gateway old", "gateway new", "--json"},
		{"remember", "--shared", "gateway shared", "--confidence", "verified", "--json"},
		{"revise", "--shared", "gateway shared", "gateway shared revised", "--confidence", "verified", "--json"},
	} {
		if out := run(args...); out.code != 0 {
			t.Fatalf("CLI %v: %+v", args, out)
		}
	}
	for _, args := range [][]string{
		{"recall", "a", "gateway", "--explain", "--min-confidence", "unverified", "--confidence-weight", "0.2", "--json"},
		{"recall", "--shared", "gateway", "--min-confidence", "verified", "--confidence-weight", "0", "--json"},
		{"recall", "a", "--query", "gateway", "--query", "rule", "--all", "--min-confidence", "verified", "--confidence-weight", "0", "--json"},
		{"recall", "a", "--query", "gateway", "--shared", "--min-confidence", "verified", "--confidence-weight", "0", "--json"},
	} {
		out := run(args...)
		if out.code != 0 || !strings.Contains(out.stdout, `"retrieval"`) {
			t.Fatalf("CLI recall %v: %+v", args, out)
		}
		if strings.Contains(strings.Join(args, " "), "--min-confidence verified") && strings.Contains(out.stdout, "gateway new") {
			t.Fatalf("filtered CLI leak: %+v", out)
		}
	}
	for _, args := range [][]string{
		{"remember", "a", "invalid", "--confidence", ""},
		{"revise", "a", "gateway new", "invalid", "--confidence", "Verified"},
		{"recall", "a", "--query", "gateway", "--confidence-weight", "NaN"},
		{"recall", "a", "--query", "gateway", "--confidence-weight", "0.51"},
		{"recall", "a", "--query", "gateway", "--min-confidence", ""},
		{"recall", "a", "--query", "gateway", "--explain", "--all"},
	} {
		out := run(args...)
		if out.code == 0 {
			t.Fatalf("invalid CLI options accepted %v: %+v", args, out)
		}
	}
	cold := filepath.Join(f.root, "invalid-cold")
	out := f.run(project, "", "--dir", cold, "recall", "a", "--query", "gateway", "--confidence-weight", "-1")
	if out.code == 0 {
		t.Fatalf("invalid cold request accepted: %+v", out)
	}
	if _, err := os.Lstat(cold); !os.IsNotExist(err) {
		t.Fatalf("validation happened after dispatch: %v", err)
	}
}

func TestConfidenceCLIEmptyResultsRetainPolicy(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "empty-cli")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	for _, flags := range [][]string{
		{"a", "gateway"},
		{"a", "gateway", "--explain"},
		{"a", "--query", "gateway", "--query", "rule"},
		{"a", "--query", "gateway", "--all"},
		{"--shared", "gateway"},
	} {
		for _, asJSON := range []bool{false, true} {
			args := append([]string{"--dir", store, "--no-daemon", "recall"}, flags...)
			args = append(args, "--min-confidence", "verified", "--confidence-weight", "0")
			if asJSON {
				args = append(args, "--json")
			}
			out := f.run(project, "", args...)
			if out.code != 0 {
				t.Fatalf("empty CLI %v: %+v", args, out)
			}
			if !asJSON {
				if !strings.Contains(out.stdout, "Retrieval policy confidence-v1") || !strings.Contains(out.stdout, "suppressed_min_confidence") || !strings.Contains(out.stdout, "No memories found") {
					t.Fatalf("empty text lost policy: %+v", out)
				}
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(out.stdout), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["count"] != float64(0) || payload["retrieval"] == nil {
				t.Fatalf("empty JSON lost policy: %#v", payload)
			}
		}
	}
}
