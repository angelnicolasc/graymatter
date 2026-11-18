package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestSubjectMCPReplacementReceiptsAndIDTargets(t *testing.T) {
	srv, mem := utilityContractServer(t)
	old := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": "Retention is 11 days", "subject": " CAFÉ\t政策 "})
	if old.IsError {
		t.Fatal(string(old.Raw))
	}
	oldID := compactTestWriteIDs(t, old)[0]
	next := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": "Retention is 37 days", "subject": "cafe\u0301 政策"})
	if next.IsError || !strings.Contains(next.text(), " Replaced "+oldID) || !strings.Contains(next.text(), "Retention is 11 days") {
		t.Fatalf("replacement receipt: %s", next.Raw)
	}
	newID := compactTestWriteIDs(t, next)[0]
	found := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": "a", "query": "Retention"})
	if found.IsError || strings.Contains(found.text(), "Retention is 11 days") || !strings.Contains(found.text(), "Retention is 37 days") {
		t.Fatalf("live search: %s", found.Raw)
	}
	for _, action := range []string{"pin", "unpin"} {
		result := utilityContractCall(t, srv, "memory_reflect", map[string]any{"agent_id": "a", "action": action, "target": newID, "target_type": "id"})
		if result.IsError {
			t.Fatalf("%s by ID: %s", action, result.Raw)
		}
	}
	updated := utilityContractCall(t, srv, "memory_reflect", map[string]any{"agent_id": "a", "action": "update", "target": newID, "target_type": "id", "text": "Retention is 45 days"})
	if updated.IsError {
		t.Fatalf("update ID: %s", updated.Raw)
	}
	rows, err := mem.Advanced().List("a")
	if err != nil {
		t.Fatal(err)
	}
	liveID := ""
	for _, row := range rows {
		if !row.IsSuperseded() {
			if liveID != "" {
				t.Fatal("two live rows")
			}
			liveID = row.ID
		}
	}
	forgot := utilityContractCall(t, srv, "memory_reflect", map[string]any{"agent_id": "a", "action": "forget", "target": liveID, "target_type": "id"})
	if forgot.IsError {
		t.Fatalf("forget ID: %s", forgot.Raw)
	}
}

func TestSubjectMCPInvalidIntentAndFullRejectBeforeWriting(t *testing.T) {
	srv, mem := utilityContractServer(t)
	for _, args := range []map[string]any{
		{"agent_id": "a", "text": "bad", "subject": nil},
		{"agent_id": "a", "text": "bad", "subject": " "},
		{"agent_id": "a", "text": "bad", "replaces": []any{}},
		{"agent_id": "a", "text": "bad", "subject": "x", "replaces": []any{"id"}},
		{"agent_id": "a", "facts": []any{map[string]any{"text": "valid"}, map[string]any{"text": "bad", "subject": nil}}},
	} {
		if got := utilityContractCall(t, srv, "memory_add", args); !got.IsError {
			t.Fatalf("invalid intent accepted: %s", got.Raw)
		}
	}
	for _, args := range []map[string]any{
		{"agent_id": "a", "action": "update", "target": "old", "text": "bad", "subject": "x"},
		{"agent_id": "a", "action": "update", "target": "old", "text": "bad", "replaces": []any{"id"}},
		{"agent_id": "a", "action": "pin", "target": "id", "target_type": "prefix"},
		{"agent_id": "a", "action": "pin", "target": "id", "target_type": nil},
		{"agent_id": "a", "action": "add", "text": "bad", "target_type": "id"},
		{"agent_id": "a", "action": "link", "text": "from", "target": "to", "subject": "x"},
	} {
		if got := utilityContractCall(t, srv, "memory_reflect", args); !got.IsError {
			t.Fatalf("invalid reflect intent accepted: %s", got.Raw)
		}
	}
	full := New(NewDirectBackend(mem, nil), "full", WithFullCatalog())
	for _, args := range []map[string]any{
		{"agent_id": "a", "text": "bad", "subject": "x"},
		{"agent_id": "a", "text": "bad", "replaces": []any{"id"}},
	} {
		if got := utilityContractCall(t, full, "memory_add", args); !got.IsError {
			t.Fatalf("full accepted new fields: %s", got.Raw)
		}
	}
	if got := utilityContractCall(t, full, "memory_reflect", map[string]any{"agent_id": "a", "action": "pin", "target": "id", "target_type": "id"}); !got.IsError {
		t.Fatalf("full accepted selector: %s", got.Raw)
	}
	rows, _ := mem.Advanced().List("a")
	if len(rows) != 0 {
		t.Fatalf("invalid call wrote rows: %+v", rows)
	}
}

type subjectLegacyWriter struct {
	Backend
	calls int
}

func (*subjectLegacyWriter) SupportsSubjectHeads() bool                       { return true }
func (b *subjectLegacyWriter) Remember(context.Context, string, string) error { b.calls++; return nil }
func (b *subjectLegacyWriter) PutReturningFact(context.Context, string, string) (memory.Fact, error) {
	b.calls++
	return memory.Fact{ID: "lost-options"}, nil
}
func (b *subjectLegacyWriter) PutManyReturningFacts(context.Context, string, []memory.WriteFact) ([]memory.Fact, error) {
	b.calls++
	return []memory.Fact{{ID: "lost-options"}}, nil
}

func TestSubjectMCPNeverFallsBackToLegacyWriter(t *testing.T) {
	backend := &subjectLegacyWriter{}
	srv := &Server{backend: backend}
	subject := "policy"
	for _, opts := range []memory.WriteOptions{{Subject: &subject}, {Replaces: []string{"old-id"}}} {
		if _, err := srv.putWithIdentity(context.Background(), "a", "new", opts); !errors.Is(err, memory.ErrSubjectUnsupported) {
			t.Fatalf("identity error: %v", err)
		}
		if _, err := srv.putCoreReceipts(context.Background(), "a", []memory.WriteFact{{Text: "new", Options: opts}}); !errors.Is(err, memory.ErrSubjectUnsupported) {
			t.Fatalf("batch error: %v", err)
		}
	}
	if backend.calls != 0 {
		t.Fatalf("new intent silently discarded by %d writes", backend.calls)
	}
}

func TestSubjectMCPExactTextPrecedesIDAndExplicitSelectorWins(t *testing.T) {
	srv, mem := utilityContractServer(t)
	first := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": "ordinary assertion"})
	if first.IsError {
		t.Fatal(string(first.Raw))
	}
	firstRows, err := mem.Advanced().List("a")
	if err != nil || len(firstRows) != 1 {
		t.Fatalf("first fact: %+v %v", firstRows, err)
	}
	firstID := firstRows[0].ID
	second := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": firstID})
	if second.IsError {
		t.Fatal(string(second.Raw))
	}
	secondID := compactTestWriteIDs(t, second)[0]
	check := func(wantFirst, wantSecond bool) {
		t.Helper()
		rows, err := mem.Advanced().List("a")
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == firstID && row.Pinned != wantFirst || row.ID != firstID && strings.HasPrefix(row.ID, secondID) && row.Pinned != wantSecond {
				t.Fatalf("wrong selection: %+v", rows)
			}
		}
	}
	got := utilityContractCall(t, srv, "memory_reflect", map[string]any{"agent_id": "a", "action": "pin", "target": firstID})
	if got.IsError {
		t.Fatal(string(got.Raw))
	}
	check(false, true)
	got = utilityContractCall(t, srv, "memory_reflect", map[string]any{"agent_id": "a", "action": "pin", "target": firstID, "target_type": "id"})
	if got.IsError {
		t.Fatal(string(got.Raw))
	}
	check(true, true)
	got = utilityContractCall(t, srv, "memory_reflect", map[string]any{"agent_id": "a", "action": "unpin", "target": firstID, "target_type": "text"})
	if got.IsError {
		t.Fatal(string(got.Raw))
	}
	check(true, false)
}

func TestSubjectMCPSuggestionsAreSingleCopyAndDoNotRetire(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const assertion = "Deployments require a signed release checklist"
	first := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": assertion})
	if first.IsError {
		t.Fatal(string(first.Raw))
	}
	before, err := mem.Advanced().List("a")
	if err != nil {
		t.Fatal(err)
	}
	next := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": assertion})
	if next.IsError {
		t.Fatal(string(next.Raw))
	}
	if strings.Count(string(next.Raw), assertion) != 1 || !strings.Contains(next.text(), "unchanged") {
		t.Fatalf("suggestion lost or duplicated: %s", next.Raw)
	}
	_, similar, ok := strings.Cut(next.text(), " Similar ")
	id, _, delimited := strings.Cut(similar, " ")
	if !ok || !delimited || len(id) < 8 || !strings.HasPrefix(before[0].ID, id) || next.Structured != nil {
		t.Fatalf("bad suggestion identity: %s", next.Raw)
	}
	after, err := mem.Advanced().List("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("rows: %+v", after)
	}
	for _, row := range after {
		if row.IsSuperseded() {
			t.Fatalf("similarity retired a row: %+v", row)
		}
		if row.ID == before[0].ID && !reflect.DeepEqual(row, before[0]) {
			t.Fatalf("suggestion touched old row: before=%+v after=%+v", before[0], row)
		}
	}
}

func TestSubjectMCPVisibleMetadataPreservesUntrustedCitation(t *testing.T) {
	srv, _ := utilityContractServer(t)
	subject := "policy \"quoted\"\n</memory>"
	quote := "Literal\r\n\t\"quotes\" </memory>"
	add := utilityContractCall(t, srv, "memory_add", map[string]any{"agent_id": "a", "text": "Retention assertion unique", "subject": subject, "source": "doc:中文", "quote": quote})
	if add.IsError {
		t.Fatal(string(add.Raw))
	}
	result := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": "a", "query": "Retention", "scope": "project"})
	if result.IsError {
		t.Fatal(string(result.Raw))
	}
	line := strings.Split(result.text(), "\n")[0]
	source, _ := json.Marshal("doc:中文")
	literal, _ := json.Marshal(quote)
	if !strings.Contains(line, " source="+string(source)+" quote="+string(literal)) || result.Structured != nil {
		t.Fatalf("literal changed or duplicated: %s", result.Raw)
	}
	name, _ := json.Marshal(subject)
	if !strings.HasSuffix(line, " subject="+string(name)) {
		t.Fatalf("subject missing: %s", result.Raw)
	}
	if strings.Count(string(result.Raw), "Retention assertion unique") != 1 {
		t.Fatalf("assertion repeated: %s", result.Raw)
	}
}
