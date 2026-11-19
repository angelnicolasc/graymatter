package main

import (
	"errors"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

type pendingCheckpointStore interface {
	CheckpointPeekPending(string) (*session.PendingCheckpoint, error)
}

func (d *directStore) CheckpointPeekPending(agentID string) (*session.PendingCheckpoint, error) {
	if d.store == nil {
		return nil, errors.New("memory store not initialised")
	}
	return session.PeekPending(d.store.DB(), agentID)
}

func (r *reconnectingStore) CheckpointPeekPending(agentID string) (*session.PendingCheckpoint, error) {
	var out *session.PendingCheckpoint
	err := r.do(func(s cliStore) error {
		reader, ok := s.(pendingCheckpointStore)
		if !ok {
			return errors.New("checkpoint pointer unsupported: upgrade and restart the GrayMatter daemon")
		}
		var err error
		out, err = reader.CheckpointPeekPending(agentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
