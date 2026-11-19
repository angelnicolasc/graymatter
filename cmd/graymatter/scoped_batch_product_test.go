package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func TestScopedBatchProductAliasAccess(t *testing.T) {
	for _, mode := range []string{"direct", "daemon"} {
		t.Run(mode, func(t *testing.T) {
			f := newIssue81Fixture(t)
			project := filepath.Join(f.root, "project")
			if err := os.Mkdir(project, 0o755); err != nil {
				t.Fatal(err)
			}
			dir := f.registerStore(filepath.Join(project, ".graymatter"))
			flags := []string{"--dir", dir}
			if mode == "direct" {
				flags = append(flags, "--no-daemon")
			}
			run := func(args ...string) issue81Output {
				return f.mustRun(project, "", append(slices.Clone(flags), args...)...)
			}

			const localFact = "route policy for this project"
			const sharedFact = "network policy for all projects"
			run("remember", "agent", localFact)
			run("remember", "--shared", sharedFact)
			run("alias", "agent", "zlookup", "route")
			run("alias", memory.SharedAgentID, "zlookup", "network")

			checkBatch := func(raw []byte, want []string) {
				t.Helper()
				var payload struct {
					PerQuery []struct {
						Query string   `json:"query"`
						Facts []string `json:"facts"`
						Error string   `json:"error"`
					} `json:"per_query"`
				}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatalf("decode batch: %v (%s)", err, raw)
				}
				if len(payload.PerQuery) != 2 {
					t.Fatalf("expected both duplicate queries: %s", raw)
				}
				for _, row := range payload.PerQuery {
					if row.Query != "zlookup" || row.Error != "" || len(row.Facts) != len(want) {
						t.Fatalf("unexpected batch row: %+v", row)
					}
					for _, text := range want {
						if !slices.Contains(row.Facts, text) {
							t.Fatalf("batch row missed %q: %+v", text, row)
						}
					}
				}
			}

			out := run("recall", "agent", "--query", "zlookup", "--query", "zlookup", "--all", "--json")
			checkBatch([]byte(out.stdout), []string{localFact, sharedFact})
			out = run("recall", "agent", "--query", "zlookup", "--query", "zlookup", "--shared", "--json")
			checkBatch([]byte(out.stdout), []string{sharedFact})

			mcp := f.startMCP(project, f.bin, append(slices.Clone(flags), "mcp", "serve"), nil)
			result := mcp.request(3, "tools/call", map[string]any{
				"name":      "memory_search_batch",
				"arguments": map[string]any{"agent_id": "agent", "queries": []string{"zlookup", "zlookup"}},
			})
			mcp.close()
			var envelope struct {
				Payload json.RawMessage `json:"structuredContent"`
			}
			if err := json.Unmarshal(result, &envelope); err != nil {
				t.Fatal(err)
			}
			checkBatch(envelope.Payload, []string{localFact})

			var store interface {
				List(string) ([]memory.Fact, error)
				Close() error
			}
			var err error
			if mode == "direct" {
				store, err = memory.Open(memory.StoreConfig{DataDir: dir, ReadOnly: true})
			} else {
				store, err = rpc.Dial(rpc.DialOptions{DataDir: dir, PingOnDial: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			for _, namespace := range []string{"agent", memory.SharedAgentID} {
				facts, err := store.List(namespace)
				if err != nil {
					t.Fatal(err)
				}
				i := slices.IndexFunc(facts, memory.Fact.IsAlias)
				// The all batch credits both aliases. The shared CLI batch and
				// the agent-scoped MCP batch each credit just one more.
				if i < 0 || facts[i].AccessCount != 2 {
					t.Fatalf("%s alias should have two accesses: %+v", namespace, facts)
				}
			}
		})
	}
}
