package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

// Client is a connection to the daemon: the embedded rpc.Client provides the
// core store surface (Put/Recall/List/Stats/...), and the typed methods here
// cover the host-level service (checkpoints, sessions, KG, audit, tokens).
type Client struct {
	*rpc.Client
	dataDir string
}

const hostCallTimeout = 30 * time.Second

// connectBudget bounds the total dial+spawn+retry sequence. Daemon cold
// start is dominated by bbolt open + listen — well under a second on local
// disks — so this is generous headroom, not expected latency.
const connectBudget = 10 * time.Second

// Connect dials the daemon for dataDir, starting one if none is running.
//
// The spawn race is benign by construction: any number of clients may spawn
// daemons concurrently, but only the bbolt lock winner survives to write the
// discovery file; every loser exits and every client converges on the winner
// through the retry loop.
func Connect(dataDir string) (*Client, error) {
	return connect(dataDir, true)
}

// ConnectNoSpawn dials the daemon for dataDir but never starts one. For
// `daemon status` / `daemon stop`, where spawning would defeat the point.
func ConnectNoSpawn(dataDir string) (*Client, error) {
	return connect(dataDir, false)
}

func connect(dataDir string, allowSpawn bool) (*Client, error) {
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: resolve data dir: %w", err)
	}

	dial := func() (*rpc.Client, error) {
		return rpc.Dial(rpc.DialOptions{DataDir: absDir, PingOnDial: true})
	}

	// Fast path: daemon already up.
	if c, err := dial(); err == nil {
		return &Client{Client: c, dataDir: absDir}, nil
	} else if !allowSpawn {
		return nil, fmt.Errorf("daemon not reachable at %s: %w", absDir, err)
	}

	if err := spawnDaemon(absDir); err != nil {
		return nil, fmt.Errorf("daemon not running and auto-start failed: %w", err)
	}

	// Retry with backoff while the daemon cold-starts (or a racing winner
	// finishes writing its discovery file).
	deadline := time.Now().Add(connectBudget)
	wait := 25 * time.Millisecond
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(wait)
		if wait < 400*time.Millisecond {
			wait = wait * 8 / 5
		}
		c, err := dial()
		if err == nil {
			return &Client{Client: c, dataDir: absDir}, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf(
		"daemon did not come up within %s: %w%s\nhint: another process may hold %s open (a Go program embedding the graymatter library?); `graymatter doctor` can diagnose this",
		connectBudget, lastErr, daemonLogTail(absDir), filepath.Join(absDir, "gray.db"))
}

// DataDir returns the absolute data directory this client is bound to.
func (c *Client) DataDir() string { return c.dataDir }

// resolveExecutable returns the path of the binary to spawn as the daemon.
// Overridable in tests so they can point at a freshly built graymatter
// binary instead of the test runner.
var resolveExecutable = os.Executable

// spawnDaemon launches `graymatter daemon run` detached, with stdout/stderr
// appended to <dataDir>/daemon.log.
func spawnDaemon(absDir string) error {
	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(absDir, "daemon.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon.log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(exe, "daemon", "run",
		"--dir", absDir,
		"--idle-exit", DefaultIdleExit.String())
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s daemon run: %w", exe, err)
	}
	// Detach fully: the daemon outlives us and we must not leave a zombie
	// behind when it exits first.
	return cmd.Process.Release()
}

// daemonLogTail returns the last few lines of daemon.log formatted for
// appending to an error message, or "" if the log is unreadable.
func daemonLogTail(absDir string) string {
	data, err := os.ReadFile(filepath.Join(absDir, "daemon.log"))
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return "\ndaemon.log (tail):\n  " + strings.Join(lines, "\n  ")
}

// --- typed host-service methods ----------------------------------------------

func (c *Client) hostCall(method string, req, resp any) error {
	return c.CallService(HostServiceName, method, req, resp, hostCallTimeout)
}

// CheckpointSave persists cp on the daemon and returns it with ID assigned.
func (c *Client) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	var resp CheckpointSaveResponse
	if err := c.hostCall("CheckpointSave", &CheckpointSaveRequest{CP: cp}, &resp); err != nil {
		return session.Checkpoint{}, err
	}
	return resp.CP, nil
}

// CheckpointLoad retrieves one checkpoint by ID.
func (c *Client) CheckpointLoad(agentID, checkpointID string) (*session.Checkpoint, error) {
	var resp CheckpointLoadResponse
	if err := c.hostCall("CheckpointLoad", &CheckpointLoadRequest{AgentID: agentID, CheckpointID: checkpointID}, &resp); err != nil {
		return nil, err
	}
	return &resp.CP, nil
}

// CheckpointResume retrieves the most recent checkpoint for agentID.
func (c *Client) CheckpointResume(agentID string) (*session.Checkpoint, error) {
	var resp CheckpointResumeResponse
	if err := c.hostCall("CheckpointResume", &CheckpointResumeRequest{AgentID: agentID}, &resp); err != nil {
		return nil, err
	}
	return &resp.CP, nil
}

// CheckpointList returns all checkpoints for agentID, newest first.
func (c *Client) CheckpointList(agentID string) ([]session.Checkpoint, error) {
	var resp CheckpointListResponse
	if err := c.hostCall("CheckpointList", &CheckpointListRequest{AgentID: agentID}, &resp); err != nil {
		return nil, err
	}
	return resp.CPs, nil
}

// SessionsList returns all harness session records, newest first.
func (c *Client) SessionsList() ([]harness.HarnessSession, error) {
	var resp SessionListResponse
	if err := c.hostCall("SessionList", &SessionListRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// SessionSave persists a harness session record.
func (c *Client) SessionSave(hs harness.HarnessSession) error {
	return c.hostCall("SessionSave", &SessionSaveRequest{S: hs}, &SessionSaveResponse{})
}

// SessionKill terminates a running background session by ID.
func (c *Client) SessionKill(id string) error {
	return c.hostCall("SessionKill", &SessionKillRequest{ID: id}, &SessionKillResponse{})
}

// SessionResolve resolves "latest" (optionally per-agent) to a concrete ID.
func (c *Client) SessionResolve(agentID, sessionID string) (string, error) {
	var resp SessionResolveResponse
	if err := c.hostCall("SessionResolve", &SessionResolveRequest{AgentID: agentID, SessionID: sessionID}, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

// KGNodes returns every knowledge-graph node (empty when no graph exists).
func (c *Client) KGNodes() ([]kg.Node, error) {
	var resp KGNodesResponse
	if err := c.hostCall("KGNodes", &KGNodesRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Nodes, nil
}

// KGLink creates an edge between two nodes on the daemon's graph.
func (c *Client) KGLink(from, to, relation string) error {
	return c.hostCall("KGLink", &KGLinkRequest{From: from, To: to, Relation: relation}, &KGLinkResponse{})
}

// KGUpsert inserts or updates a node on the daemon's graph.
func (c *Client) KGUpsert(id, label, entityType string) error {
	return c.hostCall("KGUpsert", &KGUpsertRequest{ID: id, Label: label, EntityType: entityType}, &KGUpsertResponse{})
}

// AuditWrite records an agent self-edit event.
func (c *Client) AuditWrite(e audit.Entry) error {
	return c.hostCall("AuditWrite", &AuditWriteRequest{E: e}, &AuditWriteResponse{})
}

// TokenRecord adds token usage to the daemon's pre-aggregated ledger.
func (c *Client) TokenRecord(agent, model string, input, output, cacheRead, cacheWrite uint64) error {
	return c.hostCall("TokenRecord", &TokenRecordRequest{
		Agent: agent, Model: model,
		Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite,
	}, &TokenRecordResponse{})
}

// TokenSummary aggregates the token ledger over the trailing N days.
func (c *Client) TokenSummary(days int) (harness.TokenUsageSummary, error) {
	var resp TokenSummaryResponse
	if err := c.hostCall("TokenSummary", &TokenSummaryRequest{Days: days}, &resp); err != nil {
		return harness.TokenUsageSummary{}, err
	}
	return resp.S, nil
}

// Shutdown asks the daemon to stop gracefully.
func (c *Client) Shutdown() error {
	return c.CallService(HostServiceName, "Shutdown", &ShutdownRequest{}, &ShutdownResponse{}, 5*time.Second)
}

// Remember stores a fact with full Remember semantics (the daemon backend
// routes Put through Memory.Remember, async consolidation included).
func (c *Client) Remember(ctx context.Context, agentID, text string) error {
	return c.Put(ctx, agentID, text)
}

// RecallDefault recalls with the daemon's configured TopK.
func (c *Client) RecallDefault(ctx context.Context, agentID, query string) ([]string, error) {
	return c.Recall(ctx, agentID, query, 0)
}
