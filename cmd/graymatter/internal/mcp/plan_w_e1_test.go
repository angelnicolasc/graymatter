package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const planWE1Namespace = "evidence-project"

func planWE1Args(agent, claim, source, quote string) map[string]any {
	return map[string]any{"agent_id": agent, "text": claim, "source": source, "quote": quote}
}

func planWE1Add(t *testing.T, srv *Server, args map[string]any) {
	t.Helper()
	if result := utilityContractCall(t, srv, "memory_add", args); result.IsError {
		t.Fatalf("write rejected: %s", result.Raw)
	}
}

func planWE1List(t *testing.T, mem *graymatter.Memory, namespace string) map[string]memory.Fact {
	t.Helper()
	rows, err := mem.Advanced().List(namespace)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]memory.Fact, len(rows))
	for _, row := range rows {
		if !row.IsSuperseded() {
			result[row.Text] = row
		}
	}
	return result
}

func planWE1ExpectedError(t *testing.T, result utilityContractResult, want string) {
	t.Helper()
	if !result.IsError || result.Structured != nil || len(result.Content) != 1 ||
		result.Content[0].Type != "text" || result.text() != want {
		t.Fatalf("want error %q, got %s", want, result.Raw)
	}
}

func planWE1StoredQuote(t *testing.T, facts map[string]memory.Fact, claim, quote string) memory.Fact {
	t.Helper()
	row, exists := facts[claim]
	if !exists || row.Evidence == nil || row.Evidence.Quote != quote {
		t.Fatalf("expected %q to retain stored quote %q: %+v", claim, quote, row)
	}
	return row
}

func TestPlanWE1ExistingAndBatchConflictsAreAtomic(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const source = "handbook/\"operations\"\t"
	const retained = "Retain the seal.\nKeep it intact."
	const disjoint = "Send the notice."
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, "Assembly retains the seal.", source, retained))
	wantExisting := fmt.Sprintf("Not stored. Source %q already has this quote: %q. One source keeps one quote. Resend with a quote that contains it, the whole source text, or reuse it exactly.", source, retained)
	planWE1ExpectedError(t, utilityContractCall(t, srv, "memory_add",
		planWE1Args(planWE1Namespace, "Assembly sends the notice.", source, disjoint)), wantExisting)
	planWE1ExpectedError(t, utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": planWE1Namespace,
		"facts": []map[string]any{
			{"text": "Archive holds another note.", "source": "other", "quote": "Other note."},
			{"text": "Assembly sends the notice.", "source": source, "quote": disjoint},
		},
	}), wantExisting)
	if rows := planWE1List(t, mem, planWE1Namespace); len(rows) != 1 {
		t.Fatalf("rejected calls changed the store: %+v", rows)
	}

	const emptyScope = "another-project"
	wantBatch := fmt.Sprintf("Not stored. Facts from source %q carry different quotes. One source keeps one quote: give the whole source text once for the call.", source)
	planWE1ExpectedError(t, utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": emptyScope,
		"source":   source, "quote": retained,
		"facts": []map[string]any{
			{"text": "Assembly retains the seal."},
			{"text": "Assembly sends the notice.", "quote": disjoint},
			{"text": "Other note remains independent.", "source": "other"},
		},
	}), wantBatch)
	if rows := planWE1List(t, mem, emptyScope); len(rows) != 0 {
		t.Fatalf("intra-call rejection changed the store: %+v", rows)
	}
}

func TestPlanWE1WritesKeepAdmittedQuotesAndNamespaces(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const source = "release-note"
	const short = "Open the gate."
	const long = "For the east wing, open the gate. Open the gate."
	const contained = "gate"
	for _, entry := range []struct {
		claim, quote string
	}{
		{"First gate instruction.", short},
		{"Longer gate instruction.", long},
		{"Contained gate instruction.", contained},
		{"Duplicate quote for gate instruction.", long},
	} {
		planWE1Add(t, srv, planWE1Args(planWE1Namespace, entry.claim, source, entry.quote))
		planWE1StoredQuote(t, planWE1List(t, mem, planWE1Namespace), entry.claim, entry.quote)
	}
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, "Independent source.", "other", "Disjoint evidence."))
	planWE1Add(t, srv, planWE1Args(memory.SharedAgentID, "Independent shared.", source, "Shared-only evidence."))
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, "Shared quote does not block this project write.", source, long))
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, "Missing quote is exempt.", source, ""))
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, "Missing source is exempt.", "", "Unrelated."))
	planWE1StoredQuote(t, planWE1List(t, mem, memory.SharedAgentID), "Independent shared.", "Shared-only evidence.")
	if rows := planWE1List(t, mem, planWE1Namespace); len(rows) != 8 {
		t.Fatalf("expected eight independent project writes, got %d", len(rows))
	}
}

func TestPlanWE1ReplacedAndRetiredFactsAreIgnored(t *testing.T) {
	srv, mem := utilityContractServer(t)
	for _, retire := range []bool{false, true} {
		t.Run(fmt.Sprintf("retire=%t", retire), func(t *testing.T) {
			const source = "standing-note"
			scope := fmt.Sprintf("retirement-%t", retire)
			const original = "Initial assembly instruction."
			planWE1Add(t, srv, planWE1Args(scope, original, source, "Old instruction."))
			first := planWE1StoredQuote(t, planWE1List(t, mem, scope), original, "Old instruction.")
			if retire {
				result := utilityContractCall(t, srv, "memory_reflect", map[string]any{
					"agent_id": scope, "action": "forget", "target": first.ID, "target_type": "id",
				})
				if result.IsError {
					t.Fatalf("retirement failed: %s", result.Raw)
				}
			} else {
				replacement := planWE1Args(scope, "Revised assembly instruction.", "replacement-note", "Replacement instruction.")
				replacement["replaces"] = []string{first.ID}
				planWE1Add(t, srv, replacement)
			}
			const newClaim = "New independent evidence on the old source."
			planWE1Add(t, srv, planWE1Args(scope, newClaim, source, "New instruction."))
			rows := planWE1List(t, mem, scope)
			if _, exists := rows[original]; exists {
				t.Fatal("previous fact is still live")
			}
			planWE1StoredQuote(t, rows, newClaim, "New instruction.")
		})
	}
}

func TestPlanWE1ConflictSurvivesStoreReopen(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.OllamaURL = "disabled://utility-contract"
	cfg.AsyncConsolidate = false
	cfg.ConsolidateLLM = ""
	cfg.VectorReconcileInterval = 0
	open := func() *graymatter.Memory {
		t.Helper()
		mem, err := graymatter.NewWithConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return mem
	}
	mem := open()
	defer func() { _ = mem.Close() }()
	srv := New(NewDirectBackend(mem, nil), "evidence-reopen")
	const claim = "The inspection stores the old entry."
	const source = "inspection"
	const quote = "Log the reading."
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, claim, source, quote))
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
	mem = open()
	srv = New(NewDirectBackend(mem, nil), "evidence-reopen")
	planWE1ExpectedError(t, utilityContractCall(t, srv, "memory_add",
		planWE1Args(planWE1Namespace, "A separate inspection entry.", source, "Close the enclosure.")),
		fmt.Sprintf("Not stored. Source %q already has this quote: %q. One source keeps one quote. Resend with a quote that contains it, the whole source text, or reuse it exactly.", source, quote))
	rows := planWE1List(t, mem, planWE1Namespace)
	if len(rows) != 1 {
		t.Fatalf("reopened store has %d facts, want 1", len(rows))
	}
	planWE1StoredQuote(t, rows, claim, quote)
}

func planWE1JSONString(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func planWE1Row(t *testing.T, search utilityContractResult, claim string) (string, string) {
	t.Helper()
	if search.IsError || search.Structured != nil {
		t.Fatalf("expected compact results: %s", search.Raw)
	}
	for _, line := range strings.Split(search.text(), "\n") {
		id, payload, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		shared, _ := strings.CutPrefix(payload, "[shared] ")
		if suffix, found := strings.CutPrefix(shared, planWE1JSONString(t, claim)); found {
			return id, suffix
		}
	}
	t.Fatalf("no row for %q: %s", claim, search.Raw)
	return "", ""
}

func planWE1Search(t *testing.T, srv *Server, query string, budget int) utilityContractResult {
	t.Helper()
	return utilityContractCall(t, srv, "memory_search", map[string]any{
		"agent_id": planWE1Namespace, "query": query, "scope": "project", "top_k": 10,
		"evidence_budget_bytes": budget,
	})
}

func TestPlanWE1InheritedBatchQuotesAndReadBudget(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const source = "field-guide"
	const short = "Seamtest closes."
	const long = "Seamtest closes.\nThe shield covers \"edge\" & latch."
	planWE1Add(t, srv, map[string]any{
		"agent_id": planWE1Namespace,
		"source":   source, "quote": short,
		"facts": []map[string]any{
			{"text": short},
			{"text": long, "quote": long},
		},
	})
	facts := planWE1List(t, mem, planWE1Namespace)
	storedShort := planWE1StoredQuote(t, facts, short, short)
	planWE1StoredQuote(t, facts, long, long)
	sourceSuffix := " source=" + planWE1JSONString(t, source)
	fullSuffix := " quote=" + planWE1JSONString(t, long)
	search := planWE1Search(t, srv, "Seamtest", 8192)
	shortID, shortSuffix := planWE1Row(t, search, short)
	longID, longSuffix := planWE1Row(t, search, long)
	if shortSuffix != sourceSuffix+fullSuffix || longSuffix != sourceSuffix+" literal" {
		t.Fatalf("short row=%q, long row=%q", shortSuffix, longSuffix)
	}
	for _, id := range []string{shortID, longID} {
		fetch := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": planWE1Namespace, "id": id})
		assertV1FetchedCitation(t, fetch, id, source, long)
	}
	legacy := utilityContractCall(t, srv, "memory_search", map[string]any{
		"agent_id": planWE1Namespace, "fact_id": storedShort.ID,
		"content_hash": memory.NewEvidenceSnapshot(storedShort).ContentHash,
	})
	assertV1FetchedCitation(t, legacy, "Citation", source, short)

	fullCost := len(planWE1JSONString(t, fullSuffix)) - 2
	literalCost := len(planWE1JSONString(t, " literal")) - 2
	_, insufficient := planWE1Row(t, planWE1Search(t, srv, "Seamtest", fullCost-1), short)
	_, sufficient := planWE1Row(t, planWE1Search(t, srv, "Seamtest", fullCost+literalCost), short)
	if insufficient != sourceSuffix+" citation=omitted" || sufficient != sourceSuffix+fullSuffix {
		t.Fatalf("short row at %d bytes = %q, at %d bytes = %q", fullCost-1, insufficient, fullCost+literalCost, sufficient)
	}
}

func TestPlanWE1LegacyDisjointSources(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const source = "archive"
	const left = "Left rotor opens."
	const leftFull = "Left rotor opens. It remains latched."
	const right = "Right rotor closes."
	const both = "Left rotor opens. It remains latched. Right rotor closes."
	backend := NewDirectBackend(mem, nil)
	sourceName := source
	for _, entry := range []struct{ claim, quote string }{
		{"Rotor archive left row.", left},
		{"Rotor archive expanded row.", leftFull},
		{"Rotor archive right row.", right},
	} {
		if _, err := backend.PutManyReturningReceipts(context.Background(), planWE1Namespace, []memory.WriteFact{{
			Text: entry.claim, Options: memory.WriteOptions{Source: &sourceName, Quote: &entry.quote},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	assertRead := func(claim, quote string) {
		t.Helper()
		result := planWE1Search(t, srv, "Rotor archive", 8192)
		id, suffix := planWE1Row(t, result, claim)
		want := " source=" + planWE1JSONString(t, source) + " quote=" + planWE1JSONString(t, quote)
		if suffix != want {
			t.Fatalf("%q search row = %q, want %q", claim, suffix, want)
		}
		assertV1FetchedCitation(t, utilityContractCall(t, srv, "memory_search",
			map[string]any{"agent_id": planWE1Namespace, "id": id}), id, source, quote)
	}
	assertRead("Rotor archive left row.", leftFull)
	assertRead("Rotor archive expanded row.", leftFull)
	assertRead("Rotor archive right row.", right)
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, "Rotor archive exact reuse.", source, leftFull))
	reject := utilityContractCall(t, srv, "memory_add",
		planWE1Args(planWE1Namespace, "Rotor archive partial extension.", source, leftFull+" Inspect the latch."))
	if !reject.IsError || !strings.Contains(reject.text(), "One source keeps one quote.") {
		t.Fatalf("quote extending only one branch accepted: %s", reject.Raw)
	}
	if got := planWE1List(t, mem, planWE1Namespace); len(got) != 4 {
		t.Fatalf("failed extension changed store: %d live facts", len(got))
	}
	planWE1Add(t, srv, planWE1Args(planWE1Namespace, "Rotor archive combined row.", source, both))
	for claim := range planWE1List(t, mem, planWE1Namespace) {
		assertRead(claim, both)
	}
}

func TestPlanWE1LongRejectionMessageAndCatalog(t *testing.T) {
	for _, size := range []int{600, 601} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			srv, mem := utilityContractServer(t)
			const source = "long/\"manual\""
			const claim = "The protocol stores a long citation."
			quote := strings.Repeat("x", size)
			planWE1Add(t, srv, planWE1Args(planWE1Namespace, claim, source, quote))
			want := fmt.Sprintf("Not stored. Source %q already has this quote: %q. One source keeps one quote. Resend with a quote that contains it, the whole source text, or reuse it exactly.", source, quote)
			if size > 600 {
				id, _ := planWE1Row(t, planWE1Search(t, srv, "protocol", 8192), claim)
				full := planWE1List(t, mem, planWE1Namespace)[claim].ID
				if id == full || !strings.HasPrefix(full, id) {
					t.Fatalf("expected short ID for %q, got %q", full, id)
				}
				want = fmt.Sprintf("Not stored. Source %q already has a quote on %s; read it with memory_search id. One source keeps one quote. Resend with a quote that contains it, the whole source text, or reuse it exactly.", source, id)
			}
			planWE1ExpectedError(t, utilityContractCall(t, srv, "memory_add",
				planWE1Args(planWE1Namespace, "Other protocol evidence.", source, "Different quote.")), want)
		})
	}

	const want = "Store text or facts objects. For evidence give source and quote: the whole source text or paragraph, the same on every fact from that source. A subject names one current fact. In facts, set it inside each object; facts in one call never share a subject."
	if len(want) != 253 {
		t.Fatalf("expected 253 description bytes, got %d", len(want))
	}
	for _, tool := range profileTools(t, newOptionsServer(t)) {
		if tool["name"] == "memory_add" {
			if tool["description"] != want {
				t.Fatalf("memory_add description = %q, want %q", tool["description"], want)
			}
			return
		}
	}
	t.Fatal("memory_add missing from core catalog")
}

func TestPlanWE1ConcurrentDisjointWrites(t *testing.T) {
	srv, mem := utilityContractServer(t)
	start := make(chan struct{})
	type result struct {
		failed bool
		err    error
	}
	results := make(chan result, 2)
	for _, quote := range []string{"Lock the west latch.", "Open the east hatch."} {
		go func(quote string) {
			<-start
			res, err := srv.handleMemoryAdd(context.Background(), reflectReq(planWE1Args(planWE1Namespace, quote, "concurrency-note", quote)))
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{failed: res.IsError}
		}(quote)
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.failed == second.failed {
		t.Fatalf("expected one successful and one rejected write, got %+v and %+v", first, second)
	}
	if got := planWE1List(t, mem, planWE1Namespace); len(got) != 1 {
		t.Fatalf("concurrent calls committed %d facts, want one", len(got))
	}
}
