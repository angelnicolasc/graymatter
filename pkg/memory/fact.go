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

// MemoryStats holds aggregate statistics for a single agent.
type MemoryStats struct {
	AgentID   string    `json:"agent_id"`
	FactCount int       `json:"fact_count"`
	OldestAt  time.Time `json:"oldest_at"`
	NewestAt  time.Time `json:"newest_at"`
	AvgWeight float64   `json:"avg_weight"`
}
