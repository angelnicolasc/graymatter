package memory

import "errors"

var (
	ErrSubjectUnsupported     = errors.New("subject memory operations unsupported: update and restart the store daemon")
	ErrSubjectAmbiguous       = errors.New("subject association is ambiguous; select the concrete fact ID")
	ErrSubjectHeadUnavailable = errors.New("subject head cannot be verified from retained exact revisions")
)

// SubjectProjection is derived from the separate subject index. It is never
// part of the canonical fact row or of the PR-D v1 content hash.
type SubjectProjection struct {
	Name      string `json:"name"`
	Revisions uint64 `json:"revisions"`
}

// SubjectHeadInspection exports the stored anchor, including closed heads and
// anchors whose canonical rows were pruned by an older writer. Fact.Subjects
// separately projects the resolved current version from the same transaction.
type SubjectHeadInspection struct {
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
	Reference LineageReference `json:"reference"`
	Revisions uint64           `json:"revisions"`
	State     string           `json:"state"`
}

// ReplacedFact is captured from the validated canonical rows in the same
// transaction that commits their replacement. Presentation may show a prefix
// of Text, escaped as data; citation literals remain independent and complete.
type ReplacedFact struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type FactSuggestion struct {
	ID    string  `json:"id"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}

type SuggestionResult struct {
	Suggestions []FactSuggestion `json:"suggestions"`
	Total       int              `json:"total"`
}

// WriteReceipt is a confirmed write identity plus optional intention receipts.
// A transport must not redispatch this operation after an uncertain response.
type WriteReceipt struct {
	Fact             Fact                `json:"fact"`
	Replaced         []ReplacedFact      `json:"replaced,omitempty"`
	Subjects         []SubjectProjection `json:"subjects,omitempty"`
	Suggestions      []FactSuggestion    `json:"suggestions,omitempty"`
	SuggestionsTotal int                 `json:"suggestions_total,omitempty"`
}

func (*Store) SupportsSubjectHeads() bool { return true }
