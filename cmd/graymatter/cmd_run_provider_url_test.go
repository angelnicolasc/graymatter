package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	bolt "go.etcd.io/bbolt"
)

func providerURLCLIEnv(home string) []string {
	return append(initCLIEnv(home), "HTTP_PROXY=http://127.0.0.1:0", "HTTPS_PROXY=http://127.0.0.1:0",
		"NO_PROXY=127.0.0.1,localhost")
}

func TestBackgroundRunEnvKeepsPreparedBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.md")
	if err := os.WriteFile(path, []byte(backgroundTestAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost/selected")
	prepared, err := harness.Prepare(harness.RunConfig{AgentFile: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost/changed")
	parent := []string{"ANTHROPIC_BASE_URL=" + os.Getenv("ANTHROPIC_BASE_URL"), "ANTHROPIC_API_KEY=fake-kept",
		"anthropic_base_url=http://localhost/case-variant", "=C:=C:\\fixture", "ANTHROPIC_BASE_URL=http://localhost/duplicate"}
	before := slices.Clone(parent)
	want := []string{"ANTHROPIC_API_KEY=fake-kept", "=C:=C:\\fixture", "ANTHROPIC_BASE_URL=http://localhost/selected/"}
	if runtime.GOOS != "windows" {
		want = slices.Insert(want, 1, "anthropic_base_url=http://localhost/case-variant")
	}
	if got := backgroundRunEnv(parent, prepared.AnthropicBaseURL()); !slices.Equal(got, want) {
		t.Fatalf("child env = %q; want %q", got, want)
	}
	if !slices.Equal(parent, before) || os.Getenv("ANTHROPIC_BASE_URL") != "http://localhost/changed" {
		t.Fatal("child environment composition mutated the input or process environment")
	}
}

func TestBackgroundChildUsesPreparedURLAfterParentEnvironmentChanges(t *testing.T) {
	bin := buildE2EBinary(t)
	root := t.TempDir()
	path := filepath.Join(root, "agent.md")
	if err := os.WriteFile(path, []byte(backgroundTestAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	const sentinel = "Synthetic persisted background response"
	var hitsA, hitsB atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/prepared/v1/messages" || r.Header.Get("x-api-key") != "fake-background-key" {
			t.Errorf("child request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": sentinel}},
		})
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsB.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer b.Close()

	t.Setenv("ANTHROPIC_BASE_URL", a.URL+"/prepared")
	prepared, err := harness.Prepare(harness.RunConfig{AgentFile: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", b.URL+"/unexpected")
	// If the spawner ignores Cmd.Env, it inherits B from this process.
	env := append(providerURLCLIEnv(root), "ANTHROPIC_BASE_URL="+os.Getenv("ANTHROPIC_BASE_URL"), "ANTHROPIC_API_KEY=fake-background-key")
	store := filepath.Join(root, "store")
	const sid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	args := []string{"--dir", store, "--no-daemon", "run", path, "--background", "--max-retries=1"}
	if err := spawnBackground(store, sid, args, backgroundRunEnv(env, prepared.AnthropicBaseURL()), func() (string, error) { return bin, nil }); err != nil {
		t.Fatal(err)
	}
	pid, err := harness.ReadPIDFile(harness.PIDFilePath(store, sid))
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		state, err := child.Wait()
		if err == nil && !state.Success() {
			err = errors.New(state.String())
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			log, _ := os.ReadFile(harness.LogFilePath(store, sid))
			t.Fatalf("background child: %v: %s", err, log)
		}
	case <-time.After(15 * time.Second):
		_ = child.Kill()
		<-done
		t.Fatal("background child did not exit")
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("background requests A/B = %d/%d", hitsA.Load(), hitsB.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	recall := exec.CommandContext(ctx, bin, "--dir", store, "--no-daemon", "recall", "background-test-agent", "Synthetic")
	recall.Env = providerURLCLIEnv(root)
	if out, err := recall.CombinedOutput(); err != nil || !strings.Contains(string(out), sentinel) {
		t.Fatalf("background reply was not persisted: %v: %s", err, out)
	}
}

func TestRunInvalidURLPreflightDoesNotCallStoreOpener(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://dummy:RUN-CANARY@127.0.0.1:0")
	t.Setenv("GRAYMATTER_NO_DAEMON", "")
	path := filepath.Join(t.TempDir(), "agent.md")
	if err := os.WriteFile(path, []byte(backgroundTestAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	oldReopen, oldMode := reopenStore, noDaemon
	t.Cleanup(func() { reopenStore, noDaemon = oldReopen, oldMode })
	noDaemon = false
	reopenStore = func() (cliStore, error) {
		t.Error("ran daemon opener despite rejected URL")
		return nil, errors.New("unexpected daemon opener")
	}
	cmd := runCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{path})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid_provider_base_url") || strings.Contains(err.Error(), "RUN-CANARY") {
		t.Fatalf("run error = %v", err)
	}
}

func TestRunInvalidURLPreflightCLI(t *testing.T) {
	bin := buildE2EBinary(t)
	root := t.TempDir()
	path := filepath.Join(root, "agent.md")
	if err := os.WriteFile(path, []byte(backgroundTestAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(providerURLCLIEnv(root), "ANTHROPIC_BASE_URL=http://dummy:RUN-CANARY@127.0.0.1:0")
	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"foreground-direct", []string{"--no-daemon"}},
		{"foreground-daemon", nil},
		{"background-parent", []string{"--background"}},
		{"background-child", []string{"--background-child", "--session-id=synthetic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := filepath.Join(root, tc.name)
			args := append([]string{"--dir", store, "run", path}, tc.flags...)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, args...)
			cmd.Dir, cmd.Env = root, env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if err == nil || ctx.Err() != nil || stdout.Len() != 0 ||
				!strings.Contains(stderr.String(), "invalid_provider_base_url") || strings.Contains(stderr.String(), "RUN-CANARY") {
				t.Fatalf("run preflight = %v, stdout %q, stderr %q", err, &stdout, &stderr)
			}
			if _, err := os.Stat(store); !os.IsNotExist(err) {
				t.Fatalf("run preflight created store/control files: %v", err)
			}
		})
	}
}

func TestRunInvalidURLPreflightLeavesPendingVectorUntouched(t *testing.T) {
	bin := buildE2EBinary(t)
	root := t.TempDir()
	store := filepath.Join(root, "store")
	path := filepath.Join(root, "agent.md")
	if err := os.WriteFile(path, []byte(backgroundTestAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := func(env []string, args ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"--dir", store, "--no-daemon"}, args...)...)
		cmd.Dir, cmd.Env = root, env
		return cmd.CombinedOutput()
	}
	env := providerURLCLIEnv(root)
	const agent = "synthetic-pending"
	if out, err := cli(env, "remember", agent, "Synthetic pending fact"); err != nil {
		t.Fatalf("seed store: %v: %s", err, out)
	}
	dbPath := filepath.Join(store, "gray.db")
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		id, _ := tx.Bucket([]byte("facts")).Bucket([]byte(agent)).Cursor().First()
		if id == nil {
			return errors.New("seed fact not found")
		}
		pending, err := tx.Bucket([]byte("pending_vector")).CreateBucketIfNotExists([]byte(agent))
		if err != nil {
			return err
		}
		return pending.Put(id, []byte{1})
	})
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("seed pending vector: %v, close: %v", err, closeErr)
	}
	digest := func() [32]byte {
		t.Helper()
		data, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(data)
	}

	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected pending-vector request: %s %s", r.Method, r.URL.Path)
		}
		vector := make([]float32, 1536)
		vector[0] = 1
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": vector}}})
	}))
	defer fixture.Close()
	vectorEnv := append(slices.Clone(env), "OPENAI_API_KEY=fake-pending-key", "OPENAI_BASE_URL="+fixture.URL+"/v1")
	invalidEnv := append(slices.Clone(vectorEnv), "ANTHROPIC_BASE_URL=http://dummy:RUN-CANARY@127.0.0.1:0")
	before := digest()
	if out, err := cli(invalidEnv, "run", path); err == nil || !strings.Contains(string(out), "invalid_provider_base_url") {
		t.Fatalf("invalid run = %v: %s", err, out)
	}
	if after := digest(); after != before || requests.Load() != 0 {
		t.Fatalf("invalid run changed the store or reconciled a pending vector: DB %x -> %x, %d requests", before, after, requests.Load())
	}
	if out, err := cli(vectorEnv, "remember", agent, "Synthetic new control fact"); err != nil || requests.Load() != 2 {
		t.Fatalf("positive control did not reconcile and embed, requests = %d: %v: %s", requests.Load(), err, out)
	}
}
