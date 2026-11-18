package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Core keeps confidence as an optional runtime capability and gives indexed
// ranking receipts when explain is requested.
func TestUtilityCoreConfidenceDefaultFilterZeroAndIndexedReceipts(t *testing.T) {
	_, mem := confidenceFullTestServerWithWeight(t, .2)
	srv := New(NewDirectBackend(mem, nil), "core-confidence")
	committed := make(map[string]string)
	for _, label := range []string{"verified", "unverified"} {
		added := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": "policy " + label, "confidence": label})
		if added.IsError {
			t.Fatalf("confidence write: %s", added.Raw)
		}
		committed[label] = utilityStoredShortID(t, added)
	}
	canonical, err := mem.Advanced().List("a")
	if err != nil || len(canonical) != 2 {
		t.Fatalf("canonical writes: %+v / %v", canonical, err)
	}
	byID := map[string]memory.Fact{}
	for _, fact := range canonical {
		id := committed[fact.Confidence]
		if id == "" || !strings.HasPrefix(fact.ID, id) {
			t.Fatalf("write did not identify the canonical %s fact: %+v", fact.Confidence, fact)
		}
		byID[fact.ID] = fact
	}
	for _, mode := range []string{"single", "queries", "hidden_batch"} {
		for _, explain := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explain=%v", mode, explain), func(t *testing.T) {
				tool := "memory_search"
				args := map[string]any{"agent_id": "a", "scope": "project", "explain": explain}
				if mode == "single" {
					args["query"] = "policy"
				} else {
					args["queries"] = []string{"policy", "verified"}
					if mode == "hidden_batch" {
						tool = "memory_search_batch"
					}
				}
				result := utilityContractCall(t, srv, tool, args)
				if result.IsError || !strings.Contains(result.text(), "confidence_weight=0.2") {
					t.Fatalf("configured default lost: %s", result.Raw)
				}
				textOnly := !explain
				if textOnly {
					selected := utilityV1SearchFacts(t, result, canonical)
					if len(selected) != 2 {
						t.Fatalf("default search changed selected rows: %s", result.Raw)
					}
					for _, fact := range selected {
						if strings.Count(string(result.Raw), fact.Text) != 1 {
							t.Fatalf("text lost or duplicated: %s", result.Raw)
						}
					}
				} else {
					rows, ok := result.Structured["facts"].([]any)
					if !ok || len(rows) != 2 {
						t.Fatalf("default search changed selected rows: %s", result.Raw)
					}
					for i, value := range rows {
						row := value.(map[string]any)
						fact, found := byID[row["id"].(string)]
						if !found || row["i"] != float64(i) {
							t.Fatalf("receipt has no canonical indexed identity: %#v", row)
						}
						encoded, _ := json.Marshal(fact.Text)
						if !strings.Contains(result.text(), fmt.Sprintf("%d. %s", i+1, encoded)) || strings.Count(string(result.Raw), fact.Text) != 1 {
							t.Fatalf("indexed text lost or duplicated: %s", result.Raw)
						}
						receipt, present := row["explained"].(map[string]any)
						if present != explain {
							t.Fatalf("explain selection changed: %#v", row)
						}
						if explain {
							ranking, ok := receipt["ranking"].(map[string]any)
							if !ok || ranking["effective_confidence"] != fact.Confidence || ranking["confidence_weight"] != .2 || receipt["query_index"] != float64(0) || receipt["ranks"] == nil || receipt["text"] != nil {
								t.Fatalf("receipt mismatches selected fact/query/policy: %#v", receipt)
							}
						}
					}
				}
				args["min_confidence"], args["confidence_weight"] = "verified", 0.0
				filtered := utilityContractCall(t, srv, tool, args)
				if filtered.IsError || !strings.Contains(filtered.text(), "min_confidence=verified, confidence_weight=0;") || strings.Contains(filtered.text(), "policy unverified") {
					t.Fatalf("explicit zero lost filter/opt-out: %s", filtered.Raw)
				}
				if textOnly {
					selected := utilityV1SearchFacts(t, filtered, canonical)
					if len(selected) != 1 || selected[0].Confidence != "verified" {
						t.Fatalf("filter mismatch: %s", filtered.Raw)
					}
				} else {
					filteredRows, ok := filtered.Structured["facts"].([]any)
					if !ok || len(filteredRows) != 1 || byID[filteredRows[0].(map[string]any)["id"].(string)].Confidence != "verified" {
						t.Fatalf("filter mismatch: %s", filtered.Raw)
					}
					if explain {
						receipt := filteredRows[0].(map[string]any)["explained"].(map[string]any)
						ranking := receipt["ranking"].(map[string]any)
						if ranking["confidence_weight"] != float64(0) || ranking["factor"] != float64(1) || ranking["effective_confidence"] != "verified" {
							t.Fatalf("zero weight still changed ranking or lost confidence: %#v", ranking)
						}
					}
				}
				args["agent_id"] = "absent"
				empty := utilityContractCall(t, srv, tool, args)
				emptyRows, structured := empty.Structured["facts"].([]any)
				emptySelection := structured && len(emptyRows) == 0
				if textOnly {
					emptySelection = len(utilityV1SearchFacts(t, empty, canonical)) == 0
				}
				if empty.IsError || !emptySelection || !strings.Contains(empty.text(), "No memories found.") || !strings.Contains(empty.text(), "min_confidence=verified, confidence_weight=0;") {
					t.Fatalf("empty search lost effective policy: %s", empty.Raw)
				}
			})
		}
	}
}

func TestUtilityCoreInvalidConfidenceHasNoEffects(t *testing.T) {
	srv, mem := utilityContractServer(t)
	for _, bad := range []any{nil, "", "Verified", " verified", 1, true} {
		for _, args := range []map[string]any{
			{"agent_id": "a", "text": "invalid fact", "confidence": bad},
			{"agent_id": "a", "facts": []any{map[string]any{"text": "valid first"}, map[string]any{"text": "invalid second", "confidence": bad}}},
		} {
			if result := utilityContractCall(t, srv, "memory_add", args); !result.IsError {
				t.Fatalf("accepted invalid confidence %#v: %s", bad, result.Raw)
			}
		}
		for _, tool := range []string{"memory_search", "memory_search_batch"} {
			args := map[string]any{"agent_id": "a", "min_confidence": bad}
			if tool == "memory_search" {
				args["query"] = "fact"
			} else {
				args["queries"] = []string{"fact"}
			}
			if result := utilityContractCall(t, srv, tool, args); !result.IsError {
				t.Fatalf("accepted invalid minimum %#v: %s", bad, result.Raw)
			}
		}
	}
	for _, bad := range []any{nil, "0.2", true, -.1, .51} {
		if result := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": "a", "query": "fact", "confidence_weight": bad}); !result.IsError {
			t.Fatalf("accepted invalid weight %#v: %s", bad, result.Raw)
		}
	}
	facts, err := mem.Advanced().List("a")
	if err != nil || len(facts) != 0 {
		t.Fatalf("invalid options wrote facts: %+v / %v", facts, err)
	}
}
