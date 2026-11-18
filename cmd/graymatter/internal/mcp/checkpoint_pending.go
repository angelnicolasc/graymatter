package mcp

import (
	"errors"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

func (b *DirectBackend) CheckpointPeekPending(agentID string) (*session.PendingCheckpoint, error) {
	if b.mem == nil || b.mem.Advanced() == nil {
		return nil, errors.New("memory store not initialised")
	}
	return session.PeekPending(b.mem.Advanced().DB(), agentID)
}
