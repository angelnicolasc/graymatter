package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Alias facts: vocabulary an agent teaches the store
// when a search missed because the caller's wording and the store's wording
// differ. An alias fact's text is "alias: <term> = <equivalent>[, <equivalent>...]".
//
// Aliases are content-free by construction: the recall pipeline routes them
// to the query-expansion map instead of the ranking corpus, so they never
// appear in a result, never contribute document frequencies and never occupy
// a top-k slot. They inherit the ordinary fact machinery — revisable with
// memory_reflect, superseded aliases stop expanding, receipts record them.
// The weak-match feedback block names memory_alias as its suggested
// action, which is what turns the cold encounter into a warm store: the
// first miss fixes the vocabulary for every later query.

// aliasPrefix marks an alias fact's text.
const aliasPrefix = "alias:"

// PutAlias stores one alias fact mapping term to each equivalent, for
// agentID. Both directions of a single-token pair expand: after
// PutAlias("payments", ["acquiring"]), a query saying "payments" reaches
// facts about "acquiring" and one saying "acquiring" reaches facts about
// "payments". Multi-token equivalents expand forward only — reverse-mapping
// every token of "event bus" onto "broker" would fire on any query that
// merely mentions "event".
//
// The returned Fact is what landed, mirroring putReturningFact's contract.
func (s *Store) PutAlias(ctx context.Context, agentID, term string, equivalents []string) (Fact, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return Fact{}, fmt.Errorf("alias term is required")
	}
	cleaned := make([]string, 0, len(equivalents))
	for _, e := range equivalents {
		if e = strings.TrimSpace(e); e != "" {
			cleaned = append(cleaned, e)
		}
	}
	if len(cleaned) == 0 {
		return Fact{}, fmt.Errorf("alias needs at least one equivalent")
	}
	text := aliasPrefix + " " + term + " = " + strings.Join(cleaned, ", ")
	return s.putReturningFactKind(ctx, agentID, text, KindAlias, "")
}

type aliasExpansion struct {
	token  string
	factID string
}

// parseAliasFacts compiles live alias facts into the bidirectional expansion
// map the recall pipeline applies to every query. Single-token pairs map in
// both directions; multi-token equivalents map forward only (see PutAlias).
// Malformed texts are skipped silently — an alias the parser cannot read must
// not be able to break recall, and doctor is the place to surface them.
func parseAliasFacts(aliases []Fact) map[string][]aliasExpansion {
	if len(aliases) == 0 {
		return nil
	}
	// Both retrieval paths prefer the newest mapping. Break equal creation
	// times by ID so the same fact receives credit for a duplicate mapping.
	slices.SortFunc(aliases, func(a, b Fact) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	m := make(map[string][]aliasExpansion)
	add := func(k, v, id string) {
		if k == "" || v == "" || k == v {
			return
		}
		for _, existing := range m[k] {
			if existing.token == v {
				return
			}
		}
		m[k] = append(m[k], aliasExpansion{token: v, factID: id})
	}
	for _, f := range aliases {
		// Indexed fetch can observe a fact after its cached partition was read.
		if !f.IsAlias() || f.IsSuperseded() {
			continue
		}
		body, ok := strings.CutPrefix(f.Text, aliasPrefix)
		if !ok {
			continue
		}
		left, right, ok := strings.Cut(body, "=")
		if !ok {
			continue
		}
		leftToks := tokenize(strings.TrimSpace(left))
		if len(leftToks) == 0 {
			continue
		}
		for _, eq := range strings.Split(right, ",") {
			eqToks := tokenize(strings.TrimSpace(eq))
			if len(eqToks) == 0 {
				continue
			}
			singlePair := len(leftToks) == 1 && len(eqToks) == 1
			for _, lt := range leftToks {
				for _, et := range eqToks {
					add(lt, et, f.ID)
					if singlePair {
						add(et, lt, f.ID)
					}
				}
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// expandQuery appends alias-mapped vocabulary to the query's own terms. The
// appended tokens are surface forms: the scorer re-tokenizes the joined
// string with the same fold (including stemming) it applies to stored text,
// so an alias and the fact it bridges land on identical terms. Tokens already
// present in the query are not appended again — duplicates would inflate the
// TF-IDF term frequency of that term and skew the score.
func expandQuery(query string, aliases map[string][]aliasExpansion) (string, []string) {
	if len(aliases) == 0 {
		return query, nil
	}
	toks := tokenize(query)
	if len(toks) == 0 {
		return query, nil
	}
	seen := make(map[string]bool, len(toks)*2)
	for _, t := range toks {
		seen[t] = true
	}
	var extra, activated []string
	active := make(map[string]bool)
	for _, t := range toks {
		for _, alt := range aliases[t] {
			if seen[alt.token] {
				continue
			}
			seen[alt.token] = true
			extra = append(extra, alt.token)
			if !active[alt.factID] {
				active[alt.factID] = true
				activated = append(activated, alt.factID)
			}
		}
	}
	if len(extra) == 0 {
		return query, nil
	}
	return query + " " + strings.Join(extra, " "), activated
}
