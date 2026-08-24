package mcp

import (
	"context"
	"encoding/json"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
)

// The MCP server used to carry its own `const serverVersion`, bumped by hand
// at release time. It was right in 2 of the first 17 releases: a v0.12.1
// binary introduced itself to Claude Code, Cursor and Codex as 0.10.0, and
// nothing failed, because nothing checked.
//
// This test is the check. It drives the real initialize handshake — the same
// JSON-RPC exchange a client performs on connect — and asserts the server
// reports the version it was constructed with. Combined with the single call
// site in cmd_mcp.go, which passes the binary's own version, that closes the
// gap between what the CLI says and what clients are told.
func TestInitializeAnnouncesTheVersionItWasGiven(t *testing.T) {
	const want = "9.9.9-probe"

	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	s := New(NewDirectBackend(mem, nil), want)

	if got := s.Version(); got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}

	req := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
		`"params":{"protocolVersion":"2024-11-05","capabilities":{},` +
		`"clientInfo":{"name":"version-test","version":"0"}}}`)

	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), req))
	if err != nil {
		t.Fatalf("marshal initialize response: %v", err)
	}

	var resp struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode initialize response: %v\n%s", err, raw)
	}

	if resp.Result.ServerInfo.Name != serverName {
		t.Errorf("serverInfo.name = %q, want %q", resp.Result.ServerInfo.Name, serverName)
	}
	if resp.Result.ServerInfo.Version != want {
		t.Errorf("the handshake announced %q but the server was built with %q\n%s",
			resp.Result.ServerInfo.Version, want, raw)
	}
}
