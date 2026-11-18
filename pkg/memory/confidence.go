package memory

import (
	"errors"
	"fmt"
	"math"
)

// ConfidencePolicy identifies the bounded preference formula and its receipts.
const ConfidencePolicy = "confidence-v1"

// DefaultConfidenceWeight remains zero until a separately announced release
// promotes a candidate that passes the frozen calibration and validation gates.
const DefaultConfidenceWeight = 0.0

// ErrConfidenceUnsupported means a backend cannot preserve the requested
// confidence contract. It is a semantic error, not a failed connection.
var ErrConfidenceUnsupported = errors.New("confidence options unsupported: update and restart the store daemon")

// WriteOptions distinguishes omission (legacy storage) from an explicit label.
// It is comparable, including when it carries an explicit list of replacements.
type WriteOptions struct {
	Confidence *string        `json:"confidence,omitempty"`
	Source     *string        `json:"source,omitempty"`
	Quote      *string        `json:"quote,omitempty"`
	Subject    *string        `json:"subject,omitempty"`
	Replaces   ReplacementIDs `json:"-"`
}

func (o WriteOptions) Validate() error {
	if err := o.validateSubjectOptions(); err != nil {
		return err
	}
	var source, quote string
	if o.Source != nil {
		source = *o.Source
	}
	if o.Quote != nil {
		quote = *o.Quote
	}
	if err := validateCitationStrings(source, quote); err != nil {
		return err
	}
	if o.Confidence != nil {
		return ValidateConfidence(*o.Confidence)
	}
	return nil
}

// RecallOptions applies independently to each call, without mutating a store's
// configuration. An explicit zero weight preserves a requested filter.
type RecallOptions struct {
	MinConfidence    *string  `json:"min_confidence,omitempty"`
	ConfidenceWeight *float64 `json:"confidence_weight,omitempty"`
}

func (o RecallOptions) Validate() error {
	if o.MinConfidence != nil {
		if err := ValidateConfidence(*o.MinConfidence); err != nil {
			return err
		}
	}
	if o.ConfidenceWeight != nil {
		w := *o.ConfidenceWeight
		if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 || w > 0.5 {
			return fmt.Errorf("confidence_weight must be finite and between 0 and 0.5")
		}
	}
	return nil
}

func (o RecallOptions) Requested() bool {
	return o.MinConfidence != nil || o.ConfidenceWeight != nil
}

// Weight resolves the product default exactly once at the request boundary.
func (o RecallOptions) Weight() float64 {
	if o.ConfidenceWeight != nil {
		return *o.ConfidenceWeight
	}
	return DefaultConfidenceWeight
}

func ValidateConfidence(value string) error {
	switch value {
	case "verified", "inferred", "unverified":
		return nil
	default:
		return fmt.Errorf("confidence must be verified, inferred or unverified")
	}
}

// EffectiveConfidence preserves legacy neutrality and treats unknown historical
// labels conservatively. The original stored label remains in provenance.
func EffectiveConfidence(raw string) string {
	switch raw {
	case "verified":
		return "verified"
	case "", "inferred":
		return "inferred"
	default:
		return "unverified"
	}
}

func ConfidenceLevel(raw string) int {
	switch EffectiveConfidence(raw) {
	case "verified":
		return 2
	case "inferred":
		return 1
	default:
		return 0
	}
}

// RetrievalMetadata describes the effective policy even for empty results.
// Calls with no options omit it when the vector index is available.
type RetrievalMetadata struct {
	MinConfidence    *string `json:"min_confidence,omitempty"`
	ConfidenceWeight float64 `json:"confidence_weight"`
	Policy           string  `json:"policy"`
	KG               string  `json:"kg"`
	// VectorFallback gives the reason a recall skipped external vector ranking.
	VectorFallback string `json:"vector_fallback,omitempty"`
}

func (m *RetrievalMetadata) Text() string {
	if m == nil {
		return ""
	}
	filter := "none"
	if m.MinConfidence != nil {
		filter = *m.MinConfidence
	}
	text := fmt.Sprintf("Retrieval policy %s: min_confidence=%s, confidence_weight=%g; KG=%s.", m.Policy, filter, m.ConfidenceWeight, m.KG)
	if m.VectorFallback != "" {
		text += fmt.Sprintf(" Vector search degraded (%s).", m.VectorFallback)
	}
	return text
}

// ConfidenceRanking retains the base RRF score separately from the score that
// governs selection. Confidence is a writer's declaration, not a probability.
type ConfidenceRanking struct {
	BaseScore           float64 `json:"base_score"`
	FinalScore          float64 `json:"final_score"`
	Factor              float64 `json:"factor"`
	EffectiveConfidence string  `json:"effective_confidence"`
	ConfidenceWeight    float64 `json:"confidence_weight"`
	Policy              string  `json:"policy"`
}

type RecallResult struct {
	Facts         []string           `json:"facts"`
	Feedback      string             `json:"feedback,omitempty"`
	Retrieval     *RetrievalMetadata `json:"retrieval,omitempty"`
	ReviewNotices []ReviewNotice     `json:"conflict_notices,omitempty"`
}

type RecallExplainResult struct {
	Receipts  []RecallReceipt    `json:"receipts"`
	Retrieval *RetrievalMetadata `json:"retrieval,omitempty"`
}
