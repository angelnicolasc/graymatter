package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestA201CoreExplainReceipts(t *testing.T) {
	core, mem := utilityContractServer(t)
	full := New(NewDirectBackend(mem, nil), "utility-test", WithFullCatalog())
	const fact = "Deployment window opens on Thursday"
	seed := utilityContractCall(t, core, "memory_add", map[string]any{
		"agent_id": "project", "text": fact, "subject": "deployment window",
	})
	if seed.IsError {
		t.Fatalf("seed versioned fact: %s", seed.Raw)
	}
	shortID := utilityStoredShortID(t, seed)
	facts, err := mem.Advanced().List("project")
	if err != nil || len(facts) != 1 || !strings.HasPrefix(facts[0].ID, shortID) {
		t.Fatalf("seed did not return the committed identity: %s / %+v / %v", seed.Raw, facts, err)
	}
	args := map[string]any{"agent_id": "project", "query": "deployment", "explain": true}
	want := a2ExplainReceipts(t, utilityContractCall(t, full, "memory_search", args))
	result := utilityContractCall(t, core, "memory_search", args)
	got := a2ExplainReceipts(t, result)
	if len(got) != 1 || got[0].Provenance.FactID != facts[0].ID || len(got[0].Provenance.ContentHash) != 64 ||
		len(want) != 1 || want[0].Text != fact || !strings.Contains(result.text(), fact) {
		t.Fatalf("receipt does not identify the exact recalled version: core=%s full=%+v", result.Raw, want)
	}
	// The compact response already supplies the assertion once in indexed text.
	// Compare all ranking and provenance fields against full on the same store.
	got[0].Text, want[0].Text = "", ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("core receipt differs from full: got %+v, want %+v", got, want)
	}
}

func utilityStoredShortID(t *testing.T, result utilityContractResult) string {
	t.Helper()
	text, ok := strings.CutPrefix(result.text(), "Stored ")
	id, _, found := strings.Cut(text, ".")
	if result.IsError || result.Structured != nil || len(result.Content) != 1 || !ok || !found ||
		len(id) < 8 || strings.ContainsAny(id, ", \t\r\n") {
		t.Fatalf("missing text-only committed identity: %s", result.Raw)
	}
	return id
}

func utilityV1SearchFacts(t *testing.T, result utilityContractResult, canonical []memory.Fact) []memory.Fact {
	t.Helper()
	if result.IsError || result.Structured != nil || len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("expected one text block: %s", result.Raw)
	}
	var selected []memory.Fact
	seen := make(map[string]bool)
	for _, line := range strings.Split(result.text(), "\n") {
		id, rest, ok := strings.Cut(line, " ")
		rest, _ = strings.CutPrefix(rest, "[shared] ")
		if !ok || !strings.HasPrefix(rest, "\"") {
			continue
		}
		var text string
		if err := json.NewDecoder(strings.NewReader(rest)).Decode(&text); err != nil || len(id) < 8 {
			t.Fatalf("malformed V1 fact row: %s", line)
		}
		var matched *memory.Fact
		for i := range canonical {
			fact := &canonical[i]
			if !strings.HasPrefix(fact.ID, id) {
				continue
			}
			if matched != nil {
				t.Fatalf("ambiguous V1 identity %q: %s", id, result.Raw)
			}
			matched = fact
		}
		if matched == nil || matched.Text != text || seen[matched.ID] {
			t.Fatalf("V1 row does not match one canonical fact: %s", result.Raw)
		}
		seen[matched.ID] = true
		selected = append(selected, *matched)
	}
	return selected
}

func a2ExplainReceipts(t *testing.T, result utilityContractResult) []memory.RecallReceipt {
	t.Helper()
	var payload struct {
		Count     int                    `json:"count"`
		Explained []memory.RecallReceipt `json:"explained"`
	}
	encoded, err := json.Marshal(result.Structured)
	if result.IsError || err != nil || json.Unmarshal(encoded, &payload) != nil ||
		len(payload.Explained) == 0 || payload.Count != len(payload.Explained) {
		t.Fatalf("missing top-level count and explained receipts: %s", result.Raw)
	}
	for _, receipt := range payload.Explained {
		if receipt.Provenance.FactID == "" || receipt.Provenance.WrittenAt.IsZero() ||
			receipt.Ranks.K != 60 || receipt.Ranks.RecencyRank < 1 || receipt.Ranks.FusedScore <= 0 {
			t.Fatalf("incomplete ranking or provenance: %s", result.Raw)
		}
	}
	return payload.Explained
}

type a2MissingReceiptBackend struct{ *DirectBackend }

func (*a2MissingReceiptBackend) SearchScoped(context.Context, string, []string, int, bool, memory.RecallOptions) (memory.ScopedSearchResult, error) {
	return memory.ScopedSearchResult{Facts: []memory.SearchFact{{ID: "fact-id", Text: "Fact without a receipt"}}}, nil
}

func TestA2CoreExplainErrorsAreVisible(t *testing.T) {
	_, mem := utilityContractServer(t)
	core := New(&a2MissingReceiptBackend{DirectBackend: NewDirectBackend(mem, nil)}, "missing-receipt")
	for _, probe := range []struct {
		name, errorText string
		explain         any
	}{
		{"missing receipt", "receipt", true},
		{"malformed flag", "explain", "true"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			result := utilityContractCall(t, core, "memory_search", map[string]any{
				"agent_id": "project", "query": "fact", "explain": probe.explain,
			})
			if !result.IsError || !strings.Contains(result.text(), probe.errorText) {
				t.Fatalf("expected a visible explain error: %s", result.Raw)
			}
		})
	}
}

func TestA202HiddenCatalogRuntime(t *testing.T) {
	core, mem := utilityContractServer(t)
	full := New(NewDirectBackend(mem, nil), "utility-test", WithFullCatalog())
	store := mem.Advanced()
	properties := func(tool map[string]any) map[string]any {
		return tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	}
	visible := make(map[string]map[string]any)
	for _, tool := range profileTools(t, core) {
		visible[tool["name"].(string)] = properties(tool)
	}
	type hiddenProbe struct{ tool, parameter string }
	var probes []hiddenProbe
	for _, tool := range profileTools(t, full) {
		name := tool["name"].(string)
		if visible[name] == nil {
			probes = append(probes, hiddenProbe{tool: name})
		}
		var hidden []string
		for field := range properties(tool) {
			if _, listed := visible[name][field]; !listed {
				hidden = append(hidden, field)
			}
		}
		sort.Strings(hidden)
		for _, field := range hidden {
			probes = append(probes, hiddenProbe{tool: name, parameter: field})
		}
	}
	if len(probes) == 0 {
		t.Fatal("the advertised catalogs produced no hidden entries")
	}
	for i, probe := range probes {
		name := probe.tool
		if probe.parameter != "" {
			name += "/" + probe.parameter
		}
		t.Run(name, func(t *testing.T) {
			// Separate namespaces preserve identical preconditions for writes.
			want := a2HiddenEffect(t, full, store, fmt.Sprintf("a2-full-%d", i), probe.tool, probe.parameter)
			got := a2HiddenEffect(t, core, store, fmt.Sprintf("a2-core-%d", i), probe.tool, probe.parameter)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("effect differs from full: got %v, want %v", got, want)
			}
			t.Log("same effect as full")
		})
	}
}

type a2FactLister interface {
	List(string) ([]memory.Fact, error)
}

func a2HiddenEffect(t *testing.T, server *Server, store a2FactLister, namespace, tool, parameter string) []string {
	t.Helper()
	args := map[string]any{"agent_id": namespace}
	const text = "Deployment uses the eastern harbor"
	wantKind, wantConfidence := memory.KindFact, "inferred"
	switch tool {
	case "memory_alias":
		switch parameter {
		case "", "agent_id", "term", "equivalents":
		default:
			t.Fatalf("no probe for %s.%s", tool, parameter)
		}
		args["term"], args["equivalents"] = "harbor", []string{"port"}
		wantKind = memory.KindAlias
	case "memory_add", "memory_reflect":
		args["text"] = text
		if tool == "memory_reflect" {
			args["action"] = "add"
		}
		switch {
		case parameter == "confidence":
			args["confidence"], wantConfidence = "verified", "verified"
		case tool == "memory_reflect" && parameter == "agent":
			delete(args, "agent_id")
			args["agent"] = namespace
		default:
			t.Fatalf("no probe for %s.%s", tool, parameter)
		}
	case "checkpoint_resume":
		if parameter != "on_missing" {
			t.Fatalf("no probe for %s.%s", tool, parameter)
		}
		return a2MissingCheckpointEffect(t, server, namespace)
	case "memory_search", "memory_search_batch":
		return a2HiddenSearch(t, server, store, namespace, tool, parameter, args)
	default:
		t.Fatalf("no probe for %s.%s", tool, parameter)
	}
	result := utilityContractCall(t, server, tool, args)
	facts, err := store.List(namespace)
	if result.IsError || err != nil || len(facts) != 1 || facts[0].AgentID != namespace || facts[0].Kind != wantKind ||
		memory.EffectiveConfidence(facts[0].Confidence) != wantConfidence {
		t.Fatalf("expected persisted namespace, kind and confidence: %s / %+v / %v", result.Raw, facts, err)
	}
	if wantKind == memory.KindAlias {
		if facts[0].Text != "alias: harbor = port" {
			t.Fatalf("alias mapping was not written: %+v", facts[0])
		}
	} else if facts[0].Text != text {
		t.Fatalf("assertion was not written: %+v", facts[0])
	}
	return []string{facts[0].Text, facts[0].Kind, facts[0].Confidence}
}

func a2HiddenSearch(t *testing.T, server *Server, store a2FactLister, namespace, tool, parameter string, args map[string]any) []string {
	t.Helper()
	const verified = "Deployment policy verified"
	const unverified = "Deployment policy unverified"
	for _, entry := range []struct{ text, confidence string }{{verified, "verified"}, {unverified, "unverified"}} {
		seed := utilityContractCall(t, server, "memory_add", map[string]any{
			"agent_id": namespace, "text": entry.text, "confidence": entry.confidence,
		})
		if seed.IsError {
			t.Fatalf("seed confidence fixture: %s", seed.Raw)
		}
	}
	batch := tool == "memory_search_batch"
	if batch {
		args["queries"] = []string{"deployment", "policy"}
		if !server.fullCatalog {
			args["explain"] = true
		}
	} else {
		args["query"] = "deployment"
	}
	wantCount := 2
	switch {
	case batch && (parameter == "" || parameter == "agent_id" || parameter == "queries"):
	case batch && parameter == "top_k":
		args["top_k"], wantCount = 1, 1
	case parameter == "min_confidence":
		args[parameter], wantCount = "verified", 1
	case parameter == "confidence_weight":
		args[parameter] = .5
	case !batch && parameter == "explain":
		args[parameter] = true
	default:
		t.Fatalf("no probe for %s.%s", tool, parameter)
	}
	result := utilityContractCall(t, server, tool, args)
	if result.IsError {
		t.Fatalf("hidden call failed: %s", result.Raw)
	}
	facts, err := store.List(namespace)
	if err != nil || len(facts) != 2 {
		t.Fatalf("expected two canonical fixture facts: %+v / %v", facts, err)
	}
	byID := make(map[string]string, len(facts))
	for _, fact := range facts {
		byID[fact.ID] = fact.Text
	}
	if parameter == "explain" {
		var selected []string
		for _, receipt := range a2ExplainReceipts(t, result) {
			selected = append(selected, byID[receipt.Provenance.FactID])
		}
		if len(selected) != 2 || selected[0] == "" || selected[1] == "" || selected[0] == selected[1] {
			t.Fatalf("receipts must identify both selected facts: %s", result.Raw)
		}
		return selected
	}
	rows, ok := result.Structured["merged"].([]any)
	if !ok {
		rows, _ = result.Structured["facts"].([]any)
	}
	var selected []string
	if result.Structured == nil {
		for _, fact := range utilityV1SearchFacts(t, result, facts) {
			selected = append(selected, fact.Text)
		}
	} else {
		for _, row := range rows {
			switch value := row.(type) {
			case string:
				selected = append(selected, value)
			case map[string]any:
				id, _ := value["id"].(string)
				selected = append(selected, byID[id])
			}
		}
	}
	if len(selected) != wantCount || selected[0] != verified && selected[0] != unverified ||
		len(selected) == 2 && (selected[0] == selected[1] || selected[1] != verified && selected[1] != unverified) {
		t.Fatalf("unexpected selection: %s", result.Raw)
	}
	if parameter == "min_confidence" && selected[0] != verified {
		t.Fatalf("minimum confidence did not filter: %s", result.Raw)
	}
	if parameter == "confidence_weight" {
		weightApplied := strings.Contains(result.text(), "confidence_weight=0.5")
		if retrieval, ok := result.Structured["retrieval"].(map[string]any); ok {
			weightApplied = retrieval["confidence_weight"] == .5
		}
		if !weightApplied || selected[0] != verified {
			t.Fatalf("confidence weight did not prefer the older verified fact: %s", result.Raw)
		}
	}
	if batch {
		queries, _ := result.Structured["per_query"].([]any)
		if len(queries) != 2 {
			t.Fatalf("batch did not answer both queries: %s", result.Raw)
		}
		for _, row := range queries {
			matches, ok := row.([]any)
			if legacy, isLegacy := row.(map[string]any); !ok && isLegacy {
				matches, _ = legacy["facts"].([]any)
			}
			if len(matches) != wantCount {
				t.Fatalf("batch query did not apply the same selection limit: %s", result.Raw)
			}
		}
	}
	return selected
}

func TestV203CoreCatalogRemovals(t *testing.T) {
	core, mem := utilityContractServer(t)
	full := New(NewDirectBackend(mem, nil), "utility-test", WithFullCatalog())
	properties := func(srv *Server) map[string]map[string]any {
		out := make(map[string]map[string]any)
		for _, tool := range profileTools(t, srv) {
			out[tool["name"].(string)] = tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		}
		return out
	}
	coreProps, fullProps := properties(core), properties(full)
	for _, row := range []struct {
		tool   string
		fields []string
	}{
		{"memory_search", []string{"scope", "fact_id", "content_hash", "evidence_budget_bytes"}},
		{"memory_reflect", []string{"target_type", "source", "quote", "subject", "replaces"}},
		{"checkpoint_resume", []string{"on_missing"}},
	} {
		if coreProps[row.tool] == nil {
			t.Errorf("%s is missing from the core catalog", row.tool)
			continue
		}
		for _, field := range row.fields {
			t.Run(row.tool+"/"+field, func(t *testing.T) {
				if _, listed := coreProps[row.tool][field]; listed {
					t.Errorf("%s.%s is still listed in core", row.tool, field)
				}
			})
		}
	}
	for _, field := range []string{"source", "quote", "subject", "replaces"} {
		if _, listed := fullProps["memory_reflect"][field]; listed {
			t.Errorf("memory_reflect.%s was added to full", field)
		}
	}
	action, _ := coreProps["memory_reflect"]["action"].(map[string]any)
	enum, _ := action["enum"].([]any)
	var actions []string
	for _, value := range enum {
		action, _ := value.(string)
		actions = append(actions, action)
	}
	sort.Strings(actions)
	if want := []string{"forget", "pin", "unpin", "update"}; !reflect.DeepEqual(actions, want) {
		t.Errorf("core reflect actions = %v, want %v", actions, want)
	}
	add := coreProps["memory_add"]
	facts, _ := add["facts"].(map[string]any)
	items, _ := facts["items"].(map[string]any)
	if facts["type"] != "array" || items["type"] != "object" || !reflect.DeepEqual(items["required"], []any{"text"}) {
		t.Errorf("memory_add.facts lost its array of objects requiring text: %v", facts)
	}
	wantSubject := map[string]any{"type": "string"}
	if !reflect.DeepEqual(items["properties"], map[string]any{"subject": wantSubject}) {
		t.Errorf("memory_add.facts must declare only subject: %v", items["properties"])
	}
	if !reflect.DeepEqual(add["subject"], wantSubject) {
		t.Errorf("memory_add lost its top-level subject: %v", add["subject"])
	}
}

func TestV203HiddenCatalogRuntime(t *testing.T) {
	core, mem := utilityContractServer(t)
	call := func(t *testing.T, tool, namespace string, args map[string]any) utilityContractResult {
		t.Helper()
		args["agent_id"] = namespace
		return utilityContractCall(t, core, tool, args)
	}
	list := func(t *testing.T, namespace string) []memory.Fact {
		t.Helper()
		facts, err := mem.Advanced().List(namespace)
		if err != nil {
			t.Fatal(err)
		}
		return facts
	}
	write := func(t *testing.T, tool, namespace string, args map[string]any) {
		t.Helper()
		if result := call(t, tool, namespace, args); result.IsError {
			t.Fatalf("%s failed: %s", tool, result.Raw)
		}
	}
	const source, quote = "memo/v203", "Exact words from the deployment memo"
	const before, after = "Deployment window opens Tuesday", "Deployment window opens Thursday"

	for _, row := range []struct {
		name  string
		probe func(*testing.T, string)
	}{
		{"memory_search/scope", func(t *testing.T, ns string) {
			write(t, "memory_add", ns, map[string]any{"text": before})
			write(t, "memory_add", memory.SharedAgentID, map[string]any{"text": after})
			all := call(t, "memory_search", ns, map[string]any{"query": "deployment window"})
			project := call(t, "memory_search", ns, map[string]any{"query": "deployment window", "scope": "project"})
			if all.IsError || !strings.Contains(all.text(), after) || project.IsError ||
				!strings.Contains(project.text(), before) || strings.Contains(project.text(), after) {
				t.Fatalf("scope did not exclude the shared fact: default=%s project=%s", all.Raw, project.Raw)
			}
		}},
		{"memory_search/fact_id_and_content_hash", func(t *testing.T, ns string) {
			write(t, "memory_add", ns, map[string]any{"text": before, "source": source, "quote": quote})
			fact := list(t, ns)[0]
			hash := memory.NewEvidenceSnapshot(fact).ContentHash
			result := call(t, "memory_search", ns, map[string]any{"fact_id": fact.ID, "content_hash": hash})
			assertV1FetchedCitation(t, result, "Citation", source, quote)
			for _, args := range []map[string]any{
				{"fact_id": "missing", "content_hash": hash},
				{"fact_id": fact.ID, "content_hash": strings.Repeat("0", 64)},
			} {
				if bad := call(t, "memory_search", ns, args); !bad.IsError || bad.text() == "" {
					t.Fatalf("invalid identity or version produced no visible error: %s", bad.Raw)
				}
			}
		}},
		{"memory_search/evidence_budget_bytes", func(t *testing.T, ns string) {
			write(t, "memory_add", ns, map[string]any{"text": before, "source": source, "quote": quote})
			args := map[string]any{"query": before, "scope": "project"}
			ordinary := call(t, "memory_search", ns, args)
			args["evidence_budget_bytes"] = 0
			limited := call(t, "memory_search", ns, args)
			if ordinary.IsError || !strings.Contains(ordinary.text(), quote) || limited.IsError ||
				!strings.Contains(limited.text(), " citation=omitted") || strings.Contains(limited.text(), quote) {
				t.Fatalf("budget did not omit the complete quote: default=%s limited=%s", ordinary.Raw, limited.Raw)
			}
		}},
		{"memory_reflect/target_type", func(t *testing.T, ns string) {
			write(t, "memory_add", ns, map[string]any{"text": before})
			id := list(t, ns)[0].ID
			write(t, "memory_add", ns, map[string]any{"text": id})
			write(t, "memory_reflect", ns, map[string]any{"action": "pin", "target": id, "target_type": "id"})
			facts := list(t, ns)
			if len(facts) != 2 {
				t.Fatalf("expected two targets: %+v", facts)
			}
			for _, fact := range facts {
				if fact.Pinned != (fact.ID == id) {
					t.Fatalf("target_type=id did not select the ID over exact text: %+v", facts)
				}
			}
		}},
		{"memory_reflect/source_and_quote", func(t *testing.T, ns string) {
			write(t, "memory_add", ns, map[string]any{"text": before})
			write(t, "memory_reflect", ns, map[string]any{
				"action": "update", "target": before, "text": after, "source": source, "quote": quote,
			})
			fact := v203Replacement(t, list(t, ns), before, after)
			if fact.Evidence == nil || fact.Evidence.Source != source || fact.Evidence.Quote != quote {
				t.Fatalf("hidden citation fields did not survive the update: %+v", fact)
			}
		}},
		{"memory_reflect/subject", func(t *testing.T, ns string) {
			for _, text := range []string{before, after} {
				write(t, "memory_reflect", ns, map[string]any{"action": "add", "text": text, "subject": "deployment window"})
			}
			fact := v203Replacement(t, list(t, ns), before, after)
			if want := []memory.SubjectProjection{{Name: "deployment window", Revisions: 1}}; !reflect.DeepEqual(fact.Subjects, want) {
				t.Fatalf("hidden subject did not advance the head: %+v", fact)
			}
		}},
		{"memory_reflect/replaces", func(t *testing.T, ns string) {
			write(t, "memory_add", ns, map[string]any{"text": before})
			id := list(t, ns)[0].ID
			write(t, "memory_reflect", ns, map[string]any{"action": "add", "text": after, "replaces": []string{id}})
			v203Replacement(t, list(t, ns), before, after)
		}},
		{"memory_reflect/action_add", func(t *testing.T, ns string) {
			write(t, "memory_reflect", ns, map[string]any{"action": "add", "text": before})
			if facts := list(t, ns); len(facts) != 1 || facts[0].Text != before {
				t.Fatalf("hidden add did not persist one fact: %+v", facts)
			}
		}},
		{"memory_reflect/action_link", func(t *testing.T, ns string) {
			result := call(t, "memory_reflect", ns, map[string]any{"action": "link", "text": "node-a", "target": "node-b"})
			if !result.IsError || !strings.Contains(result.text(), "knowledge graph") {
				t.Fatalf("hidden link did not report unavailable graph: %s", result.Raw)
			}
		}},
		{"checkpoint_resume/on_missing", func(t *testing.T, ns string) {
			ordinary := call(t, "checkpoint_resume", ns, map[string]any{})
			if ordinary.IsError || ordinary.text() != "No pending checkpoint." {
				t.Fatalf("missing checkpoint default changed: %s", ordinary.Raw)
			}
			a2MissingCheckpointEffect(t, core, ns)
		}},
		{"memory_add/facts_item_fields", func(t *testing.T, ns string) {
			write(t, "memory_add", ns, map[string]any{"text": before, "subject": "deployment window"})
			write(t, "memory_add", ns, map[string]any{"text": "Original quay assignment"})
			var replaceID string
			for _, fact := range list(t, ns) {
				if fact.Text == "Original quay assignment" {
					replaceID = fact.ID
				}
			}
			write(t, "memory_add", ns, map[string]any{"facts": []map[string]any{
				{"text": after, "source": source, "quote": quote, "subject": "deployment window"},
				{"text": "Updated quay assignment", "replaces": []string{replaceID}},
			}})
			facts := list(t, ns)
			deployment := make([]memory.Fact, 0, 2)
			quay := make([]memory.Fact, 0, 2)
			for _, fact := range facts {
				if strings.HasPrefix(fact.Text, "Deployment window") {
					deployment = append(deployment, fact)
				} else {
					quay = append(quay, fact)
				}
			}
			current := v203Replacement(t, deployment, before, after)
			v203Replacement(t, quay, "Original quay assignment", "Updated quay assignment")
			if current.Evidence == nil || current.Evidence.Source != source || current.Evidence.Quote != quote ||
				!reflect.DeepEqual(current.Subjects, []memory.SubjectProjection{{Name: "deployment window", Revisions: 1}}) {
				t.Fatalf("nested citation or subject was lost: %+v", current)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.probe(t, "v203-"+strings.ReplaceAll(row.name, "/", "-"))
		})
	}
}

func v203Replacement(t *testing.T, facts []memory.Fact, before, after string) memory.Fact {
	t.Helper()
	if len(facts) != 2 {
		t.Fatalf("expected predecessor and replacement: %+v", facts)
	}
	var predecessor, current memory.Fact
	for _, fact := range facts {
		switch fact.Text {
		case before:
			predecessor = fact
		case after:
			current = fact
		}
	}
	if predecessor.ID == "" || current.ID == "" || predecessor.SupersededBy != current.ID || current.IsSuperseded() {
		t.Fatalf("replacement did not leave exactly one current fact: %+v", facts)
	}
	return current
}

func a2MissingCheckpointEffect(t *testing.T, server *Server, namespace string) []string {
	t.Helper()
	result := utilityContractCall(t, server, "checkpoint_resume", map[string]any{
		"agent_id": namespace, "on_missing": "empty",
	})
	absent := result.Structured["found"] == false && result.Structured["agent_id"] == namespace &&
		strings.Contains(result.text(), "No checkpoint saved")
	if !server.fullCatalog {
		absent = result.Structured == nil && result.text() == "No pending checkpoint."
	}
	if result.IsError || !absent {
		t.Fatalf("on_missing=empty did not return visible absence: %s", result.Raw)
	}
	return []string{"found=false"}
}
