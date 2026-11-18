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
		[]byte(`{"replaces":[{"content_hash":[]}]}`),
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
