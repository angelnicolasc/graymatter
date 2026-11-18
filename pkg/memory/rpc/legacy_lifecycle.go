package rpc

import (
	"fmt"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// legacyLifecycle preserves pin/unpin/forget on a daemon whose negotiated
// capabilities do not include narrow lifecycle patches. It is selected only
// before sending a mutation, never after a new endpoint has returned an error.
//
// Old daemons only provide full-snapshot UpdateFact. Refreshing the snapshot
// avoids copying stale caller metadata, but does not make the old read/modify/
// write protocol atomic: another writer can still change it after List. The
// stronger metadata-preservation guarantee requires the new daemon endpoint.
// The direct compatibility call is restricted to these fresh metadata patches.
// General UpdateFact and revisions require a daemon that understands lineage.
func (c *Client) legacyLifecycle(agentID string, pinned *bool, victims ...memory.Fact) error {
	if len(victims) == 0 {
		return nil
	}
	facts, err := c.List(agentID)
	if err != nil {
		return err
	}
	byID := make(map[string]memory.Fact, len(facts))
	for _, fact := range facts {
		byID[fact.ID] = fact
	}
	updates := make([]memory.Fact, 0, len(victims))
	seen := make(map[string]bool, len(victims))
	at := time.Now().UTC()
	for _, victim := range victims {
		current, found := byID[victim.ID]
		requiresLive := pinned == nil || *pinned
		if victim.AgentID != agentID || !found || current.AgentID != agentID ||
			current.Text != victim.Text || current.Kind != victim.Kind ||
			(requiresLive && (victim.IsSuperseded() || current.IsSuperseded())) {
			return fmt.Errorf("%w: cannot change legacy fact %s", memory.ErrFactChanged, victim.ID)
		}
		if seen[victim.ID] {
			continue
		}
		seen[victim.ID] = true
		if pinned == nil {
			current.SupersededBy = memory.SupersededByAgent
		} else {
			current.Pinned, current.PinnedAt = *pinned, time.Time{}
			if *pinned {
				current.PinnedAt = at
			}
		}
		updates = append(updates, current)
	}
	for _, current := range updates {
		req := &UpdateFactRequest{AgentID: agentID, Fact: current}
		if err := c.call("UpdateFact", req, &UpdateFactResponse{}); err != nil {
			// Commit status may be unknown. Do not replay this update or send
			// the remaining mutations after losing the response.
			return fmt.Errorf("legacy lifecycle update %s failed: %w", current.ID, err)
		}
	}
	return nil
}
