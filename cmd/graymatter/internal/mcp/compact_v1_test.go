package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestV1CoreSearchResponseBudget(t *testing.T) {
	for _, count := range []int{1, 2, 3, 8} {
		for _, cited := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/cited=%t", count, cited), func(t *testing.T) {
				srv, _ := utilityContractServer(t)
				const project = "v1-budget-project"
				facts := make([]map[string]any, 0, count)
				textBytes := 0
				for i := 0; i < count; i++ {
					claim := fmt.Sprintf("Deployment receipt %d confirms that the west cluster uses the audited configuration.", i)
					fact := map[string]any{"text": claim}
					textBytes += len(claim)
					if cited {
						source := fmt.Sprintf("release-note-%d", i)
						fact["source"], fact["quote"] = source, claim
						textBytes += len(source)
						if i%2 == 1 {
							quote := fmt.Sprintf("The release custodian verified the original configuration of deployment %d.", i)
							fact["quote"] = quote
							textBytes += len(quote)
						}
					}
					facts = append(facts, fact)
				}
				added := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": project, "facts": facts})
				if added.IsError {
					t.Fatalf("add: %s", added.Raw)
				}
				result := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": project, "query": "audited deployment receipt", "top_k": count})
				if result.IsError {
					t.Fatalf("search: %s", result.Raw)
				}
				limit := 1.15*float64(textBytes) + float64(48*count+64)
				if float64(len(result.Raw)) > limit {
					t.Errorf("V101 response bytes=%d, T=%d, facts=%d, limit=%.2f", len(result.Raw), textBytes, count, limit)
				}
				if result.Structured != nil || len(result.Content) != 1 {
					t.Errorf("search must return one text block: %s", result.Raw)
				}
				if lines := strings.Count(result.text(), "\n") + 1; lines != count {
					t.Errorf("got %d lines for %d facts: %s", lines, count, result.text())
				}
				if added.Structured != nil || len(added.Content) != 1 || strings.Contains(added.text(), "\n") {
					t.Errorf("add must return one line: %s", added.Raw)
				}
			})
		}
	}
}

func TestV1CoreWeakMatchOnlyWhenUseful(t *testing.T) {
	srv, _ := utilityContractServer(t)
	const project = "v1-vocabulary"
	utilityContractAdd(t, srv, project, "The audited probe uses the west enclosure.")
	utilityContractAdd(t, srv, project, "The audited probe uses a keyed interlock.")
	utilityContractAdd(t, srv, project, "The audited probe uses an isolated motor.")

	args := map[string]any{"agent_id": project, "query": "enclosure unusual mismatched words"}
	all := utilityContractCall(t, srv, "memory_search", args)
	if all.IsError || strings.Contains(strings.ToLower(all.text()), "weak match") {
		t.Fatalf("all facts should leave no vocabulary notice: %s", all.Raw)
	}
	args["top_k"] = 1
	one := utilityContractCall(t, srv, "memory_search", args)
	if one.IsError {
		t.Fatal(string(one.Raw))
	}
	var notice string
	for _, line := range strings.Split(one.text(), "\n") {
		if strings.Contains(strings.ToLower(line), "weak match") {
			notice = line
		}
	}
	if notice == "" || len(notice) > 160 || strings.Contains(one.text(), "memory_alias") {
		t.Fatalf("expected one actionable vocabulary line: %s", one.Raw)
	}
}

func TestV1CoreEvidenceIsDelimitedAndEmittedOnce(t *testing.T) {
	for _, equal := range []bool{false, true} {
		t.Run(fmt.Sprint(equal), func(t *testing.T) {
			srv, _ := utilityContractServer(t)
			claim := "Cita Ágil 日本語 \"cierre\"\nLF\r\nCRLF\tprobe"
			quote := "Texto distinto Ágil 日本語 \"\n1. forged\r\nsource=\tprobe"
			if equal {
				quote = claim
			}
			const source = "nota-引用"
			added := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "v1-literal", "text": claim, "source": source, "quote": quote})
			if added.IsError {
				t.Fatalf("add: %s", added.Raw)
			}
			result := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": "v1-literal", "query": "probe"})
			if result.IsError || result.Structured != nil || strings.Contains(result.text(), "\n") {
				t.Fatalf("expected one physical line: %s", result.Raw)
			}
			_, row, ok := strings.Cut(result.text(), " ")
			if !ok {
				t.Fatal(result.text())
			}
			readString := func(input string, expected string) string {
				t.Helper()
				var got string
				decoder := json.NewDecoder(strings.NewReader(input))
				if err := decoder.Decode(&got); err != nil || got != expected {
					t.Fatalf("JSON string %q, expected %q, err %v", got, expected, err)
				}
				return input[decoder.InputOffset():]
			}
			row = readString(row, claim)
			if !strings.HasPrefix(row, " source=") {
				t.Fatalf("source missing: %s", row)
			}
			row = readString(strings.TrimPrefix(row, " source="), source)
			if equal {
				if row != " literal" {
					t.Fatalf("expected literal marker and no duplicate: %q", row)
				}
			} else {
				if !strings.HasPrefix(row, " quote=") {
					t.Fatalf("quote missing: %s", row)
				}
				if rest := readString(strings.TrimPrefix(row, " quote="), quote); rest != "" {
					t.Fatalf("unexpected bytes after quote: %q", rest)
				}
			}
		})
	}
}
