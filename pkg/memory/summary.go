package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// FactSummary contains whole-namespace and live-only aggregates. Like existing
// Stats and StoreOverview, live counts include aliases; tombstones do not.
type FactSummary struct {
	All        MemoryStats
	Live       MemoryStats
	Tombstones int
	Recalls    int
}

// FactSummarizer is optional so existing AdvancedStore implementations remain
// compatible. Implementations need not retain rows or decode their embeddings.
type FactSummarizer interface {
	SummarizeFacts(context.Context, string) (FactSummary, error)
}

func newFactSummary(agent string) FactSummary {
	return FactSummary{All: MemoryStats{AgentID: agent}, Live: MemoryStats{AgentID: agent}}
}
func addSummaryStat(stats *MemoryStats, weight float64, created time.Time) {
	if stats.FactCount == 0 || created.Before(stats.OldestAt) {
		stats.OldestAt = created
	}
	if stats.FactCount == 0 || created.After(stats.NewestAt) {
		stats.NewestAt = created
	}
	stats.FactCount++
	stats.AvgWeight += weight // divide once after the scan
}
func (s *FactSummary) add(weight float64, created time.Time, retired bool, accesses int) {
	addSummaryStat(&s.All, weight, created)
	if retired {
		s.Tombstones++
		return
	}
	addSummaryStat(&s.Live, weight, created)
	s.Recalls += accesses
}
func (s *FactSummary) finish() {
	if s.All.FactCount > 0 {
		s.All.AvgWeight /= float64(s.All.FactCount)
	}
	if s.Live.FactCount > 0 {
		s.Live.AvgWeight /= float64(s.Live.FactCount)
	}
}

// SummarizeFacts computes all aggregates in one read transaction with constant
// retained memory. Text and embedding payloads are skipped, and no candidate
// index is consulted, built or repaired.
func (s *Store) SummarizeFacts(ctx context.Context, agent string) (FactSummary, error) {
	out := newFactSummary(agent)
	count := 0
	out.All.LiveEvidenceCount, out.Live.LiveEvidenceCount = &count, &count
	if err := ctx.Err(); err != nil {
		return out, err
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket(bucketFacts)
		if parent == nil {
			return nil
		}
		bucket := parent.Bucket([]byte(agent))
		if bucket == nil {
			return nil
		}
		citations, _ := evidenceNamespace(tx, bucketCitations, agent, false)
		return bucket.ForEach(func(key, raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw == nil {
				return nil
			}
			var row struct {
				CreatedAt    time.Time `json:"created_at"`
				Weight       float64   `json:"weight"`
				SupersededBy string    `json:"superseded_by"`
				AccessCount  int       `json:"access_count"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				return fmt.Errorf("cannot decode fact %q in raw facts for namespace %q: %w", key, agent, err)
			}
			out.add(row.Weight, row.CreatedAt, row.SupersededBy != "", row.AccessCount)
			if citations != nil && row.SupersededBy == "" {
				f, err := unmarshalFactLite(raw)
				if err != nil {
					return err
				}
				if f.ID != string(key) || f.AgentID != agent {
					return fmt.Errorf("citation fact identity mismatch")
				}
				if err := hydrateCitationTx(tx, &f); err != nil {
					return err
				}
				if f.Evidence != nil {
					count++
				}
			}
			return nil
		})
	})
	if err != nil {
		return FactSummary{}, err
	}
	out.finish()
	return out, nil
}

// ReadFactSummary uses streaming aggregation when available and preserves the
// previous List-based behavior for older optional store implementations.
func ReadFactSummary(ctx context.Context, store interface{ List(string) ([]Fact, error) }, agent string) (FactSummary, error) {
	if summary, ok := store.(FactSummarizer); ok {
		return summary.SummarizeFacts(ctx, agent)
	}
	out := newFactSummary(agent)
	if err := ctx.Err(); err != nil {
		return out, err
	}
	facts, err := store.List(agent)
	if err != nil {
		return out, err
	}
	for _, f := range facts {
		if err := ctx.Err(); err != nil {
			return FactSummary{}, err
		}
		out.add(f.Weight, f.CreatedAt, f.IsSuperseded(), f.AccessCount)
	}
	out.finish()
	return out, nil
}
