package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const MaxFactsImportBytes = 8 << 20

// ImportedFact maps an exported identity to the new committed fact. Import is
// intentionally not restore: only assertion, confidence and citation survive.
type ImportedFact struct {
	SourceNamespace string `json:"source_namespace"`
	SourceID        string `json:"source_id"`
	ID              string `json:"id"`
}

// ParseFactsJSON accepts the memories.json fact array, not the lineage audit
// archive. Quote limits belong to the destination StoreConfig. The bounded
// document is validated completely before any mutation or embedder callback.
func ParseFactsJSON(reader io.Reader) ([]WriteFact, []CitationReference, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, MaxFactsImportBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxFactsImportBytes {
		return nil, nil, fmt.Errorf("facts JSON exceeds limit of %d bytes", MaxFactsImportBytes)
	}
	if !utf8.Valid(raw) {
		return nil, nil, errors.New("facts JSON must be valid UTF-8")
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, nil, errors.New("import requires a memories.json array; lineage audit archives require full backup restore")
	}
	var rows []Fact
	if err = json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, fmt.Errorf("decode facts JSON: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, errors.New("facts JSON must contain at least one fact")
	}
	inputs := make([]WriteFact, len(rows))
	refs := make([]CitationReference, len(rows))
	seen := make(map[string]bool)
	for i, f := range rows {
		if f.Kind != "" || f.IsSuperseded() {
			return nil, nil, fmt.Errorf("facts[%d]: import accepts live content facts only; aliases and historical rows require backup restore", i)
		}
		if f.ID == "" || f.AgentID == "" {
			return nil, nil, fmt.Errorf("facts[%d]: exported namespace and id are required", i)
		}
		key := f.AgentID + "\x00" + f.ID
		if seen[key] {
			return nil, nil, fmt.Errorf("facts[%d]: duplicate exported identity", i)
		}
		seen[key] = true
		opts := WriteOptions{}
		if f.Confidence != "" {
			value := f.Confidence
			opts.Confidence = &value
		}
		if f.Evidence != nil {
			source, quote := f.Evidence.Source, f.Evidence.Quote
			opts.Source = &source
			opts.Quote = &quote
		}
		inputs[i] = WriteFact{Text: f.Text, Options: opts}
		refs[i] = CitationReference{Namespace: f.AgentID, FactID: f.ID, ContentHash: f.ContentHash}
	}
	if err = ValidateWriteFacts("import", inputs); err != nil {
		return nil, nil, err
	}
	return inputs, refs, nil
}

// ImportFactsJSON creates new facts in the explicit namespace in one commit.
// It does not import historical identity, access/decay/pin state or lineage.
func (s *Store) ImportFactsJSON(ctx context.Context, namespace string, reader io.Reader) ([]ImportedFact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inputs, refs, err := ParseFactsJSON(reader)
	if err != nil {
		return nil, err
	}
	facts, err := s.PutManyReturningFacts(ctx, namespace, inputs)
	if err != nil {
		return nil, err
	}
	out := make([]ImportedFact, len(facts))
	for i, f := range facts {
		out[i] = ImportedFact{SourceNamespace: refs[i].Namespace, SourceID: refs[i].FactID, ID: f.ID}
	}
	return out, nil
}
