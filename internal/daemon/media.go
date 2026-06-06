package daemon

// media.go is the daemon-side implementation of the 08 §F media/transport ops and
// the §G.2 group-status read that the web.Deps closures (deps.go) wrap. It is the
// bridge layer (doc 01 §2 rule 6): it may import web + state + the realtime stack;
// web reaches all of it only through the Deps function values.
//
// Each control op follows the 08 §F proxy semantics: a LOCAL ConfigDoc write
// (state.Store.Apply under If-Match) → gossip (the store signals Changed; the
// gossip plane replicates it), then a FAN-OUT to the group master over mTLS so
// audio begins/halts without waiting for gossip convergence (08 §F.3/§F.4). The
// master's role loop, seeing GroupRecord.Playing/Media change, runs applyRole
// (role.go) which (re)starts the origin. play/stop only flip the replicated
// Playing bool; the master's Timeline resumes from sampleIndex (04 §4.4.4).
//
// These methods are nil-safe before a live session: with no state.Store they
// return an ErrNotReady-class error, so a partially-wired daemon (P0.3 skeleton)
// still serves the surface and degrades to 503/404 in the handler.

import (
	"context"
	"errors"
	"strings"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// transport holds the live engine handles the media ops drive. It is set on the
// Node by activate() once the realtime planes stand up; until then it is nil and
// the ops degrade gracefully. Kept as a small struct (rather than fields strewn
// across Node) so the wiring step and the unit tests can populate exactly the
// seams an op needs.
type transport struct {
	store   *state.Store // replicated ConfigDoc store (P2.1)
	self    string       // this node's id
	dataDir string       // node data/ folder for the local media list (P0.1)

	// master resolves the elected master id for a group (cluster.GroupElections,
	// P2.3). "" => unknown / no master yet.
	master func(groupID string) string

	// live reports gossip liveness for a node id (the G.2 Online flag when no
	// peer telemetry proxy is wired — without it every non-self member renders
	// as offline on the dashboard while the cluster screen says online). nil =>
	// liveness unknown (offline).
	live func(nodeID string) bool

	// peer is the cross-node seam: media-existence proxy + start/stop fan-out +
	// per-member status fan-out, all over mTLS. nil => no peers wired (solo); the
	// ops then treat the local node as authoritative.
	peer peerProxy

	// listLocal lists this node's playable media (flat); injected so tests need
	// no disk — it also short-circuits the media existence check. nil => read
	// the dataDir via the media source helper (which browses subdirectories).
	listLocal func() ([]web.MediaFile, error)

	// statusOf returns this node's live per-group telemetry (render counters /
	// clock health) for the G.2 self projection. ok=false before sync. nil =>
	// zeroed live fields.
	statusOf func(groupID string) (liveStatus, bool)

	// playCalibrateLocal plays the A.10b signal on this node's local sink for
	// durationSec (F2.1 self path). nil => no local signal source (no-op success).
	playCalibrateLocal func(durationSec int) error

	// roleEngine is the live group.Engine + inputs resolver the role loop drives
	// (role.go). nil => no realtime plane wired (runMaster/runFollower no-op).
	roleEngine *roleEngine

	// hooks is the live subsystem hookState behind roleEngine's engine, kept so
	// the role loop can sync the doc-driven transport state (media/Playing) after
	// each engine.Apply (role.go syncTransport). nil in tests that fake the engine.
	hooks *hookState

	// roleCtx is the current fenced role ctx the hook goroutines start under; it is
	// republished by applyRoleEngine on every role apply (set by setRoleCtx).
	roleCtx context.Context
}

// liveStatus is the daemon-internal per-member telemetry snapshot the transport's
// statusOf returns; memberStatus projects it into web.MemberStatus (08 §G.2).
type liveStatus struct {
	SyncErrorUs  int64
	OffsetUs     int64
	DriftRatio   float64
	Underruns    int64
	ClockQuality string
}

// peerProxy is the cross-node control seam (08 §F/§G fan-out + proxy), narrowed to
// an interface so media.go is testable with a fake. cmd binds it to an mTLS client
// over cluster member addrs. A nil peerProxy on the transport means "no peers" —
// the local node answers authoritatively (solo).
type peerProxy interface {
	// MediaExists reports whether file exists on nodeID's data/ folder (08 §F.2
	// master-side existence check). exists is meaningful only when err == nil.
	MediaExists(nodeID, file string) (exists bool, err error)
	// FanOutTransport notifies nodeID (the master) that the group's transport state
	// changed so it (re)points its origin without waiting for gossip (08 §F.3/§F.4).
	FanOutTransport(nodeID, groupID string) error
	// MemberStatus reads nodeID's live per-group telemetry over mTLS (08 §G.2).
	MemberStatus(nodeID, groupID string) (web.MemberStatus, error)
	// ListMedia proxies the F.1 list (one data/-relative folder: files + its
	// subdirectories) to a peer node.
	ListMedia(nodeID, path string) ([]web.MediaFile, []string, error)
	// CalibratePlay fans the A.10b signal out to nodeID for durationSec (F2.1).
	CalibratePlay(nodeID string, durationSec int) error
}

// errNoSession is the internal "no live engine yet" error the media ops return
// when the daemon has not stood up a state store. The Deps closures map it to a
// web.ErrNotReady-class response. Distinct from errNotImplemented (which marks a
// closure whose OWNING piece has not landed); media.go HAS landed, it just needs
// a live session.
var errNoSession = errors.New("daemon: no active session")

// txn returns the live transport handle under sessMu, or nil when no session.
func (n *Node) txn() *transport {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	return n.tx
}

// --- F.1 list ---------------------------------------------------------------

// listMedia backs Deps.ListMedia (08 §F.1): one data/-relative folder of the
// node's media tree (files + subdirectories, so the UI can browse into album
// folders). nodeID=="" or ==self lists locally; a peer id proxies over mTLS.
func (n *Node) listMedia(nodeID, path string) ([]web.MediaFile, []string, error) {
	tx := n.txn()
	if tx == nil {
		return nil, nil, nil // skeleton: empty list (handler returns []), not an error
	}
	if nodeID == "" || nodeID == tx.self {
		if tx.listLocal != nil {
			files, err := tx.listLocal() // injected flat list (tests)
			return files, nil, err
		}
		return listLocalMedia(tx.dataDir, path)
	}
	if tx.peer == nil {
		return nil, nil, nil
	}
	return tx.peer.ListMedia(nodeID, path)
}

// --- F.2 select / F.3 play / F.4 stop ---------------------------------------

// selectMedia is the F.2 core: validate the file (.mp3, exists on its SOURCE
// node), write GroupRecord.Media={file,loop} — and the MasterHint: the node
// whose data/ holds the file becomes the group's elected master and decodes it
// locally (A.5 soft hint; selecting a source MOVES mastership). src=="" leaves
// the hint alone (legacy callers). Under If-Match, gossiped.
func (n *Node) selectMedia(groupID, file string, loop bool, src string, ifMatch uint64) (state.ConfigDoc, error) {
	tx := n.txn()
	if tx == nil {
		return state.ConfigDoc{}, errNoSession
	}
	if !isMP3(file) {
		return state.ConfigDoc{}, web.ErrNotMP3
	}
	if err := n.checkMediaOnSource(tx, groupID, src, file); err != nil {
		return state.ConfigDoc{}, err
	}
	return n.applyGroup(tx, groupID, ifMatch, func(g *state.GroupRecord) {
		g.Media = state.MediaSelection{File: file, Loop: loop}
		if src != "" {
			g.MasterHint = src
		}
	})
}

// play is the F.3 core: optional one-shot select, then flip Playing=true under
// If-Match, gossip, and fan out to the master. Play with no media selected => 409.
func (n *Node) play(groupID, file string, loop bool, src string, ifMatch uint64) (state.ConfigDoc, error) {
	tx := n.txn()
	if tx == nil {
		return state.ConfigDoc{}, errNoSession
	}
	// One-shot select: validate + (re)write Media in the SAME write that flips
	// Playing, so play{file} is atomic (08 §F.3 "= F.2-then-play in one shot").
	if file != "" {
		if !isMP3(file) {
			return state.ConfigDoc{}, web.ErrNotMP3
		}
		if err := n.checkMediaOnSource(tx, groupID, src, file); err != nil {
			return state.ConfigDoc{}, err
		}
	}
	doc, err := n.applyGroup(tx, groupID, ifMatch, func(g *state.GroupRecord) {
		if file != "" {
			g.Media = state.MediaSelection{File: file, Loop: loop}
			if src != "" {
				g.MasterHint = src // the source node masters + decodes locally
			}
		}
		g.Playing = true
	})
	if err != nil {
		// A "no media selected" conflict is detected before the write in applyGroup's
		// guard, so any error here is the write/precondition failing.
		return state.ConfigDoc{}, err
	}
	// Fan the start out to the master so audio begins without waiting for gossip.
	if err := n.fanOut(tx, groupID); err != nil {
		return doc, err // config is written+gossiped; report the proxy failure (502)
	}
	return doc, nil
}

// stop is the F.4 core: flip Playing=false under If-Match, gossip, fan out the
// stop to the master.
func (n *Node) stop(groupID string, ifMatch uint64) (state.ConfigDoc, error) {
	tx := n.txn()
	if tx == nil {
		return state.ConfigDoc{}, errNoSession
	}
	doc, err := n.applyGroup(tx, groupID, ifMatch, func(g *state.GroupRecord) {
		g.Playing = false
	})
	if err != nil {
		return state.ConfigDoc{}, err
	}
	if err := n.fanOut(tx, groupID); err != nil {
		return doc, err
	}
	return doc, nil
}

// applyGroup reads the current doc, finds the group, runs mutate on its record,
// and writes it back under optimistic concurrency at ifMatch. It enforces the
// shared guards: unknown group => ErrNotMember; a play that would set Playing on a
// group with no media => ErrNoMedia (checked AFTER mutate so the post-state is the
// authority). A version mismatch => ErrVersionConflict.
func (n *Node) applyGroup(tx *transport, groupID string, ifMatch uint64, mutate func(*state.GroupRecord)) (state.ConfigDoc, error) {
	doc := tx.store.Get()
	if doc.Version != ifMatch {
		return state.ConfigDoc{}, web.ErrVersionConflict
	}
	idx := indexGroup(doc.Groups, groupID)
	if idx < 0 {
		return state.ConfigDoc{}, web.ErrNotMember
	}
	mutate(&doc.Groups[idx])
	// Play guard: cannot start a group that has no media selected (08 §F.3 409).
	if doc.Groups[idx].Playing && doc.Groups[idx].Media.File == "" {
		return state.ConfigDoc{}, web.ErrNoMedia
	}
	out, err := tx.store.Apply(doc)
	if err != nil {
		if errors.Is(err, state.ErrConflict) {
			return state.ConfigDoc{}, web.ErrVersionConflict
		}
		return state.ConfigDoc{}, err
	}
	return out, nil
}

// checkMediaOnSource verifies the file exists on its SOURCE node — the node
// that will be hinted master and decode it (08 §F.2 master-side decode). An
// empty src falls back to the group's current master (legacy callers). When the
// target is this node (or no peer proxy is wired — solo), the check is local. A
// missing file => ErrMissingOnMaster; an unreachable target => ErrUnreachable.
func (n *Node) checkMediaOnSource(tx *transport, groupID, src, file string) error {
	master := src
	if master == "" && tx.master != nil {
		master = tx.master(groupID)
	}
	if master == "" || master == tx.self || tx.peer == nil {
		// Local existence check (we are the master, or solo).
		ok, err := localMediaExists(tx, file)
		if err != nil {
			return err
		}
		if !ok {
			return web.ErrMissingOnMaster
		}
		return nil
	}
	ok, err := tx.peer.MediaExists(master, file)
	if err != nil {
		return web.ErrUnreachable
	}
	if !ok {
		return web.ErrMissingOnMaster
	}
	return nil
}

// fanOut notifies the group master that the transport state changed, so it
// (re)points its origin promptly (08 §F.3/§F.4 "+ fan-out to master"). When this
// node is the master (or solo / no peer), there is nothing to proxy — the local
// role loop already observes the store change. An unreachable master => 502.
func (n *Node) fanOut(tx *transport, groupID string) error {
	if tx.master == nil || tx.peer == nil {
		return nil
	}
	master := tx.master(groupID)
	if master == "" || master == tx.self {
		return nil
	}
	if err := tx.peer.FanOutTransport(master, groupID); err != nil {
		return web.ErrUnreachable
	}
	return nil
}

// localMediaExists reports whether file (a data/-relative path, possibly
// nested) is present on this node. The injected listLocal seam (tests) compares
// against the flat list; the disk path stats the sanitized file directly so a
// nested selection validates without walking the whole tree.
func localMediaExists(tx *transport, file string) (bool, error) {
	if tx.listLocal != nil {
		files, err := tx.listLocal()
		if err != nil {
			return false, err
		}
		for _, f := range files {
			if f.File == file {
				return true, nil
			}
		}
		return false, nil
	}
	return statLocalMedia(tx.dataDir, file), nil
}

// --- G.2 group status -------------------------------------------------------

// groupStatus backs Deps.GroupStatus (08 §G.2). It builds the response from the
// replicated GroupRecord (master/profile/playing/streamGen authority) and fans
// out a live-telemetry read to each member over mTLS. A single member that is
// unreachable is reported with Online=false, not a top-level error; an unknown
// group => ErrNotMember; an unreachable master => ErrUnreachable.
func (n *Node) groupStatus(groupID string) (web.GroupStatus, error) {
	tx := n.txn()
	if tx == nil {
		return web.GroupStatus{}, web.ErrGroupNotReady
	}
	doc := tx.store.Get()
	idx := indexGroup(doc.Groups, groupID)
	if idx < 0 {
		return web.GroupStatus{}, web.ErrNotMember
	}
	g := doc.Groups[idx]
	master := ""
	if tx.master != nil {
		master = tx.master(groupID)
	}

	st := web.GroupStatus{
		GroupID:      groupID,
		MasterNodeID: master,
		Profile:      profileView(g.Profile),
		Playing:      g.Playing,
	}
	for _, id := range g.MemberNodeIDs {
		st.Members = append(st.Members, n.memberStatus(tx, groupID, id))
	}
	return st, nil
}

// memberStatus reads one member's live telemetry: locally for self, else over the
// peer proxy. An unreachable peer is reported Online=false (08 §G.2 per-member).
// With no proxy wired, Online still reflects gossip liveness (zeroed telemetry)
// so the dashboard agrees with the cluster screen about who is alive.
func (n *Node) memberStatus(tx *transport, groupID, nodeID string) web.MemberStatus {
	if nodeID == tx.self {
		return localMemberStatus(tx, groupID, nodeID)
	}
	if tx.peer == nil {
		online := tx.live != nil && tx.live(nodeID)
		return web.MemberStatus{NodeID: nodeID, Online: online}
	}
	ms, err := tx.peer.MemberStatus(nodeID, groupID)
	if err != nil {
		return web.MemberStatus{NodeID: nodeID, Online: false}
	}
	ms.NodeID = nodeID
	ms.Online = true
	return ms
}

// localMemberStatus reads this node's own live telemetry from the renderer
// counters (when rendering). Before sync it reports Online with zeroed fields.
func localMemberStatus(tx *transport, groupID, nodeID string) web.MemberStatus {
	ms := web.MemberStatus{NodeID: nodeID, Online: true, ClockQuality: "poor"}
	if tx.statusOf != nil {
		live, ok := tx.statusOf(groupID)
		if ok {
			ms.SyncErrorUs = live.SyncErrorUs
			ms.OffsetUs = live.OffsetUs
			ms.DriftRatio = live.DriftRatio
			ms.Underruns = live.Underruns
			ms.ClockQuality = live.ClockQuality
		}
	}
	return ms
}

// --- F2.1 calibrate ---------------------------------------------------------

// calibratePlay backs Deps.CalibratePlay (08 §F2.1, A.10b). It resolves the
// selector to a node set, then plays the calibration signal on each: locally for
// self, fanned out over mTLS for peers. A Render=false node lands in warnings (not
// fatal). Returns the nodes that actually played + warnings.
func (n *Node) calibratePlay(sel web.CalibrateSel, durationSec int) (played []string, warnings []string, err error) {
	tx := n.txn()
	if tx == nil {
		return nil, nil, errNoSession
	}
	nodes, derr := n.calibrateTargets(tx, sel)
	if derr != nil {
		return nil, nil, derr
	}
	for _, id := range nodes {
		if !canRender(tx, id) {
			warnings = append(warnings, id+": node cannot render (Caps.Render=false)")
			continue
		}
		if perr := n.playCalibrate(tx, id, durationSec); perr != nil {
			warnings = append(warnings, id+": "+perr.Error())
			continue
		}
		played = append(played, id)
	}
	return played, warnings, nil
}

// calibrateTargets resolves a CalibrateSel to the node-id set (the handler has
// already enforced exactly-one). An unknown group => ErrNotMember.
func (n *Node) calibrateTargets(tx *transport, sel web.CalibrateSel) ([]string, error) {
	if sel.GroupID != "" {
		doc := tx.store.Get()
		idx := indexGroup(doc.Groups, sel.GroupID)
		if idx < 0 {
			return nil, web.ErrNotMember
		}
		return doc.Groups[idx].MemberNodeIDs, nil
	}
	return sel.NodeIDs, nil
}

// playCalibrate plays the signal on one node: locally for self, else proxied.
func (n *Node) playCalibrate(tx *transport, nodeID string, durationSec int) error {
	if nodeID == tx.self {
		if tx.playCalibrateLocal != nil {
			return tx.playCalibrateLocal(durationSec)
		}
		return nil
	}
	if tx.peer == nil {
		return web.ErrUnreachable
	}
	if err := tx.peer.CalibratePlay(nodeID, durationSec); err != nil {
		return web.ErrUnreachable
	}
	return nil
}

// canRender reports whether nodeID has Caps.Render=true in the replicated doc.
func canRender(tx *transport, nodeID string) bool {
	doc := tx.store.Get()
	for _, nr := range doc.Nodes {
		if nr.ID == nodeID {
			return nr.Caps.Render
		}
	}
	// Unknown node (e.g. self in a tiny test doc): assume render-capable so the
	// caller's playCalibrate path runs (it no-ops without a local signal source).
	return nodeID == tx.self
}

// --- Deps adapters (state.ConfigDoc -> web.ConfigView) ----------------------

// selectMediaDep adapts selectMedia to the Deps.SelectMedia signature (web view).
func (n *Node) selectMediaDep(groupID, file string, loop bool, src string, ifMatch uint64) (web.ConfigView, error) {
	doc, err := n.selectMedia(groupID, file, loop, src, ifMatch)
	return configView(doc), wrapMediaErr(err)
}

// playDep adapts play to the Deps.Play signature.
func (n *Node) playDep(groupID, file string, loop bool, src string, ifMatch uint64) (web.ConfigView, error) {
	doc, err := n.play(groupID, file, loop, src, ifMatch)
	return configView(doc), wrapMediaErr(err)
}

// stopDep adapts stop to the Deps.Stop signature.
func (n *Node) stopDep(groupID string, ifMatch uint64) (web.ConfigView, error) {
	doc, err := n.stop(groupID, ifMatch)
	return configView(doc), wrapMediaErr(err)
}

// wrapMediaErr maps the internal errNoSession (no live state store yet) to the
// web.ErrUnreachable sentinel so the media handlers surface a 502 proxy_failed
// rather than leaking the internal error string through their default 500 branch.
// In production the closures are wired only AFTER activate stands up the session,
// so this path is reached only by a request that races startup; in tests a live
// store is always injected. Every other (web sentinel) error is returned as-is.
func wrapMediaErr(err error) error {
	if errors.Is(err, errNoSession) {
		return web.ErrUnreachable
	}
	return err
}

// --- helpers ----------------------------------------------------------------

// indexGroup returns the index of groupID in groups, or -1.
func indexGroup(groups []state.GroupRecord, groupID string) int {
	for i := range groups {
		if groups[i].ID == groupID {
			return i
		}
	}
	return -1
}

// isMP3 reports whether file has a .mp3 extension (case-insensitive), the only
// MVP media format (D14).
func isMP3(file string) bool {
	return strings.EqualFold(strings.TrimSpace(filepathExt(file)), ".mp3")
}

// filepathExt returns the lowercase extension of file (incl. the dot), or "".
func filepathExt(file string) string {
	i := strings.LastIndexByte(file, '.')
	if i < 0 {
		return ""
	}
	return file[i:]
}
