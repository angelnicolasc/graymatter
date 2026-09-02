package memory

import (
	"math"
	"time"
)

// RecallReceipt is the per-fact provenance record behind one recalled fact:
// what came back, why it ranked where it did, and where it came from.
//
// It is produced by RecallExplain and carries exactly the signals Recall fused
// internally since v0.10.0 — the ranks the three retrieval signals assigned
// (1-based, 0 = the signal did not rank this fact), the RRF constant k that
// damped them, and the fact's own metadata (stored weight, age, written-at
// instant, tombstone pointer). It is surface, not architecture: nothing is
// recomputed for explain, the values are the ones the ranking read.
//
// Added in v0.17.0.
type RecallReceipt struct {
	// Text is the fact text exactly as stored.
	Text string `json:"text"`
	// Weight is the fact's stored decay-adjusted weight in [0, 1].
	Weight float64 `json:"weight"`
	// AgeDays is the fact's age in days at recall time, rounded to two
	// decimals so the JSON is stable across runs on the same store.
	AgeDays float64 `json:"age_days"`
	// Ranks is the per-signal ranking breakdown that produced the fused score.
	Ranks RecallRanks `json:"ranks"`
	// Provenance is the fact's identity and lifecycle record.
	Provenance RecallProvenance `json:"provenance"`
	// KGLinks lists the entity IDs the knowledge-graph extractor finds in the
	// fact text, when an extractor is wired. Omitted otherwise.
	KGLinks []string `json:"kg_links,omitempty"`
}

// RecallRanks breaks one fact's fused RRF score into the per-signal ranks that
// produced it. A rank of 0 means the signal did not rank this fact at all
// (vector: no embedder or no embedding; keyword: no term overlap; recency
// always ranks every live fact). K is the RRF damping constant in force.
type RecallRanks struct {
	VectorRank  int     `json:"vector_rank"`
	KeywordRank int     `json:"keyword_rank"`
	RecencyRank int     `json:"recency_rank"`
	FusedScore  float64 `json:"fused_score"`
	K           float64 `json:"k"`
}

// RecallProvenance is the fact's identity and lifecycle record: the receipt ID
// a caller can cite back, the instant the fact was written, and its tombstone
// state. Recalled facts are always live, so SupersededBy is empty on every
// receipt; the field exists so the schema is stable and self-describing.
type RecallProvenance struct {
	FactID       string    `json:"fact_id"`
	WrittenAt    time.Time `json:"written_at"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	Confidence   string    `json:"confidence,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
	// Supersedes names every fact this one replaced, walked transitively, so a
	// value corrected twice reports both retired versions. Empty for a fact
	// that revised nothing.
	//
	// This is the other half of the tombstone. Recall drops superseded facts
	// before scoring, which is what makes the answer current — but it also
	// makes the correction invisible: the caller receives one value with no
	// sign that it replaced anything. Naming the retired IDs restores the
	// justification without putting a retired belief back in the ranking, and
	// the IDs resolve against the store, which keeps every tombstone.
	Supersedes []string `json:"supersedes,omitempty"`
}

// newReceipt builds one receipt from the pipeline's ranking state. Extracted
// so RecallExplain stays readable and the field mapping has exactly one home.
func (s *Store) newReceipt(f *Fact, p *recallPipeline, fused float64) RecallReceipt {
	r := RecallReceipt{
		Text:    f.Text,
		Weight:  f.Weight,
		AgeDays: math.Round(p.nowT.Sub(f.CreatedAt).Hours()/24*100) / 100,
		Ranks: RecallRanks{
			VectorRank:  p.vectorRank[f.ID],
			KeywordRank: p.kwRank[f.ID],
			RecencyRank: p.recRank[f.ID],
			FusedScore:  fused,
			K:           p.k,
		},
		Provenance: RecallProvenance{
			FactID:       f.ID,
			WrittenAt:    f.CreatedAt,
			SupersededBy: f.SupersededBy,
			Confidence:   f.Confidence,
			Pinned:       f.Pinned,
			Supersedes:   p.lineage(f.ID),
		},
	}
	s.mu.RLock()
	extractor := s.extractor
	s.mu.RUnlock()
	if extractor != nil {
		if ids, err := extractor.ExtractIDs(f.Text); err == nil && len(ids) > 0 {
			r.KGLinks = ids
		}
	}
	return r
}
