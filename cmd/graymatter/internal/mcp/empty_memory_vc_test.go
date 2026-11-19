package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const vcStartupProject = "vc-startup-project"
const vcHarborFact = "Harbor loading berth closes on Thursday"

func TestVC101PendingProjectCheckpointKeepsStaticInstructions(t *testing.T) {
	srv, mem := utilityContractServer(t, WithLocalProjectStartup(vcStartupProject))
	id := vcSavePendingCheckpoint(t, srv, vcStartupProject)
	for _, ns := range []string{vcStartupProject, memory.SharedAgentID} {
		if facts := vcStartupFacts(t, mem, ns); len(facts) != 0 {
			t.Fatalf("checkpoint-only fixture has facts in %s: %+v", ns, facts)
		}
	}
	if got := initializeInstructions(t, srv); got != coreServerInstructions {
		t.Errorf("pending project checkpoint changed static instructions: %q", got)
	}
	result := vcStartupSearch(t, srv, vcStartupProject, "deployment window")
	want := fmt.Sprintf("Pending checkpoint %q; use checkpoint_resume.", id)
	if result.IsError || !vcHasResponseLine(result.text(), want) {
		t.Fatalf("first project search did not report the pending checkpoint: %s", result.Raw)
	}
	if facts := utilityV1SearchFacts(t, result, nil); len(facts) != 0 {
		t.Fatalf("checkpoint-only search returned facts: %s", result.Raw)
	}
}

func TestVC102EmptyMemoryHintInvariant(t *testing.T) {
	type fixture struct {
		srv *Server
		mem *graymatter.Memory
	}
	for _, state := range []struct {
		name string
		hint string
		seed func(*testing.T, *fixture)
	}{
		{"empty", "yes", nil},
		{"project-live", "no", func(t *testing.T, f *fixture) {
			utilityContractAdd(t, f.srv, vcStartupProject, vcHarborFact)
		}},
		{"shared-live", "no", func(t *testing.T, f *fixture) {
			utilityContractAdd(t, f.srv, memory.SharedAgentID, vcHarborFact)
		}},
		{"project-checkpoint", "no", func(t *testing.T, f *fixture) {
			vcSavePendingCheckpoint(t, f.srv, vcStartupProject)
		}},
		{"project-list-error", "no", func(t *testing.T, f *fixture) {
			f.srv = vcStartupFailureServer(f.mem, vcStartupProject, false)
		}},
		{"shared-list-error", "no", func(t *testing.T, f *fixture) {
			f.srv = vcStartupFailureServer(f.mem, memory.SharedAgentID, false)
		}},
		{"checkpoint-read-error", "no", func(t *testing.T, f *fixture) {
			f.srv = vcStartupFailureServer(f.mem, "", true)
		}},
		{"retired-only", "yes", func(t *testing.T, f *fixture) {
			utilityContractAdd(t, f.srv, vcStartupProject, vcHarborFact)
			result := utilityContractCall(t, f.srv, "memory_reflect", map[string]any{
				"agent_id": vcStartupProject, "action": "forget", "target": vcHarborFact,
			})
			facts := vcStartupFacts(t, f.mem, vcStartupProject)
			if result.IsError || len(facts) != 1 || !facts[0].IsSuperseded() {
				t.Fatalf("fixture did not retain exactly one retired fact: %s / %+v", result.Raw, facts)
			}
		}},
		{"expired-pruned", "yes", func(t *testing.T, f *fixture) {
			vcAgeStartupFact(t, f.srv, f.mem)
			if err := f.mem.Consolidate(context.Background(), vcStartupProject); err != nil {
				t.Fatalf("prune aged fact: %v", err)
			}
			if facts := vcStartupFacts(t, f.mem, vcStartupProject); len(facts) != 0 {
				t.Fatalf("aged fixture was not physically pruned: %+v", facts)
			}
		}},
		{"other-namespace-live", "yes", func(t *testing.T, f *fixture) {
			utilityContractAdd(t, f.srv, "vc-other-project", vcHarborFact)
		}},
		{"expired-retained", "", func(t *testing.T, f *fixture) {
			vcAgeStartupFact(t, f.srv, f.mem)
		}},
		{"alias-only", "", func(t *testing.T, f *fixture) {
			result := utilityContractCall(t, f.srv, "memory_alias", map[string]any{
				"agent_id": vcStartupProject, "term": "harbor", "equivalents": []string{"loading berth"},
			})
			facts := vcStartupFacts(t, f.mem, vcStartupProject)
			if result.IsError || len(facts) != 1 || !facts[0].IsAlias() {
				t.Fatalf("fixture did not retain exactly one alias: %s / %+v", result.Raw, facts)
			}
		}},
		{"shared-checkpoint", "", func(t *testing.T, f *fixture) {
			vcSavePendingCheckpoint(t, f.srv, memory.SharedAgentID)
		}},
		{"resumed-checkpoint", "", func(t *testing.T, f *fixture) {
			id := vcSavePendingCheckpoint(t, f.srv, vcStartupProject)
			result := utilityContractCall(t, f.srv, "checkpoint_resume", map[string]any{"agent_id": vcStartupProject})
			if result.IsError || result.Structured["id"] != id {
				t.Fatalf("fixture did not resume its saved checkpoint: %s", result.Raw)
			}
		}},
	} {
		t.Run(state.name, func(t *testing.T) {
			srv, mem := utilityContractServer(t, WithLocalProjectStartup(vcStartupProject))
			f := fixture{srv, mem}
			if state.seed != nil {
				state.seed(t, &f)
			}

			instructions := initializeInstructions(t, f.srv)
			// T3 selects a complete briefing with a single directive for an empty store.
			hint := instructions == emptyCoreServerInstructions
			if !hint && instructions != coreServerInstructions {
				t.Errorf("unexpected initialization instructions: %q", instructions)
			}
			if (state.hint == "yes" && !hint) || (state.hint == "no" && hint) {
				t.Errorf("hint present = %t, want %s", hint, state.hint)
			}

			var factHits, pointerHits, searchErrors int
			for _, ns := range []string{vcStartupProject, memory.SharedAgentID} {
				canonical := vcStartupFacts(t, f.mem, ns)
				for _, query := range []string{"deployment window", "quartz azimuth", "harbor loading berth"} {
					result := vcStartupSearch(t, f.srv, ns, query)
					if result.IsError {
						searchErrors++
						if hint {
							t.Errorf("hint present with unreadable search for %s %q: %s", ns, query, result.Raw)
						}
						continue
					}
					facts := utilityV1SearchFacts(t, result, canonical)
					factHits += len(facts)
					pointer := vcHasCheckpointPointer(result.text())
					if pointer {
						pointerHits++
					}
					if hint && (len(facts) != 0 || pointer) {
						t.Errorf("hint present with facts or checkpoint for %s %q: %s", ns, query, result.Raw)
					}
				}
			}
			t.Logf("VC103 state=%s hint=%t both_searches_empty=%t fact_hits=%d pointer_hits=%d search_errors=%d",
				state.name, hint, factHits == 0 && pointerHits == 0 && searchErrors == 0, factHits, pointerHits, searchErrors)
		})
	}
}

func vcStartupSearch(t *testing.T, srv *Server, namespace, query string) utilityContractResult {
	t.Helper()
	return utilityContractCall(t, srv, "memory_search", map[string]any{
		"agent_id": namespace, "query": query, "scope": "project",
	})
}

func vcSavePendingCheckpoint(t *testing.T, srv *Server, namespace string) string {
	t.Helper()
	result := utilityContractCall(t, srv, "checkpoint_save", map[string]any{
		"agent_id": namespace, "state": `{"task":"Resume the harbor review"}`,
	})
	id, _ := result.Structured["checkpoint_id"].(string)
	if result.IsError || id == "" {
		t.Fatalf("seed pending checkpoint: %s", result.Raw)
	}
	return id
}

func vcHasResponseLine(text, want string) bool {
	for _, line := range strings.Split(text, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func vcHasCheckpointPointer(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, `Pending checkpoint "`) {
			return true
		}
	}
	return false
}

func vcStartupFacts(t *testing.T, mem *graymatter.Memory, namespace string) []memory.Fact {
	t.Helper()
	facts, err := mem.Advanced().List(namespace)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func vcAgeStartupFact(t *testing.T, srv *Server, mem *graymatter.Memory) {
	t.Helper()
	utilityContractAdd(t, srv, vcStartupProject, vcHarborFact)
	facts := vcStartupFacts(t, mem, vcStartupProject)
	if len(facts) != 1 {
		t.Fatalf("expected one fact to age: %+v", facts)
	}
	aged := facts[0]
	aged.CreatedAt = time.Now().UTC().Add(-2 * 365 * 24 * time.Hour)
	aged.AccessedAt = aged.CreatedAt
	aged.Weight = 0.001
	if err := mem.Advanced().UpdateFact(vcStartupProject, aged); err != nil {
		t.Fatalf("age fixture: %v", err)
	}
	facts = vcStartupFacts(t, mem, vcStartupProject)
	if len(facts) != 1 || facts[0].IsSuperseded() || !facts[0].AccessedAt.Equal(aged.AccessedAt) || facts[0].Weight != aged.Weight {
		t.Fatalf("expected one physically retained, aged fact: %+v", facts)
	}
}

type vcStartupReadFailure struct {
	*DirectBackend
	listNamespace  string
	checkpointRead bool
}

func vcStartupFailureServer(mem *graymatter.Memory, namespace string, checkpoint bool) *Server {
	backend := &vcStartupReadFailure{NewDirectBackend(mem, nil), namespace, checkpoint}
	return New(backend, "vc-test", WithLocalProjectStartup(vcStartupProject))
}

func (b *vcStartupReadFailure) List(namespace string) ([]memory.Fact, error) {
	if namespace == b.listNamespace {
		return nil, errors.New("list read failed")
	}
	return b.DirectBackend.List(namespace)
}

func (b *vcStartupReadFailure) CheckpointPeekPending(namespace string) (*session.PendingCheckpoint, error) {
	if b.checkpointRead && namespace == vcStartupProject {
		return nil, errors.New("checkpoint read failed")
	}
	return b.DirectBackend.CheckpointPeekPending(namespace)
}
