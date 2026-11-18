package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRP42BatchCitationDefaults(t *testing.T) {
	srv, mem := utilityContractServer(t)
	const ns = "r-p42"
	const defaultSource = "ops/manual"
	const defaultQuote = "Texto exacto 中文\r\ncon salto."
	got := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": ns, "source": defaultSource, "quote": defaultQuote,
		"facts": []map[string]any{
			{"text": "Atlas has both defaults."},
			{"text": "Birch has its own source.", "source": "ops/override"},
			{"text": "Cobalt has its own quote.", "source": "ops/cobalt", "quote": "Texto diferente."},
			{"text": "Delta explicitly has empty citation fields.", "source": "", "quote": ""},
		},
	})
	if got.IsError {
		t.Fatalf("inheriting source and quote failed: %s", got.Raw)
	}
	rows, err := mem.Advanced().List(ns)
	if err != nil || len(rows) != 4 {
		t.Fatalf("batch rows: %+v / %v", rows, err)
	}
	want := map[string][2]string{
		"Atlas has both defaults.":                    {defaultSource, defaultQuote},
		"Birch has its own source.":                   {"ops/override", defaultQuote},
		"Cobalt has its own quote.":                   {"ops/cobalt", "Texto diferente."},
		"Delta explicitly has empty citation fields.": {"", ""},
	}
	for _, row := range rows {
		expected := want[row.Text]
		fetch := utilityContractCall(t, srv, "memory_search", map[string]any{
			"agent_id": ns, "id": row.ID,
		})
		source, _ := json.Marshal(expected[0])
		quote, _ := json.Marshal(expected[1])
		if fetch.IsError || !strings.Contains(fetch.text(), "source="+string(source)+" quote="+string(quote)) {
			t.Errorf("citation on %q: %s", row.Text, fetch.Raw)
		}
	}
}

func TestRP42BatchSingletonSubject(t *testing.T) {
	for _, tt := range []struct {
		name string
		row  map[string]any
		want string
	}{
		{"inherit", map[string]any{"text": "Retention is 45 days."}, "maintenance"},
		{"override", map[string]any{"text": "Retention is 45 days.", "subject": "archive"}, "archive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := utilityContractServer(t)
			got := utilityContractCall(t, srv, "memory_add", map[string]any{
				"agent_id": "a", "subject": "maintenance", "facts": []map[string]any{tt.row},
			})
			if got.IsError {
				t.Fatalf("singleton subject rejected: %s", got.Raw)
			}
			search := utilityContractCall(t, srv, "memory_search", map[string]any{"agent_id": "a", "query": "retention"})
			if search.IsError || !strings.Contains(search.text(), `subject="`+tt.want+`"`) {
				t.Fatalf("wrong subject: %s", search.Raw)
			}
		})
	}
}

func TestRP42BatchFieldErrorsAreAtomic(t *testing.T) {
	for _, tt := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"multi_subject", map[string]any{
			"subject": "maintenance", "facts": []map[string]any{{"text": "one"}, {"text": "two"}},
		}, "put subject inside each facts object"},
		{"duplicate_normalized_subject", map[string]any{
			"facts": []map[string]any{
				{"text": "one", "subject": " CAFÉ\tpolicy "},
				{"text": "two", "subject": "cafe\u0301 policy"},
			},
		}, "batch writes the same normalized subject more than once"},
		{"invalid_source_default", map[string]any{
			"source": false, "facts": []map[string]any{{"text": "one", "source": "per-row"}, {"text": "two"}},
		}, "facts[1]: source must be a string"},
		{"invalid_quote_override", map[string]any{
			"quote": "default", "facts": []map[string]any{{"text": "one"}, {"text": "two", "quote": nil}},
		}, "facts[1]: quote must be a string"},
		{"invalid_subject_singleton", map[string]any{
			"subject": false, "facts": []map[string]any{{"text": "one"}},
		}, "facts[0]: subject must be a string"},
		{"confidence_stays_per_row", map[string]any{
			"confidence": "verified", "facts": []map[string]any{{"text": "one"}},
		}, "put confidence inside each facts object"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, mem := utilityContractServer(t)
			tt.args["agent_id"] = "a"
			got := utilityContractCall(t, srv, "memory_add", tt.args)
			if !got.IsError || got.text() != tt.want {
				t.Errorf("want error %q, got %s", tt.want, got.Raw)
			}
			rows, err := mem.Advanced().List("a")
			if err != nil || len(rows) != 0 {
				t.Fatalf("invalid batch partially wrote: %+v / %v", rows, err)
			}
		})
	}
}

func TestRP42FullRequiresTextForOrdinaryAdd(t *testing.T) {
	srv, mem := utilityContractServer(t, WithFullCatalog())
	got := utilityContractCall(t, srv, "memory_add", map[string]any{
		"agent_id": "a", "source": "private source", "facts": []map[string]any{{"text": "one"}},
	})
	if !got.IsError || got.text() != "text is required" {
		t.Fatalf("expected an ordinary add validation error: %s", got.Raw)
	}
	facts, err := mem.Advanced().List("a")
	if err != nil || len(facts) != 0 {
		t.Fatalf("invalid full add wrote facts: %+v / %v", facts, err)
	}
}
