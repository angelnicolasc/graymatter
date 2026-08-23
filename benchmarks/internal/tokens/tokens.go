// Package tokens holds the one token approximation the benchmarks share.
//
// It exists so that two benchmarks reporting "tokens" cannot mean two
// different things. docs/benchmarks.md publishes figures from
// benchmarks/token_count and benchmarks/retrieval_quality side by side; if
// each carried its own counter, a reader comparing them would be comparing
// nothing.
//
// A note on what this is not. pkg/memory has an unexported tokenize() used for
// keyword relevance, and it is tempting to reuse it here for "internal
// consistency". It would be wrong: tokenize() strips stop words, punctuation
// and single characters because that is what improves retrieval scoring, and
// as a measure of what a prompt costs it undercounts by 43 to 56 percent on
// realistic context files. Retrieval tokenization and cost estimation are
// different jobs that happen to share a word.
package tokens

import "strings"

// PerWord is the multiplier from whitespace-separated words to GPT-4-class
// tokens. Empirically within about 10 percent of tiktoken for English prose.
//
// It is an approximation and every published figure derived from it says so.
// A real BPE tokenizer would be more accurate and would cost a dependency and
// a model file, which is a trade this repository does not make: the benchmarks
// have to run offline, with no API key, from a clean clone.
const PerWord = 1.33

// Approx estimates the token cost of text.
func Approx(text string) int {
	return int(float64(len(strings.Fields(text))) * PerWord)
}

// ApproxAll estimates the token cost of a set of facts joined by newlines,
// which is how they reach a prompt.
func ApproxAll(texts []string) int {
	return Approx(strings.Join(texts, "\n"))
}
