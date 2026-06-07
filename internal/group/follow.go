package group

import (
	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// follow validates target and, if valid, records SetFollowing(target) (§5.1).
// Re-pointing (already following someone) is allowed and overwrites.
func (e *Engine) follow(target id.ID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	snap := e.p.Cluster.Snapshot()
	if err := validateFollowTarget(snap, e.self, target); err != nil {
		return err
	}
	e.p.Cluster.SetFollowing(target)
	return nil
}

// validateFollowTarget enforces §5.1: target must not be self, must be a known,
// alive node, and must itself be a master (Following == Zero).
func validateFollowTarget(snap contracts.Snapshot, self, target id.ID) error {
	if target == self {
		return ErrSelfFollow
	}
	var tn *contracts.NodeView
	for i := range snap.Nodes {
		if snap.Nodes[i].ID == target {
			tn = &snap.Nodes[i]
			break
		}
	}
	if tn == nil {
		return ErrTargetUnknown
	}
	if !tn.Alive {
		return ErrTargetDead
	}
	if !tn.Following.IsZero() {
		return ErrTargetFollower
	}
	return nil
}
