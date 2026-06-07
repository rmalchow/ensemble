package group

import (
	"context"

	"ensemble/internal/id"
)

// MakeMaster orchestrates takeover so `node` becomes master of this node's
// current group (§5.2). MUST run on the current master (D17): returns
// ErrNotMaster otherwise so the API (I) can proxy to the master first. ctx
// bounds the HTTP fan-out.
func (e *Engine) MakeMaster(ctx context.Context, node id.ID) error {
	// Phase 1 (under lock): classify, validate, stop any running session, and
	// collect the members to retarget. HTTP calls happen after the lock drops.
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	if !mv.found || mv.role == roleFollower {
		e.mu.Unlock()
		return ErrNotMaster
	}
	// newMaster must be a current member of this group.
	isMember := false
	for _, m := range mv.group.Members {
		if m == node {
			isMember = true
			break
		}
	}
	if !isMember {
		e.mu.Unlock()
		return ErrTargetUnknown
	}

	// Stop any running playback session before mastership moves (§5.2 step 2).
	members := append([]id.ID(nil), mv.group.Members...)
	hadSession := e.sess != nil
	e.stopLocked()
	e.mu.Unlock()

	e.log.Info("takeover: orchestrating", "newMaster", node.String(),
		"group", mv.group.ID.String(), "members", len(members), "stoppedSession", hadSession)

	// Phase 2 (no lock): drive every member over HTTP (§5.2 step 3). Per-member
	// errors are logged, not fatal — members that miss the command self-heal.
	for _, m := range members {
		if m == node {
			continue // newMaster is unfollowed below
		}
		if m == e.self {
			// We are a member but not the new master: follow newMaster locally.
			e.p.Cluster.SetFollowing(node)
			e.log.Info("takeover: following new master", "target", node.String())
			continue
		}
		if err := e.p.Follow.Follow(ctx, m, node); err != nil {
			e.log.Warn("takeover: follow command failed", "member", m, "target", node, "err", err)
		} else {
			e.log.Info("takeover: directed member to follow", "member", m.String(), "target", node.String())
		}
	}

	// Tell the new master to become solo (§5.2 step 3).
	if node == e.self {
		e.p.Cluster.SetFollowing(id.Zero)
	} else {
		if err := e.p.Follow.Unfollow(ctx, node); err != nil {
			e.log.Warn("takeover: unfollow new master failed", "target", node, "err", err)
		}
	}
	e.log.Info("takeover: complete", "newMaster", node.String())
	return nil
}
