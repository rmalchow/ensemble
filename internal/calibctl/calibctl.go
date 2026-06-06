// Package calibctl is the cmd-side fan-out controller for the A.10b calibration
// signal (08 §F2.1). It resolves a calibration selector (a group id, or an
// explicit node-id list) against the replicated ConfigDoc, picks ONE common
// future group startSample so every selected node emits the click at the same
// group instant, runs the local player when this node is a target, and proxies
// every remote target in parallel over mTLS — assembling the per-node
// playedOn/warnings result the §F2.1 body needs.
//
// Layering (doc 01 §2): calibctl MAY import internal/state (read the ConfigDoc
// for target + peer-address resolution). It deliberately does NOT import
// internal/web, internal/audio/* or internal/group: it reaches the local player,
// the timeline and the mTLS peer client through the LocalPlayer / Proxy /
// NowSampleFunc function-value seams that cmd wires (mirroring the A.14.3 Deps
// discipline). The handler passes scalars (groupID / nodeIDs), so calibctl never
// needs web.CalibrateSel (doc P6.2 §6 recommendation R3).
package calibctl

import (
	"context"
	"errors"
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

var (
	// ErrNoTarget is returned when the selection resolves to zero target nodes.
	ErrNoTarget = errors.New("calibrate: no target nodes")
	// ErrUnknownGroup is returned when groupID is not a current GroupRecord.
	ErrUnknownGroup = errors.New("calibrate: unknown group")
	// ErrUnknownNode is returned when a requested node id is not a current NodeRecord.
	ErrUnknownNode = errors.New("calibrate: unknown node")
	// ErrNotSynced is returned when a groupId selection has no synced timeline, so
	// no common future start instant can be computed (cross-node alignment needs it).
	ErrNotSynced = errors.New("calibrate: group not synced")
)

// LeadFrames is the common-start lead in canonical frames: A.12 LeadMs 300 ms at
// the canonical rate 48000 = 14400 frames. The controller picks
// startSample = now + LeadFrames so every node can prime before the instant
// arrives. Quoted from A.12; not invented here.
const (
	leadMs      = 300
	canonicalHz = 48000
	LeadFrames  = int64(leadMs) * canonicalHz / 1000 // 14400
)

// LocalPlayer plays the calibration signal on THIS node at the agreed group
// startSample for durationSec. cmd binds it to the local CalibratePlayer.Play.
type LocalPlayer func(ctx context.Context, startSample int64, durationSec int) error

// Proxy forwards a single-node calibrate request to peer addr (host:port, mTLS
// control plane) carrying the already-resolved startSample + durationSec, so the
// peer aligns to the controller's instant rather than recomputing its own (R6).
// cmd binds it to the pki mTLS client.
type Proxy func(ctx context.Context, addr, nodeID string, startSample int64, durationSec int) error

// NowSampleFunc returns the current group sample for groupID (from that group's
// Timeline) so the controller can compute a common future start; ok=false when
// the group is not synced.
type NowSampleFunc func(groupID string) (sample int64, ok bool)

// Result is the richer per-request outcome the §F2.1 body needs.
type Result struct {
	PlayedOn []string // node ids that successfully started
	Warnings []string // human-readable per-node failures (Render=false, unreachable)
}

// Controller fans a calibration request out across the selected nodes. It is
// safe for concurrent use (it holds no per-request mutable state).
type Controller struct {
	state      func() state.ConfigDoc
	selfID     string
	local      LocalPlayer
	proxy      Proxy
	now        NowSampleFunc
	leadFrames int64
}

// NewController binds the seams. stateFn returns the current ConfigDoc (for target
// + peer-address resolution); selfID is this node's id; local runs the local
// player; proxy reaches a peer over mTLS; now reads a group's current sample.
func NewController(stateFn func() state.ConfigDoc, selfID string, local LocalPlayer, proxy Proxy, now NowSampleFunc) *Controller {
	return &Controller{
		state:      stateFn,
		selfID:     selfID,
		local:      local,
		proxy:      proxy,
		now:        now,
		leadFrames: LeadFrames,
	}
}

// PlayDetailed resolves the selection, picks ONE common future startSample, runs
// the local player (if self is a target) and proxies every remote target in
// parallel, aggregating per-node failures into Warnings. It returns a non-nil
// error ONLY for whole-request failures (bad selection, no target, unknown id,
// group unsynced); per-node failures (Render=false, unreachable) are reported via
// Warnings with a 200-class outcome.
//
// Exactly one of groupID / nodeIDs must be non-empty (the handler enforces this,
// but PlayDetailed re-checks defensively).
func (c *Controller) PlayDetailed(ctx context.Context, groupID string, nodeIDs []string, durationSec int) (Result, error) {
	doc := c.state()

	targets, alignGroup, err := c.resolve(doc, groupID, nodeIDs)
	if err != nil {
		return Result{}, err
	}
	if len(targets) == 0 {
		return Result{}, ErrNoTarget
	}

	// One common future start instant for every target. A groupId selection MUST be
	// synced (cross-node coincidence needs a shared timeline). A nodeIds selection
	// best-effort-aligns to a common group if all members share one; otherwise it
	// starts each node from its own now (startSample 0 => the local player begins
	// immediately), with an informational warning (R5).
	var startSample int64
	var aligned bool
	if alignGroup != "" {
		now, ok := c.now(alignGroup)
		if !ok {
			return Result{}, ErrNotSynced
		}
		startSample = now + c.leadFrames
		aligned = true
	}

	res := c.fanOut(ctx, doc, targets, startSample, durationSec)
	if !aligned && len(targets) > 1 {
		res.Warnings = append(res.Warnings,
			"nodes not in a common group: played per-node from each node's own now (no cross-node alignment)")
	}
	return res, nil
}

// resolve turns the selector into the target node-id set plus the group to align
// to (alignGroup=="" => no shared timeline, best-effort per-node start).
func (c *Controller) resolve(doc state.ConfigDoc, groupID string, nodeIDs []string) (targets []string, alignGroup string, err error) {
	if (groupID == "") == (len(nodeIDs) == 0) {
		return nil, "", errors.New("calibrate: provide exactly one of groupID or nodeIDs")
	}
	if groupID != "" {
		g := findGroup(doc, groupID)
		if g == nil {
			return nil, "", ErrUnknownGroup
		}
		return append([]string(nil), g.MemberNodeIDs...), groupID, nil
	}
	// nodeIds: each must be a current node. Align to a common group iff every
	// target shares exactly one.
	for _, id := range nodeIDs {
		if findNode(doc, id) == nil {
			return nil, "", ErrUnknownNode
		}
	}
	return append([]string(nil), nodeIDs...), commonGroup(doc, nodeIDs), nil
}

// fanOut runs the local player (self) and proxies the rest in parallel.
func (c *Controller) fanOut(ctx context.Context, doc state.ConfigDoc, targets []string, startSample int64, durationSec int) Result {
	type outcome struct {
		id      string
		warning string
		played  bool
	}
	results := make([]outcome, len(targets))
	var wg sync.WaitGroup

	for i, id := range targets {
		i, id := i, id
		node := findNode(doc, id)
		// Render=false node cannot play; not fatal (§F2.1).
		if node != nil && !node.Caps.Render {
			results[i] = outcome{id: id, warning: id + ": node cannot play (Render=false)"}
			continue
		}
		if id == c.selfID {
			results[i] = outcome{id: id, played: true}
			if c.local != nil {
				if err := c.local(ctx, startSample, durationSec); err != nil {
					results[i] = outcome{id: id, warning: id + ": " + err.Error()}
				}
			}
			continue
		}
		addr := firstAddr(node)
		if addr == "" || c.proxy == nil {
			results[i] = outcome{id: id, warning: id + ": unreachable (no address)"}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.proxy(ctx, addr, id, startSample, durationSec); err != nil {
				results[i] = outcome{id: id, warning: id + ": unreachable (" + err.Error() + ")"}
				return
			}
			results[i] = outcome{id: id, played: true}
		}()
	}
	wg.Wait()

	var res Result
	for _, o := range results {
		if o.played {
			res.PlayedOn = append(res.PlayedOn, o.id)
		}
		if o.warning != "" {
			res.Warnings = append(res.Warnings, o.warning)
		}
	}
	return res
}

// findGroup / findNode / commonGroup / firstAddr are small ConfigDoc lookups.

func findGroup(doc state.ConfigDoc, id string) *state.GroupRecord {
	for i := range doc.Groups {
		if doc.Groups[i].ID == id {
			return &doc.Groups[i]
		}
	}
	return nil
}

func findNode(doc state.ConfigDoc, id string) *state.NodeRecord {
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == id {
			return &doc.Nodes[i]
		}
	}
	return nil
}

// commonGroup returns the single group every node id belongs to, or "" if they
// span zero or multiple groups (no shared timeline to align against, R5).
func commonGroup(doc state.ConfigDoc, nodeIDs []string) string {
	candidate := ""
	for _, g := range doc.Groups {
		if containsAll(g.MemberNodeIDs, nodeIDs) {
			if candidate != "" {
				return "" // ambiguous: more than one group contains all targets
			}
			candidate = g.ID
		}
	}
	return candidate
}

func containsAll(members, want []string) bool {
	for _, w := range want {
		found := false
		for _, m := range members {
			if m == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func firstAddr(node *state.NodeRecord) string {
	if node == nil || len(node.Addrs) == 0 {
		return ""
	}
	return node.Addrs[0]
}
