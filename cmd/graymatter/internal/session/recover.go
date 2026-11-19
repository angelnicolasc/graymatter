package session

import bolt "go.etcd.io/bbolt"

// Resume loads the latest resumable checkpoint in the agentID namespace.
// Callers may use its state and any saved messages to reconstruct context.
//
// Typical usage:
//
//	cp, err := session.Resume(store.DB(), "my-agent")
//	if err == nil {
//	    messages = append(cp.Messages, newUserMessage)
//	    state    = cp.State
//	}
func Resume(db *bolt.DB, agentID string) (*Checkpoint, error) {
	return Latest(db, agentID)
}
