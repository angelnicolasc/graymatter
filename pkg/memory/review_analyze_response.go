package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// ErrInvalidReviewResponse reports a malformed, truncated, or over-budget response.
var ErrInvalidReviewResponse = errors.New("invalid review response")

func invalidReview(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidReviewResponse, reason)
}

// reviewObject accepts exactly one object, rejects duplicate keys, and can
// restrict keys for model-owned data. Provider envelopes may carry metadata
// that is independent of the shared model response contract.
func reviewObject(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, invalidReview("response is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, invalidReview("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, invalidReview("invalid JSON object key")
		}
		if len(allowed) != 0 && !slices.Contains(allowed, key) {
			return nil, invalidReview("unknown response field")
		}
		if _, exists := fields[key]; exists {
			return nil, invalidReview("duplicate response field")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, invalidReview("malformed JSON field")
		}
		fields[key] = value
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return nil, invalidReview("unterminated JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalidReview("trailing data after JSON object")
	}
	return fields, nil
}

func reviewField[T any](fields map[string]json.RawMessage, key string) (T, error) {
	var value T
	raw, exists := fields[key]
	if !exists {
		return value, nil
	}
	if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return value, invalidReview("invalid " + key + " field")
	}
	return value, nil
}

func reviewResponseText(provider string, raw []byte) (string, error) {
	fields, err := reviewObject(raw)
	if err != nil {
		return "", err
	}
	if provider == "ollama" {
		done, err := reviewField[bool](fields, "done")
		if err != nil {
			return "", err
		}
		reason, err := reviewField[string](fields, "done_reason")
		if err != nil {
			return "", err
		}
		if !done || (reason != "" && reason != "stop") {
			return "", invalidReview("Ollama response did not complete normally")
		}
		return reviewField[string](fields, "response")
	}
	reason, err := reviewField[string](fields, "stop_reason")
	if err != nil {
		return "", err
	}
	if reason != "end_turn" {
		return "", invalidReview("Anthropic response did not complete normally")
	}
	blocks, err := reviewField[[]json.RawMessage](fields, "content")
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, block := range blocks {
		blockFields, err := reviewObject(block)
		if err != nil {
			return "", err
		}
		kind, err := reviewField[string](blockFields, "type")
		if err != nil {
			return "", err
		}
		if kind != "text" {
			return "", invalidReview("Anthropic response contains a non-text block")
		}
		part, err := reviewField[string](blockFields, "text")
		if err != nil {
			return "", err
		}
		text.WriteString(part)
	}
	return text.String(), nil
}

func parseReviewFindings(raw string, batch []EvidenceSnapshot) ([]ReviewFinding, error) {
	fields, err := reviewObject([]byte(raw), "summary", "consumes", "conflicts", "contradictions")
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, invalidReview("response does not declare any findings")
	}
	summary, err := reviewField[string](fields, "summary")
	if err != nil {
		return nil, err
	}
	refs, err := reviewField[[]string](fields, "consumes")
	if err != nil {
		return nil, err
	}
	conflicts, err := reviewField[[]json.RawMessage](fields, "conflicts")
	if err != nil {
		return nil, err
	}
	legacy, err := reviewField[[]string](fields, "contradictions")
	if err != nil {
		return nil, err
	}
	hasSummary := strings.TrimSpace(summary) != "" || len(refs) > 0
	groups := len(conflicts) + len(legacy)
	if hasSummary {
		groups++
	}
	if groups > ReviewMaxGroups {
		return nil, invalidReview("too many review findings")
	}
	ids := make(map[string]bool, len(batch))
	for _, source := range batch {
		ids[source.FactID] = true
	}
	var findings []ReviewFinding
	if hasSummary {
		if strings.TrimSpace(summary) == "" {
			return nil, invalidReview("summary references require summary text")
		}
		if err := validateReviewReferences(refs, ids, 1); err != nil {
			return nil, err
		}
		findings = append(findings, ReviewFinding{Kind: ReviewKindSummary, Summary: summary, SourceIDs: refs})
	}
	for _, conflict := range conflicts {
		group, err := reviewObject(conflict, "ids", "note")
		if err != nil {
			return nil, err
		}
		refs, err := reviewField[[]string](group, "ids")
		if err != nil {
			return nil, err
		}
		if err := validateReviewReferences(refs, ids, 2); err != nil {
			return nil, err
		}
		note, err := reviewField[string](group, "note")
		if err != nil {
			return nil, err
		}
		if err := validateReviewNote(note); err != nil {
			return nil, err
		}
		findings = append(findings, ReviewFinding{Kind: ReviewKindConflict, Note: note, SourceIDs: refs})
	}
	for _, note := range legacy {
		if err := validateReviewNote(note); err != nil {
			return nil, err
		}
		findings = append(findings, ReviewFinding{Kind: ReviewKindUntyped, Note: note})
	}
	return findings, nil
}

func validateReviewReferences(refs []string, batch map[string]bool, minimum int) error {
	if len(refs) < minimum || len(refs) > len(batch) {
		return invalidReview("invalid source reference count")
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !batch[ref] || seen[ref] {
			return invalidReview("source reference is outside the sent batch or repeated")
		}
		seen[ref] = true
	}
	return nil
}

func validateReviewNote(note string) error {
	if len(note) > ReviewMaxNoteBytes || strings.TrimSpace(note) == "" {
		return invalidReview("conflict note is empty or exceeds its byte limit")
	}
	return nil
}
