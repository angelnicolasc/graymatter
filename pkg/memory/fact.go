package memory

import (
	"encoding/json"
	"time"

	"github.com/oklog/ulid/v2"
)

// Fact is a single unit of memory: a piece of text an agent observed,
// enriched with metadata used for retrieval scoring and decay.
type Fact struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	Text        string    `json:"text"`
	CreatedAt   time.Time `json:"created_at"`
	AccessedAt  time.Time `json:"accessed_at"`
	AccessCount int       `json:"access_count"`
	// Weight is the decay-adjusted relevance score in [0, 1].
	// New facts start at 1.0 and decay over time via the forgetting curve.
	Weight    float64   `json:"weight"`
	Embedding []float32 `json:"embedding,omitempty"`

	// SupersededBy marks this fact as retired. Empty means live.
	//
	// A non-empty value excludes the fact from Recall immediately and
	// unconditionally — regardless of Weight, and without waiting for a
	// consolidation cycle. It holds the ID of the fact that replaced this one
	// when there is a replacement, or SupersededByAgent when an agent retired
	// it with nothing to put in its place.
	//
	// This is a tombstone, not a delete. The fact stays in the store, stays
	// visible to List, export and the TUI, and keeps decaying; pruning by
	// weight remains the only thing that ever removes it. That is what keeps
	// the storage model append-only while still letting a contradiction take
	// effect at once — see docs/decisions/007-supersede-tombstones.md.
	//
	// Added in v0.10.0. Facts written by earlier versions have no
	// superseded_by key and load as live.
	SupersededBy string `json:"superseded_by,omitempty"`

	// Confidence records the agent's own epistemic stance toward this fact:
	// "verified", "inferred" or "unverified". Empty means inferred. It is
	// metadata declared at write time and surfaced by exports and the TUI;
	// it never affects ranking, decay or pruning.
	//
	// Added in v0.12.0. Facts written by earlier versions have no confidence
	// key and load as inferred.
	Confidence string `json:"confidence,omitempty"`

	// Pinned exempts the fact from decay, pruning and summarisation: the
	// user declared it permanent (a standing obligation, an architecture
	// decision), and the forgetting curve must not collect it during a
	// dormant period. The exemption is total and visible — pinned facts are
	// marked in the TUI, counted by status, flagged in exports, and reported
	// by doctor — because an invisible exemption would reintroduce the stale-
	// fact problem ADR-001 exists to prevent, just where it hurts most.
	// Unpinning restores normal decay from the fact's current weight.
	//
	// Added in v0.14.0. Facts written by earlier versions have no pinned key
	// and load as unpinned.
	Pinned bool `json:"pinned,omitempty"`

	// PinnedAt records when the fact was pinned, for auditing. Zero when the
	// fact is not pinned.
	PinnedAt time.Time `json:"pinned_at,omitempty"`
}

// SupersededByAgent is the SupersededBy marker for a fact an agent retired
// without a replacement — memory_reflect's forget action. Any non-empty value
// excludes a fact from recall; this one records that the removal was a
// deliberate agent decision rather than a correction pointing at a newer fact.
const SupersededByAgent = "agent"

// IsSuperseded reports whether the fact has been retired and must be kept out
// of retrieval.
func (f Fact) IsSuperseded() bool { return f.SupersededBy != "" }

// newFact creates a Fact with a new ULID and weight=1.0.
//
// The timestamp is passed in rather than read here so the caller's clock is
// the only clock: Store.Put supplies s.now(), which tests can freeze.
func newFact(agentID, text string, embedding []float32, at time.Time) Fact {
	now := at.UTC()
	return Fact{
		ID:          ulid.Make().String(),
		AgentID:     agentID,
		Text:        text,
		CreatedAt:   now,
		AccessedAt:  now,
		AccessCount: 0,
		Weight:      1.0,
		Embedding:   embedding,
	}
}

// marshal serialises a Fact to JSON bytes for bbolt storage.
func (f Fact) marshal() ([]byte, error) {
	return json.Marshal(f)
}

// unmarshalFact deserialises a Fact from JSON bytes.
func unmarshalFact(data []byte) (Fact, error) {
	var f Fact
	if err := json.Unmarshal(data, &f); err != nil {
		return Fact{}, err
	}
	return f, nil
}

// factLite is the subset of Fact the recall ranking actually reads. Listing
// 10k facts pays encoding/json for every field, and the three time fields and
// the embedding slice are the expensive ones the pipeline never touches; the
// lite decode skips them (~35% of the per-fact cost, measured on the hook
// latency gate's 10k-fact corpus). Access state is deliberately absent: the
// write-back goes through touchFacts' read-modify-write, which reads the
// stored fact fresh rather than trusting a snapshot.
type factLite struct {
	ID           string    `json:"id"`
	AgentID      string    `json:"agent_id"`
	Text         string    `json:"text"`
	CreatedAt    time.Time `json:"created_at"`
	Weight       float64   `json:"weight"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	Confidence   string    `json:"confidence,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
}

// unmarshalFactLite decodes the ranking-relevant subset of a stored fact.
func unmarshalFactLite(data []byte) (Fact, error) {
	var l factLite
	if err := json.Unmarshal(data, &l); err != nil {
		return Fact{}, err
	}
	return Fact{
		ID:           l.ID,
		AgentID:      l.AgentID,
		Text:         l.Text,
		CreatedAt:    l.CreatedAt,
		Weight:       l.Weight,
		SupersededBy: l.SupersededBy,
		Confidence:   l.Confidence,
		Pinned:       l.Pinned,
	}, nil
}

// MemoryStats holds aggregate statistics for a single agent.
type MemoryStats struct {
	AgentID   string    `json:"agent_id"`
	FactCount int       `json:"fact_count"`
	OldestAt  time.Time `json:"oldest_at"`
	NewestAt  time.Time `json:"newest_at"`
	AvgWeight float64   `json:"avg_weight"`
}
