package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/text/unicode/norm"
)

const suggestionLimit = 3
const suggestionThreshold = .8

func suggestionWords(value string) map[string]bool {
	words := strings.FieldsFunc(norm.NFC.String(strings.ToLower(value)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

func suggestionText(value string) string {
	return norm.NFC.String(strings.Join(strings.Fields(strings.ToLower(value)), " "))
}

// similarFactSuggestions is pure: it neither ranks through Recall nor learns
// aliases, records accesses, changes retention or invokes an embedding provider.
func similarFactSuggestions(namespace, text string, facts []Fact) SuggestionResult {
	out := SuggestionResult{Suggestions: []FactSuggestion{}}
	words := suggestionWords(text)
	if len(words) == 0 {
		return out
	}
	for _, fact := range facts {
		if fact.AgentID != namespace || !fact.Injectable() {
			continue
		}
		other := suggestionWords(fact.Text)
		score := 0.0
		if suggestionText(text) == suggestionText(fact.Text) {
			score = 1
		} else if len(words) >= 3 && len(other) >= 3 {
			intersection := 0
			for word := range words {
				if other[word] {
					intersection++
				}
			}
			score = float64(intersection) / float64(len(words)+len(other)-intersection)
		}
		if score >= suggestionThreshold {
			out.Suggestions = append(out.Suggestions, FactSuggestion{ID: fact.ID, Text: fact.Text, Score: score})
		}
	}
	sort.Slice(out.Suggestions, func(i, j int) bool {
		if out.Suggestions[i].Score != out.Suggestions[j].Score {
			return out.Suggestions[i].Score > out.Suggestions[j].Score
		}
		return out.Suggestions[i].ID < out.Suggestions[j].ID
	})
	out.Total = len(out.Suggestions)
	if len(out.Suggestions) > suggestionLimit {
		out.Suggestions = out.Suggestions[:suggestionLimit]
	}
	return out
}

func canonicalFactsTx(tx *bolt.Tx, namespace string) ([]Fact, error) {
	var facts []Fact
	b := lineageRowBucket(tx, namespace)
	if b == nil {
		return facts, nil
	}
	err := b.ForEach(func(id, raw []byte) error {
		f, err := unmarshalFactLite(raw)
		if err != nil {
			return err
		}
		if f.ID != string(id) || f.AgentID != namespace {
			return ErrFactChanged
		}
		facts = append(facts, f)
		return nil
	})
	return facts, err
}

func (s *Store) SuggestSimilarFacts(ctx context.Context, namespace, text string) (SuggestionResult, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(text) == "" {
		return SuggestionResult{}, fmt.Errorf("namespace and text are required")
	}
	if err := ctx.Err(); err != nil {
		return SuggestionResult{}, err
	}
	var out SuggestionResult
	err := s.db.View(func(tx *bolt.Tx) error {
		facts, err := canonicalFactsTx(tx, namespace)
		if err != nil {
			return err
		}
		out = similarFactSuggestions(namespace, text, facts)
		return ctx.Err()
	})
	return out, err
}
