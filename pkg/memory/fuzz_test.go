package memory

import (
	"encoding/json"
	"testing"
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
			// Must be lowercase ASCII/digit only (by construction).
			for _, r := range tok {
				if !('a' <= r && r <= 'z') && !('0' <= r && r <= '9') {
					t.Errorf("tokenize produced non-lowercase token %q (rune %q)", tok, r)
				}
			}
		}
	})
}

// FuzzUnmarshalFact ensures unmarshalFact never panics on arbitrary bytes.
// Run the fuzzer: go test -fuzz=FuzzUnmarshalFact ./pkg/memory/
func FuzzUnmarshalFact(f *testing.F) {
	// Seed corpus: valid JSON, truncated JSON, empty, binary garbage.
	validFact, _ := json.Marshal(newFact("agent", "some text", nil))
	seeds := [][]byte{
		validFact,
		[]byte(`{}`),
		[]byte(`{"id":"x","agent_id":"a","text":"t","weight":1.0}`),
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
		// Must not panic; errors are acceptable and expected for bad input.
		_, _ = unmarshalFact(data)
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
