package daemon

import (
	"errors"
	"fmt"
	netrpc "net/rpc"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

type CheckpointPeekPendingRequest struct{ AgentID string }
type CheckpointPeekPendingResponse struct{ Pending *session.PendingCheckpoint }

// CheckpointPeekPending performs no resume, audit, fact access or token write.
func (h *Host) CheckpointPeekPending(req *CheckpointPeekPendingRequest, resp *CheckpointPeekPendingResponse) error {
	pending, err := session.PeekPending(h.db, req.AgentID)
	if err != nil {
		return err
	}
	resp.Pending = pending
	return nil
}

// CheckpointPeekPending deliberately has no fallback to CheckpointResume on an
// older daemon: callers must not silently change the operation's read contract.
func (c *Client) CheckpointPeekPending(agentID string) (*session.PendingCheckpoint, error) {
	var resp CheckpointPeekPendingResponse
	if err := c.hostCall("CheckpointPeekPending", &CheckpointPeekPendingRequest{AgentID: agentID}, &resp); err != nil {
		var remote netrpc.ServerError
		if errors.As(err, &remote) && strings.Contains(string(remote), "can't find") {
			return nil, fmt.Errorf("checkpoint pointer unsupported: upgrade and restart the GrayMatter daemon: %w", err)
		}
		return nil, err
	}
	return resp.Pending, nil
}
