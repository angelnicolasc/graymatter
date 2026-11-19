package memory

import (
	"math"
	"time"
)

// decayFact retains the per-row reference used by the lifecycle and
// differential tests. Maintenance uses the bounded transactional path.
func (s *Store) decayFact(agentID, id string, now time.Time, lambda float64) (Fact, error) {
	return s.patchFact(agentID, id, func(current *Fact) error {
		if current.agingExempt() {
			return errFactUnchanged
		}
		current.Weight = math.Min(current.Weight, weightAtAge(current.AccessedAt, now, lambda))
		return nil
	})
}
