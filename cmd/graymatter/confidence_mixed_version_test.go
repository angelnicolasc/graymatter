package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	bolt "go.etcd.io/bbolt"
)

// This is an actual previous implementation, not an index-version simulator.
// It is the published main revision audited before confidence remediation.
const confidencePreviousRevision = "bb71971419070c07d36ad91eb1e3a5a9302d8c6a"

func confidencePreviousBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a pinned previous binary and runs daemon upgrade/downgrade")
	}
	binary, err := cliE2EBinaries.get(confidencePreviousRevision, buildConfidencePreviousBinary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("previous source archive sha256=%s", binary.archiveSHA256)
	bin := filepath.Join(t.TempDir(), filepath.Base(binary.path))
	issue81CopyExecutable(t, binary.path, bin)
	return bin
}

func buildConfidencePreviousBinary(env e2eBuildEnvironment, dir string) (e2eBuiltBinary, error) {
	archive, err := env.command("git", "archive", "--format=tar", confidencePreviousRevision)
	if err != nil {
		return e2eBuiltBinary{}, err
	}
	archive.Dir = filepath.Join(env.cwd, "..", "..")
	data, err := archive.Output()
	if err != nil {
		return e2eBuiltBinary{}, fmt.Errorf("archive pinned baseline %s: %w", confidencePreviousRevision, err)
	}
	source := filepath.Join(dir, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		return e2eBuiltBinary{}, err
	}
	// Go resolves the child working directory before matching workspace modules.
	// Resolve aliases such as macOS /var -> /private/var so GOWORK and cwd use
	// the same source path when building the archived nested CLI module.
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return e2eBuiltBinary{}, fmt.Errorf("resolve previous source directory: %w", err)
	}
	archiveHash := sha256.Sum256(data)
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return e2eBuiltBinary{}, err
		}
		path := filepath.Join(source, filepath.FromSlash(header.Name))
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return e2eBuiltBinary{}, fmt.Errorf("archive escapes fixture: %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return e2eBuiltBinary{}, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return e2eBuiltBinary{}, err
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return e2eBuiltBinary{}, err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return e2eBuiltBinary{}, copyErr
			}
			if closeErr != nil {
				return e2eBuiltBinary{}, closeErr
			}
		}
	}
	name := "graymatter-previous"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)
	build, err := env.command("go", "build", "-trimpath", "-o", bin, "./cmd/graymatter")
	if err != nil {
		return e2eBuiltBinary{}, err
	}
	build.Dir = source
	build.Env = issue81Env(env.env, map[string]string{"GOWORK": filepath.Join(source, "go.work")})
	if output, err := build.CombinedOutput(); err != nil {
		return e2eBuiltBinary{}, fmt.Errorf("build previous: %w\n%s", err, output)
	}
	return e2eBuiltBinary{path: bin, archiveSHA256: fmt.Sprintf("%x", archiveHash)}, nil
}

func confidenceBinaryChecksum(t *testing.T, binary string) string {
	t.Helper()
	file, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type confidenceDaemonProcess struct {
	t       *testing.T
	dir     string
	cmd     *exec.Cmd
	done    chan error
	stopped bool
}

func confidenceStartDaemon(t *testing.T, binary, dir string) *confidenceDaemonProcess {
	t.Helper()
	log, err := os.Create(filepath.Join(dir, "test-daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "daemon", "run", "--dir", dir, "--idle-exit", "0s")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	_ = log.Close()
	p := &confidenceDaemonProcess{t: t, dir: dir, cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !p.stopped {
			_ = cmd.Process.Kill()
			<-p.done
			p.stopped = true
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		client, err := daemon.ConnectNoSpawn(dir)
		if err == nil {
			_ = client.Close()
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	output, _ := os.ReadFile(filepath.Join(dir, "test-daemon.log"))
	t.Fatalf("daemon startup failed: %s", output)
	return nil
}

func (p *confidenceDaemonProcess) stop() {
	p.t.Helper()
	client, err := daemon.ConnectNoSpawn(p.dir)
	if err != nil {
		p.t.Fatal(err)
	}
	err = client.Shutdown()
	_ = client.Close()
	if err != nil {
		p.t.Fatal(err)
	}
	select {
	case err := <-p.done:
		p.stopped = true
		if err != nil {
			p.t.Fatalf("daemon exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		p.t.Fatal("daemon did not exit after Shutdown")
	}
}

func confidenceIndexVersion(t *testing.T, dir, agent string) int {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state struct {
		Version int `json:"version"`
	}
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("idx_meta"))
		if meta == nil {
			return errors.New("index metadata absent")
		}
		return json.Unmarshal(meta.Get([]byte(agent)), &state)
	}); err != nil {
		t.Fatal(err)
	}
	return state.Version
}

func TestConfidenceRealBinaryUpgradeDowngradeAndReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("runs pinned previous/current binary daemons")
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "VOYAGE_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://confidence-mixed-version")
	t.Setenv("GRAYMATTER_KG", "0")
	t.Setenv("GRAYMATTER_NO_DAEMON", "0")
	previous := confidencePreviousBinary(t)
	current := buildE2EBinary(t)
	t.Logf("previous source=%s sha256=%s", confidencePreviousRevision, confidenceBinaryChecksum(t, previous))
	t.Logf("current binary sha256=%s OS=%s Go=%s", confidenceBinaryChecksum(t, current), runtime.GOOS, runtime.Version())
	dir := t.TempDir()
	ctx := context.Background()
	verified, unverified := "verified", "unverified"
	weight := 0.3
	opts := memory.RecallOptions{MinConfidence: &verified, ConfidenceWeight: &weight}
	oldProcess := confidenceStartDaemon(t, previous, dir)
	initial, err := daemon.ConnectNoSpawn(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Seed through the old wire protocol before exercising the upgraded client.
	if err := initial.CallService(rpc.ServiceName, "Put", &rpc.PutRequest{AgentID: "a", Text: "legacy database policy"}, &rpc.PutResponse{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := initial.Recall(ctx, "a", "database", 8); err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{false, true} {
		args := []string{"--dir", dir, "recall", "a", "--query", "database", "--query", "policy", "--min-confidence", verified, "--confidence-weight", "0.3"}
		if jsonOutput {
			args = append(args, "--json")
		}
		output, code := runE2E(t, current, dir, "", args...)
		if code == 0 || !strings.Contains(output, "confidence options unsupported") {
			t.Fatalf("all-failed batch against previous daemon returned success or hid error: json=%v exit=%d output=%q", jsonOutput, code, output)
		}
		if !jsonOutput && strings.Contains(output, "No memories found") {
			t.Fatalf("all-failed batch against previous daemon reported absence: exit=%d output=%q", code, output)
		}
	}
	reopens := 0
	r := newReconnectingStoreAt(daemonStore{Client: initial}, func() (cliStore, error) {
		reopens++
		client, err := daemon.ConnectNoSpawn(dir)
		if err != nil {
			return nil, err
		}
		return daemonStore{Client: client}, nil
	})
	t.Cleanup(func() { _ = r.Close() })
	if _, err := r.PutWithOptionsReturningFact(ctx, "a", "must not write", memory.WriteOptions{Confidence: &verified}); !errors.Is(err, daemon.ErrMaintenancePolicyUnsupported) {
		t.Fatalf("new write against previous=%v", err)
	}
	if _, err := r.PutSharedWithOptionsReturningFact(ctx, "must not write shared", memory.WriteOptions{Confidence: &verified}); !errors.Is(err, memory.ErrConfidenceUnsupported) {
		t.Fatalf("new explicit shared write against previous=%v", err)
	}
	if _, err := r.RecallWithOptions(ctx, "a", "database", 8, opts); !errors.Is(err, memory.ErrConfidenceUnsupported) {
		t.Fatalf("new recall against previous=%v", err)
	}
	legacy, err := r.RecallWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || len(legacy.Facts) != 1 || legacy.Retrieval != nil || reopens != 0 {
		t.Fatalf("legacy fallback=%+v err=%v reopens=%d", legacy, err, reopens)
	}
	oldProcess.stop()
	if version := confidenceIndexVersion(t, dir, "a"); version != 2 {
		t.Fatalf("previous index version=%d", version)
	}

	newProcess := confidenceStartDaemon(t, current, dir)
	empty, err := r.RecallWithOptions(ctx, "a", "database", 8, opts)
	if err != nil || len(empty.Facts) != 0 || empty.Retrieval == nil || reopens != 1 {
		t.Fatalf("upgrade negotiation=%+v err=%v reopens=%d", empty, err, reopens)
	}
	v, err := r.PutWithOptionsReturningFact(ctx, "a", "verified database policy", memory.WriteOptions{Confidence: &verified})
	if err != nil {
		t.Fatal(err)
	}
	u, err := r.PutWithOptionsReturningFact(ctx, "a", "unverified database policy", memory.WriteOptions{Confidence: &unverified})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := r.PutSharedWithOptionsReturningFact(ctx, "verified shared database policy", memory.WriteOptions{Confidence: &verified})
	if err != nil || shared.AgentID != memory.SharedAgentID || shared.Confidence != verified {
		t.Fatalf("new shared endpoint: %+v %v", shared, err)
	}
	eligible, err := r.RecallWithOptions(ctx, "a", "database", 8, opts)
	if err != nil || len(eligible.Facts) != 1 || eligible.Facts[0] != v.Text {
		t.Fatalf("new filtered recall=%+v %v", eligible, err)
	}
	newProcess.stop()
	if version := confidenceIndexVersion(t, dir, "a"); version != 4 {
		t.Fatalf("new index version=%d", version)
	}

	oldProcess = confidenceStartDaemon(t, previous, dir)
	if _, err := r.RecallWithOptions(ctx, "a", "database", 8, opts); !errors.Is(err, memory.ErrConfidenceUnsupported) || reopens != 2 {
		t.Fatalf("downgrade renegotiation err=%v reopens=%d", err, reopens)
	}
	if _, err := r.PutWithOptionsReturningFact(ctx, "a", "must not write on downgrade", memory.WriteOptions{Confidence: &verified}); !errors.Is(err, daemon.ErrMaintenancePolicyUnsupported) || reopens != 2 {
		t.Fatalf("downgrade mutation err=%v reopens=%d", err, reopens)
	}
	legacy, err = r.RecallWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || len(legacy.Facts) != 3 {
		t.Fatalf("downgrade legacy=%+v %v", legacy, err)
	}
	old, err := daemon.ConnectNoSpawn(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the older binary's wire compatibility independently of the new write gate.
	if err := old.CallService(rpc.ServiceName, "Put", &rpc.PutRequest{AgentID: "a", Text: "downgrade database policy"}, &rpc.PutResponse{}, 0); err != nil {
		t.Fatal(err)
	}
	facts, err := old.List("a")
	_ = old.Close()
	if err != nil || len(facts) != 4 {
		t.Fatalf("downgrade facts=%v err=%v", facts, err)
	}
	for _, fact := range facts {
		if fact.ID == v.ID && fact.Confidence != verified || fact.ID == u.ID && fact.Confidence != unverified {
			t.Fatalf("previous binary lost label: %+v", fact)
		}
	}
	oldProcess.stop()
	if version := confidenceIndexVersion(t, dir, "a"); version != 2 {
		t.Fatalf("downgraded index version=%d", version)
	}

	newProcess = confidenceStartDaemon(t, current, dir)
	eligible, err = r.RecallWithOptions(ctx, "a", "database", 8, opts)
	if err != nil || len(eligible.Facts) != 1 || eligible.Facts[0] != v.Text || reopens != 3 {
		t.Fatalf("second upgrade=%+v err=%v reopens=%d", eligible, err, reopens)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	newProcess.stop()
	if version := confidenceIndexVersion(t, dir, "a"); version != 4 {
		t.Fatalf("reupgraded index version=%d", version)
	}
	// Canonical data must retain every write and its confidence through both
	// direction changes; compare IDs, not text searches or index state alone.
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, StrictWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	facts, err = store.List("a")
	if err != nil || len(facts) != 4 {
		t.Fatalf("final facts=%v err=%v", facts, err)
	}
	byID := make(map[string]memory.Fact)
	for _, fact := range facts {
		byID[fact.ID] = fact
	}
	if byID[v.ID].Confidence != verified || byID[u.ID].Confidence != unverified {
		t.Fatalf("canonical confidence lost: %v", byID)
	}
	sharedFacts, err := store.List(memory.SharedAgentID)
	if err != nil || len(sharedFacts) != 1 || sharedFacts[0].ID != shared.ID || sharedFacts[0].Confidence != verified {
		t.Fatalf("explicit shared confidence lost across binaries: %+v %v", sharedFacts, err)
	}
}
