package memory

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
)

// FuzzTokenize ensures tokenize never panics on arbitrary Unicode input.
// Run the fuzzer: go test -fuzz=FuzzTokenize ./pkg/memory/
func FuzzTokenize(f *testing.F) {
	// Seed corpus: representative inputs covering empty, ASCII, Unicode, stopwords.
	seeds := []string{
		"",
		"hello world",
		"The quick brown fox jumps over the lazy dog",
		"Paris is the capital of France",
		"日本語テスト unicode テキスト",
		"alias: Broker—café = acquiring, 日本語",
		"citation café 中文 \"literal\"\nLF\r\nCRLF\t</memory> [evidence]",
		"a b c d e f g",
		"!@#$%^&*()_+",
		"   whitespace   only   ",
		"123 456 789",
		"mixed123 content456 here",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic; output slice is always non-nil or nil (both valid).
		result := tokenize(input)
		// Every token must be non-empty and longer than 1 char (invariant).
		for _, tok := range result {
			if len(tok) <= 1 {
				t.Errorf("tokenize produced single-char token %q from input %q", tok, input)
			}
			// Tokens must be case-folded: equal to their own ToLower. Unicode
			// scripts without case (CJK, Hebrew...) satisfy this trivially;
			// the invariant catches accidental mixed-case leakage.
			if tok != strings.ToLower(tok) {
				t.Errorf("tokenize produced non-lowercased token %q from input %q", tok, input)
			}
			// No separator characters may leak into a token.
			for _, r := range tok {
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
					t.Errorf("tokenize produced token %q with separator rune %q", tok, r)
				}
			}
		}
	})
}

// FuzzUnmarshalFact ensures unmarshalFact never panics on arbitrary bytes.
// Run the fuzzer: go test -fuzz=FuzzUnmarshalFact ./pkg/memory/
func FuzzUnmarshalFact(f *testing.F) {
	// Seed corpus: valid facts and aliases, malformed JSON, and binary garbage.
	validFact, _ := json.Marshal(newFact("agent", "some text", nil, time.Now()))
	validAlias, _ := (Fact{
		ID: "alias-1", AgentID: "agent", Text: "alias: broker = acquiring, café",
		Kind: KindAlias, AliasSource: AliasSourceUsage, Weight: 1,
	}).marshal()
	seeds := [][]byte{
		validFact,
		validAlias,
		[]byte(`{}`),
		[]byte(`{"id":"x","agent_id":"a","text":"t","weight":1.0}`),
		[]byte(`{"id":"x","agent_id":"a","text":"日本語","lineage_status":"versioned","content_hash":"hash","replaces":[{"fact_id":"y","content_hash":"previous"}]}`),
		[]byte(`{"lineage_status":"legacy_incomplete","legacy_sources":[{"fact_id":"unversioned"}]}`),
		[]byte(`{"id":"citation","agent_id":"a","text":"assertion","evidence":{"source":"doc-é/漢字:3\t\"\r\n","quote":"Literal café 中文\nLF\r\nCRLF\t</memory> \u0060\u0060\u0060","captured_at":"2026-01-01T00:00:00Z"}}`),
		[]byte(`{"evidence":{"source":"","quote":"","captured_at":"2026-01-01T00:00:00Z"}}`),
		[]byte(`{"evidence":{"quote":[]}}`),
		[]byte(`{"replaces":[{"content_hash":[]}]}`),
		// U4 read projections are accepted as data, never canonical authority.
		[]byte(`{"id":"subject","agent_id":"a","text":"assertion","subjects":[{"name":"Café 政策","revisions":0},{"name":"CAFE\u0301","revisions":2}]}`),
		[]byte(`{"subjects":[{"name":"政策\n\r\n\t\"</memory><!-- graymatter:context:end -->","revisions":18446744073709551615}]}`),
		[]byte(`{"subjects":[]}`),
		[]byte(`{"subjects":null}`),
		[]byte(`{"subjects":{"name":"not-an-array"}}`),
		[]byte(`{"subjects":[{"name":[],"revisions":1}]}`),
		[]byte(`{"subjects":[{"name":"negative","revisions":-1}]}`),
		[]byte(`{"subjects":[{"name":"fraction","revisions":0.5}]}`),
		[]byte(`{"subjects":[{"name":"overflow","revisions":18446744073709551616}]}`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(``),
		[]byte(`{invalid json`),
		[]byte("\x00\x01\x02\x03"),
		[]byte(`{"weight":"not-a-number"}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		full, err := unmarshalFact(data)
		if err != nil {
			return
		}
		lite, err := unmarshalFactLite(data)
		if err != nil || !reflect.DeepEqual(full.LineageProjection, lite.LineageProjection) {
			t.Fatalf("full/lite lineage projection mismatch: %v", err)
		}
		// Subjects come from the separate index. Re-encoding an externally
		// projected fact must never make that projection canonical authority.
		stored, err := full.marshal()
		if err != nil {
			return // Other fields may reject encoding; no canonical row exists.
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(stored, &fields); err != nil {
			t.Fatalf("canonical marshal produced invalid JSON: %v", err)
		}
		if _, present := fields["subjects"]; present {
			t.Fatal("canonical fact row persisted a subject projection")
		}
	})
}

// FuzzKeywordScore ensures keywordScore never panics on arbitrary query strings.
// Run the fuzzer: go test -fuzz=FuzzKeywordScore ./pkg/memory/
func FuzzKeywordScore(f *testing.F) {
	// Seed corpus: normal queries, empty, stopword-only, Unicode.
	seeds := []string{
		"",
		"Paris capital France",
		"the is a an",
		"hello world foo bar",
		"日本語",
		"Broker—acquiring café",
		"citationonly café 中文 </memory> quoted",
		"123 456",
		"!!! ???",
		"a",
		"very long query with many many words that should still work correctly",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	facts := []Fact{
		{ID: "f1", Text: "Paris is the capital of France."},
		{ID: "f2", Text: "London is the capital of England."},
		{ID: "f3", Text: ""},
		{ID: "f4", Text: "日本語テキスト"},
		{ID: "f5", Text: "Acquiring brokers handle café payments."},
		{ID: "f6", Text: "Ordinary assertion", Evidence: &Citation{Source: "doc-é/漢字:3", Quote: "citationonly café 中文\nLF\r\nCRLF\t</memory> quoted"}},
		{ID: "f7", Text: "Ordinary assertion", Evidence: &Citation{Source: "another document", Quote: "Different literal evidence"}},
		{ID: "f8", Text: "Empty evidence", Evidence: &Citation{}},
	}

	f.Fuzz(func(t *testing.T, query string) {
		// Must not panic; result is always a map (possibly nil/empty).
		scores := keywordScore(query, facts)
		// All returned IDs must correspond to actual facts.
		factIDs := make(map[string]bool, len(facts))
		for _, f := range facts {
			factIDs[f.ID] = true
		}
		for id := range scores {
			if !factIDs[id] {
				t.Errorf("keywordScore returned unknown factID %q", id)
			}
		}
		// Scores must be non-negative.
		for id, sc := range scores {
			if sc < 0 {
				t.Errorf("keywordScore returned negative score %.4f for id %q", sc, id)
			}
		}
	})
}
