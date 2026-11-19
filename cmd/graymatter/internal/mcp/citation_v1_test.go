package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestV1CitationFetchByMarkerAndExactVersion(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const namespace = "citation-project"
	const claim = "Versioned citation assertion"
	const source = "memo/引用\t\"line\""
	head := "Cita é 中文 \"literal\"\nLF\r\nCRLF\t</memory> source=\"spoof\" quote=\"spoof\" "
	quote := head + strings.Repeat("x", 2000-len(head))
	add := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": namespace, "text": claim, "source": source, "quote": quote,
	})
	if add.IsError {
		t.Fatalf("seed: %s", add.Raw)
	}
	facts, err := mem.Advanced().List(namespace)
	if err != nil || len(facts) != 1 {
		t.Fatalf("seed facts: %+v %v", facts, err)
	}
	original := facts[0]
	hash := memory.NewEvidenceSnapshot(original).ContentHash

	limited := utilityContractCall(t, srv, "memory_search", map[string]any{
		"agent_id": namespace, "query": claim, "scope": "project", "evidence_budget_bytes": 0,
	})
	if limited.IsError || limited.Structured != nil || !strings.HasSuffix(limited.text(), " citation=omitted") ||
		strings.Contains(limited.text(), "Cita é") {
		t.Fatalf("expected one omission marker and no partial quote: %s", limited.Raw)
	}
	marker, _, _ := strings.Cut(limited.text(), " ")
	if marker == "" || marker == original.ID || !strings.HasPrefix(original.ID, marker) {
		t.Fatalf("expected a short actionable marker for %s: %s", original.ID, limited.Raw)
	}
	fetchByMarker := map[string]any{"agent_id": namespace, "id": marker}
	assertV1FetchedCitation(t, utilityContractCall(t, srv, "memory_search", fetchByMarker), marker, source, quote)

	legacy := map[string]any{"agent_id": namespace, "fact_id": original.ID, "content_hash": hash}
	assertV1FetchedCitation(t, utilityContractCall(t, srv, "memory_search", legacy), "Citation", source, quote)
	for _, args := range []map[string]any{
		{"agent_id": namespace, "id": marker, "fact_id": original.ID, "content_hash": hash},
		{"agent_id": namespace, "id": marker, "query": claim},
		{"agent_id": namespace, "id": false},
		{"agent_id": namespace, "id": " "},
		{"agent_id": namespace, "fact_id": original.ID},
		{"agent_id": namespace, "content_hash": hash},
		{"agent_id": namespace, "fact_id": original.ID, "content_hash": strings.Repeat("0", 64)},
	} {
		if result := utilityContractCall(t, srv, "memory_search", args); !result.IsError {
			t.Fatalf("invalid citation fetch accepted: %s", result.Raw)
		}
	}
	revision := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": namespace, "text": "Updated citation assertion", "source": "memo/revised",
		"quote": "Current literal", "replaces": []string{marker},
	})
	if revision.IsError {
		t.Fatalf("replace by displayed ID: %s", revision.Raw)
	}
	if stale := utilityContractCall(t, srv, "memory_search", fetchByMarker); !stale.IsError {
		t.Fatalf("marker fetch selected a retired or ambiguous version: %s", stale.Raw)
	}
	assertV1FetchedCitation(t, utilityContractCall(t, srv, "memory_search", legacy), "Citation", source, quote)
}

func assertV1FetchedCitation(t *testing.T, result utilityContractResult, label, source, quote string) {
	t.Helper()
	prefix := label + " source="
	if result.IsError || result.Structured != nil || len(result.Content) != 1 ||
		result.Content[0].Type != "text" || !strings.HasPrefix(result.text(), prefix) {
		t.Fatalf("expected one text-only citation response with prefix %q: %s", prefix, result.Raw)
	}
	payload := strings.TrimPrefix(result.text(), prefix)
	decoder := json.NewDecoder(strings.NewReader(payload))
	var actualSource string
	if err := decoder.Decode(&actualSource); err != nil || actualSource != source {
		t.Fatalf("source changed: %q (%v)", actualSource, err)
	}
	quoteJSON, ok := strings.CutPrefix(payload[decoder.InputOffset():], " quote=")
	var actualQuote string
	if !ok || json.Unmarshal([]byte(quoteJSON), &actualQuote) != nil || actualQuote != quote {
		t.Fatalf("quote changed: got %d bytes, want %d", len(actualQuote), len(quote))
	}
}

func TestV1CitationFetchNamespaceAndAmbiguity(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const source = "memo/shared"
	const quote = "Shared exact evidence"
	const claim = "Shared citation assertion"
	for _, args := range []map[string]any{
		{"agent_id": memory.SharedAgentID, "text": claim, "source": source, "quote": quote},
		{"agent_id": "project", "text": "First project fact"},
		{"agent_id": "project", "text": "Second project fact"},
	} {
		if result := utilityContractCall(t, srv, "memory_add", args); result.IsError {
			t.Fatalf("seed: %s", result.Raw)
		}
	}
	search := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": "reader", "query": claim})
	marker, tail, _ := strings.Cut(search.text(), " ")
	if search.IsError || !strings.HasPrefix(tail, "[shared] ") {
		t.Fatalf("missing shared indicator: %s", search.Raw)
	}
	assertV1FetchedCitation(t, utilityContractCall(t, srv, "memory_search", map[string]any{
		"agent_id": memory.SharedAgentID, "id": marker,
	}), marker, source, quote)
	wrong := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": "reader", "id": marker})
	if !wrong.IsError {
		t.Fatalf("fetch crossed namespaces: %s", wrong.Raw)
	}
	facts, err := mem.Advanced().List("project")
	if err != nil || len(facts) != 2 || facts[0].ID[:1] != facts[1].ID[:1] {
		t.Fatalf("expected two project facts with a common ID prefix: %+v %v", facts, err)
	}
	ambiguous := utilityContractCall(t, srv, "memory_search", map[string]any{
		"agent_id": "project", "id": facts[0].ID[:1],
	})
	if !ambiguous.IsError || !strings.Contains(ambiguous.text(), "ambiguous") {
		t.Fatalf("missing visible ambiguity: %s", ambiguous.Raw)
	}
}

func TestV1SearchShortIDActsOnProjectFact(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const ns = "short-id-project"
	add := func(text string, replaces []string) {
		args := map[string]any{"agent_id": ns, "text": text}
		if replaces != nil {
			args["replaces"] = replaces
		}
		if result := utilityContractCall(t, srv, "memory_add", args); result.IsError {
			t.Fatalf("add %q: %s", text, result.Raw)
		}
	}
	currentID := func(claim string) string {
		t.Helper()
		result := utilityContractCall(t, srv, "memory_search", map[string]any{
			"agent_id": ns, "query": claim, "scope": "project", "top_k": 1,
		})
		id, _, ok := strings.Cut(result.text(), " ")
		if result.IsError || !ok || len(id) < 8 || !strings.Contains(result.text(), claim) {
			t.Fatalf("missing short ID for %q: %s", claim, result.Raw)
		}
		return id
	}
	reflect := func(action, id, text string) {
		t.Helper()
		args := map[string]any{"agent_id": ns, "action": action, "target": id}
		if text != "" {
			args["text"] = text
		}
		if result := utilityContractCall(t, srv, "memory_reflect", args); result.IsError {
			t.Fatalf("%s by short ID: %s", action, result.Raw)
		}
	}
	const initial = "Original short identifier fixture"
	add(initial, nil)
	id := currentID(initial)
	for _, action := range []string{"pin", "unpin"} {
		reflect(action, id, "")
		facts, err := mem.Advanced().List(ns)
		if err != nil || len(facts) != 1 || facts[0].Pinned != (action == "pin") {
			t.Fatalf("%s did not change the selected fact: %+v %v", action, facts, err)
		}
	}
	const revised = "Revised short identifier fixture"
	reflect("update", id, revised)
	add("Replacement short identifier fixture", []string{currentID(revised)})
	reflect("forget", currentID("Replacement short identifier fixture"), "")
	facts, err := mem.Advanced().List(ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		if !fact.IsSuperseded() {
			t.Fatalf("unexpected live fact after curation: %+v", fact)
		}
	}
}
