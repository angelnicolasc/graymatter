package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// An alias the store promoted itself is invisible by design — never injected,
// never ranked, never in a result — so the only thing standing between "the
// store learns" and "the store changed my answers and I cannot see why" is
// this command. These pin the part that matters: that it separates what an
// agent taught from what the store concluded, and that a retired alias is not
// quietly presented as live.
func TestAliasListSeparatesAuthoredFromPromoted(t *testing.T) {
	const agent = "vocab"
	seedRevise(t, agent, "the billing service retries webhooks eight times")

	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.PutAlias(ctx, agent, "clearinghouse", []string{"billing"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// The usage source has no CLI writer — that is the point of it — so it
	// goes in through the library, exactly as the learner does.
	direct, err := memory.Open(memory.StoreConfig{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.PutAlias(ctx, agent, "retried", []string{"retries"}); err != nil {
		t.Fatal(err)
	}
	promoted := stampUsageSource(t, direct, agent, "retried")
	_ = direct.Close()

	out := runAliasList(t, agent, false)
	if !strings.Contains(out, "[authored] clearinghouse = billing") {
		t.Errorf("authored alias missing or unlabelled:\n%s", out)
	}
	if !strings.Contains(out, "[usage] retried = retries") {
		t.Errorf("promoted alias missing or unlabelled:\n%s", out)
	}

	// Retire the promoted one. A retired alias stops expanding queries, so
	// listing it as live would describe a rule the store is no longer
	// applying.
	direct, err = memory.Open(memory.StoreConfig{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Retire(agent, promoted); err != nil {
		t.Fatal(err)
	}
	_ = direct.Close()

	if out := runAliasList(t, agent, false); strings.Contains(out, "retried") {
		t.Errorf("retired alias still listed as live:\n%s", out)
	}
	if out := runAliasList(t, agent, true); !strings.Contains(out, "retried") {
		t.Errorf("--all did not surface the retired alias:\n%s", out)
	}
}

func TestAliasListJSONShape(t *testing.T) {
	const agent = "vocab"
	seedRevise(t, agent, "deployments need two approvals")
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutAlias(context.Background(), agent, "rollout", []string{"deployments", "release"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	oldJSON := jsonOut
	jsonOut = true
	t.Cleanup(func() { jsonOut = oldJSON })

	var doc struct {
		AgentID string `json:"agent_id"`
		Count   int    `json:"count"`
		Aliases []struct {
			Term        string   `json:"term"`
			Equivalents []string `json:"equivalents"`
			Source      string   `json:"source"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal([]byte(runAliasList(t, agent, false)), &doc); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if doc.Count != 1 || len(doc.Aliases) != 1 {
		t.Fatalf("want 1 alias, got count=%d len=%d", doc.Count, len(doc.Aliases))
	}
	a := doc.Aliases[0]
	if a.Term != "rollout" || a.Source != "authored" {
		t.Errorf("unexpected alias: %+v", a)
	}
	if len(a.Equivalents) != 2 || a.Equivalents[0] != "deployments" || a.Equivalents[1] != "release" {
		t.Errorf("equivalents lost their order or content: %v", a.Equivalents)
	}
}

func runAliasList(t *testing.T, agent string, all bool) string {
	t.Helper()
	cmd := aliasCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	args := []string{"list", agent}
	if all {
		args = append(args, "--all")
	}
	cmd.SetArgs(args)
	stdout := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("alias list: %v", err)
		}
	})
	return stdout + buf.String()
}

// stampUsageSource marks an alias as store-promoted. The learner writes that
// source from inside pkg/memory and exports no way to do it, which is correct
// — "the store concluded this" is not something a caller may claim — so the
// test reproduces the stored shape instead of asking for an API that should
// not exist.
func stampUsageSource(t *testing.T, s *memory.Store, agent, term string) memory.Fact {
	t.Helper()
	all, err := s.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all {
		if f.Kind == memory.KindAlias && strings.Contains(f.Text, term+" =") {
			f.AliasSource = memory.AliasSourceUsage
			if err := s.UpdateFact(agent, f); err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	t.Fatalf("alias %q not found after write", term)
	return memory.Fact{}
}
