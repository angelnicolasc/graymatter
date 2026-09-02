package memory

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Weak-match feedback, implementing verbatim a specification frozen before
// any of it was measured. The trigger, the neighbourhood and the output
// shape below are that specification, not a reinterpretation: any change is
// a new decision to pre-register.

// weakMatchInput carries everything the feedback computation reads. It is all
// already computed by the recall pipeline. Costing no extra pass over the
// corpus is a requirement of the specification, not an optimisation.
type weakMatchInput struct {
	// originalQuery is what the caller wrote; the diagnostic line reports on it.
	originalQuery string
	// effectiveQuery is the alias-expanded query; the trigger reads it, so a
	// satisfied alias does not leave a weak signal firing.
	effectiveQuery string
	doStem         bool
	// n is the size of the live ranking corpus, the N of the idf below.
	n int
	// df is the corpus document frequency of one term, and tf the token →
	// frequency map of one fact. Both are functions rather than the maps they
	// used to be: the scan path answers them from arrays it already built,
	// while the candidate-set path answers df from the inverted index and
	// loads a fact's text only when asked. Same spec, same numbers, two
	// storage strategies — and exactly one implementation of the diagnostic,
	// which is what keeps the specified behaviour single-sourced.
	df func(term string) int
	tf func(factID string) map[string]int
	// seedDocs yields the token→frequency map of every fact containing at
	// least one seed term. It is the one-hop co-occurrence neighbourhood the
	// specification calls for, expressed as the only thing that computation
	// needs: the scan
	// path walks the whole corpus, the indexed path walks the seeds' posting
	// lists, and neither has to know which it is.
	seedDocs func(seeds map[string]bool) []map[string]int
	// ranked is the fused ranking, best first, post MinRelevance cut.
	ranked []scored
	topK   int
}

// FeedbackAction is the name the weak-match block tells the caller to use.
//
// It is a constant rather than a literal inside the message because the
// uninstructed-agent arm measured what an unresolvable suggestion costs: the
// block said "memory_alias", the CLI had no command by that name, and the
// agent answered 40 of 40 questions across 98 calls without writing a single
// alias — against 6 calls and 6 aliases once the affordance was named where
// the caller stood. The store taught nothing because the thing it named did
// not exist there.
//
// Both halves of that pairing now read this one symbol: the block formats it,
// and the CLI registers it as an alias of its own command. Drift is not
// something a test has to catch, because it is no longer expressible. The
// value is the one the specification names, and changing it moves both
// halves together.
const FeedbackAction = "memory_alias"

// feedbackMaxTerms caps the vocabulary line; the specified ceiling is ten.
const feedbackMaxTerms = 10

// feedbackMinCandidates is the fallback threshold: below four candidates
// the neighbourhood is too sparse to trust, and the returned facts' own
// vocabulary takes over — exactly the case where coverage is 0 and the
// primary neighbourhood would come back empty.
const feedbackMinCandidates = 4

// questionWords is the interrogative scaffolding of a question, excluded from
// the coverage calculation ONLY. The ranking tokenizer is untouched: a
// diagnostic must not move the thing it diagnoses.
//
// Specification revision 2, disposed of in advance by the original: "if (3)
// fails the trigger is mis-calibrated and gets re-specified before anything
// is re-measured". Measured cause: the store's stopword list carries no
// interrogatives, so "how", "which" and "often" counted as content terms the
// store was missing, and every well-formed question opened with two or three
// terms it could never cover. Calibration over 120 held-out queries: 56% firing
// at 52% precision with 97% recall of failures — a trigger that caught nearly
// every gap and cried wolf half the time.
//
// Only words a declarative fact would essentially never contain are listed:
// excluding a term the store does hold would lower coverage and fire MORE, so
// the list is deliberately short rather than thorough.
var questionWords = map[string]bool{
	"how": true, "what": true, "which": true, "who": true, "whom": true,
	"whose": true, "when": true, "where": true, "why": true,
	"does": true, "do": true, "did": true, "can": true, "could": true,
	"should": true, "would": true, "will": true, "shall": true,
	"must": true, "may": true, "might": true,
	// The quantity modifiers that only show up in "how <x>" questions.
	"long": true, "much": true, "many": true, "often": true, "soon": true,
	"our": true, "your": true, "their": true,
}

// weakMatchFeedback returns the feedback block, or "" when the match is
// strong enough that the trigger does not fire. The block is additive
// text: nothing here touches the ranking, and the test suite pins that the
// facts and their order are identical with the feedback on or off.
func weakMatchFeedback(in weakMatchInput) string {
	effToks := dedupTokens(tokenizeStem(in.effectiveQuery, in.doStem))
	if len(effToks) == 0 || len(in.ranked) == 0 {
		return ""
	}
	n := in.n

	effSet := make(map[string]bool, len(effToks))
	for _, t := range effToks {
		effSet[t] = true
	}

	// Spec §2. coverage: how many of the query's content terms exist in the
	// store at all — the direct observable of "my words are not the store's
	// words". Revision 2: the interrogative scaffolding is not content, so it
	// is out of both numerator and denominator. Falls back to the full token
	// set when a query is nothing but question words, which would otherwise
	// divide by zero.
	contentToks := make([]string, 0, len(effToks))
	for _, t := range effToks {
		if !questionWords[t] {
			contentToks = append(contentToks, t)
		}
	}
	if len(contentToks) == 0 {
		contentToks = effToks
	}
	coverage := 0
	for _, t := range contentToks {
		if in.df(t) > 0 {
			coverage++
		}
	}
	covRatio := float64(coverage) / float64(len(contentToks))

	// Spec §2. maxOverlap: the strongest returned fact's term overlap with
	// the query. Covers the case coverage cannot see — the terms exist but
	// are scattered, and the best fact shares a single word with a
	// five-word query.
	maxOverlap := 0
	k := in.topK
	if k > len(in.ranked) {
		k = len(in.ranked)
	}
	for _, sc := range in.ranked[:k] {
		ov := 0
		for t := range in.tf(sc.id) {
			if effSet[t] {
				ov++
			}
		}
		if ov > maxOverlap {
			maxOverlap = ov
		}
	}

	// Spec §2, frozen thresholds, declared as an uncalibrated choice.
	if covRatio >= 0.5 && maxOverlap > 1 {
		return ""
	}

	// Spec §3. Neighbourhood: one-hop co-occurrence with the query's seeds,
	// scored by document count × idf, excluding query terms and terms so
	// common they cannot inform a reformulation.
	seedSet := make(map[string]bool)
	for _, t := range effToks {
		if in.df(t) > 0 {
			seedSet[t] = true
		}
	}
	idf := func(t string) float64 {
		return math.Log((float64(n) + 1) / (float64(in.df(t)) + 1))
	}

	type candidate struct {
		term  string
		score float64
	}
	var cands []candidate
	if len(seedSet) > 0 {
		docCount := make(map[string]int)
		for _, tf := range in.seedDocs(seedSet) {
			for t := range tf {
				if seedSet[t] || effSet[t] {
					continue
				}
				if float64(in.df(t)) > 0.25*float64(n) {
					continue
				}
				docCount[t]++
			}
		}
		for t, c := range docCount {
			cands = append(cands, candidate{t, float64(c) * idf(t)})
		}
	}

	byScore := func(a, b candidate) bool {
		if a.score != b.score {
			return a.score > b.score
		}
		return a.term < b.term
	}

	var terms []string
	if len(cands) >= feedbackMinCandidates {
		sort.Slice(cands, func(i, j int) bool { return byScore(cands[i], cands[j]) })
		if len(cands) > feedbackMaxTerms {
			cands = cands[:feedbackMaxTerms]
		}
		for _, c := range cands {
			terms = append(terms, c.term)
		}
	} else {
		// Spec §3.6 fallback: the returned facts' own vocabulary, top idf.
		// When coverage is 0 there are no seeds, and without this the block
		// would come back empty exactly in the worst case.
		seen := make(map[string]bool)
		var fb []candidate
		for _, sc := range in.ranked[:k] {
			for t := range in.tf(sc.id) {
				if effSet[t] || seen[t] {
					continue
				}
				seen[t] = true
				fb = append(fb, candidate{t, idf(t)})
			}
		}
		sort.Slice(fb, func(i, j int) bool { return byScore(fb[i], fb[j]) })
		if len(fb) > feedbackMaxTerms {
			fb = fb[:feedbackMaxTerms]
		}
		for _, c := range fb {
			terms = append(terms, c.term)
		}
	}
	if len(terms) == 0 {
		return ""
	}

	// Spec §4. Diagnostic, vocabulary, action — three lines, ~25 tokens.
	return fmt.Sprintf("(weak match: %d of %d terms in your query exist in this store)\n"+
		"nearby vocabulary: %s\n"+
		"If one of those is what you meant, re-query with that word. If your term is a "+
		"synonym this store does not know, record it with "+FeedbackAction+" and the next query finds it.",
		coverage, len(effToks), strings.Join(terms, ", "))
}

// dedupTokens preserves first-seen order and drops repeats.
func dedupTokens(toks []string) []string {
	seen := make(map[string]bool, len(toks))
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
