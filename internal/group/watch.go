package group

import (
	"context"
	"net/netip"
	"time"

	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// reconcileTick is the self-heal / heartbeat ticker cadence. Self-heal needs a
// sub-Grace cadence so a dangling follow resets on time even absent cluster
// events; the heartbeat keys off its own elapsed check.
const reconcileTick = 1 * time.Second

// Run launches the single reconcile goroutine: it ranges Cluster.Subscribe() and
// a 1 s ticker, and on each wake re-points the local subscriber/sink/clock at the
// current master, runs self-heal, tears down a session this node should no longer
// host, and (master, while playing) refreshes the playback heartbeat. Returns
// when ctx is cancelled or Close is called.
func (e *Engine) Run(ctx context.Context) {
	changes := e.p.Cluster.Subscribe()
	t := time.NewTicker(reconcileTick)
	defer t.Stop()

	e.reconcile() // initial pass

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.done:
			return
		case <-changes:
			e.reconcile()
		case <-t.C:
			e.reconcile()
		}
	}
}

// reconcile is one pass of the watch loop. Takes e.mu for the duration except
// across a session teardown halt (which is done lock-free, then re-locked).
func (e *Engine) reconcile() {
	now := e.now()
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if !mv.found {
		// Self not yet derived (transient): skip re-point/heal, no panic.
		e.mu.Unlock()
		return
	}

	// Log observed group composition changes (membership / master / our role).
	e.logCompositionLocked(mv)

	// Self-heal dangling follow (§5).
	e.reconcileHeal(mv, now)

	// Tear down a session this node no longer hosts (lost mastership). No status
	// rewrite — H is no longer the writer of that group's record (§3.4).
	isMaster := mv.role == roleMaster || mv.role == roleSolo
	if e.sess != nil && !isMaster {
		e.stopLocked()
	}

	// Re-point local plumbing at the current master (the heart of "members follow
	// the master automatically", §3.2). Subscribe/arm only while the group has an
	// active session: an idle node must not HELLO, must not arm its sink (no
	// boot-time starvation warnings), and must BYE when the session ends.
	// A paused session (D39) is frozen: not "playing" for plumbing purposes, so
	// members unsubscribe + Disarm and the master leaves its own source too.
	activeSess := e.sess != nil && !e.sess.paused.Load()
	gen := e.curGen
	if isMaster && activeSess {
		gen = e.sess.gen.Load() // our own running session
	}
	playing := mv.group.Playback.State == "playing" || (isMaster && activeSess)
	e.repointLocked(mv.master, gen, mv.group.Settings.Transport, playing)

	// Heartbeat (D28): master, while playing, every Heartbeat interval.
	if e.sess != nil && isMaster && !now.Before(e.lastBeat.Add(e.p.Heartbeat)) {
		e.p.Cluster.SetPlayback(e.sess.groupID, e.sess.playbackRecord(now, e.p.Source.Stats()))
		e.lastBeat = now
	}

	// 1 Hz playing stats: one INFO line per active side per second (master via
	// SourceStats; member via sink/clock/client counters). Stops when idle.
	e.logPlayingStatsLocked(mv, isMaster, now)

	e.mu.Unlock()
}

// repointLocked arms the three local consumers (subscriber, sink, clock) for the
// given master + generation when they changed since last time (§3.2). The master
// dials itself via loopback like any member (§8.2). Caller holds e.mu; the calls
// are all non-blocking target updates.
func (e *Engine) repointLocked(master id.ID, gen uint32, transport string, playing bool) {
	if master.IsZero() {
		return
	}
	if e.haveCur && e.curMaster == master && e.curGen == gen && e.curPlaying == playing {
		return // unchanged: idempotent, no churn
	}
	// Generations are PER-MASTER counters: a new master's session may carry a
	// LOWER gen than our previous master's (or our own) last one. Never carry
	// the old floor across a master change — subscribe/arm at 0 and let the
	// first frame / RECONFIG establish the new master's gen (the client window
	// and the deliver-side sink re-arm both re-anchor upward).
	if e.haveCur && e.curMaster != master && master != e.self {
		gen = 0
	}

	ip := e.dialIP(master)
	if !ip.IsValid() {
		// No address yet (cold peer with no observations / no CIDRs). Try again
		// next reconcile; do not record the (master,gen) as armed.
		return
	}

	var srcPort, streamPort int
	for _, n := range e.snapshotNodes() {
		if n.ID == master {
			srcPort = n.SourcePort
			streamPort = n.StreamPort
			break
		}
	}
	if srcPort == 0 || streamPort == 0 {
		return
	}

	srcAddr := netip.AddrPortFrom(ip, uint16(srcPort))
	clkAddr := netip.AddrPortFrom(ip, uint16(streamPort))

	// The clock follower tracks the master ALWAYS (cheap; keeps every member
	// synced and ready). Stream subscription + sink arming are session-gated.
	e.p.ClockCtl.SetMaster(clkAddr, gen)
	if playing {
		e.p.Sink.Reset(gen)
		_ = e.p.Sub.Subscribe(srcAddr, gen, stream.ParseTransport(transport))
	} else if e.haveCur && e.curPlaying {
		// Session ended: leave the source politely (BYE) and disarm playout
		// cleanly (no starvation warnings).
		e.p.Sub.Unsubscribe()
		e.p.Sink.Disarm()
	}

	e.curMaster = master
	e.curGen = gen
	e.curPlaying = playing
	e.haveCur = true
}

// dialIP returns the best dial IP for master, substituting loopback when this
// node IS the master (DialCandidates may be empty on a loopback-only host).
func (e *Engine) dialIP(master id.ID) netip.Addr {
	if cands := e.p.Cluster.DialCandidates(master); len(cands) > 0 {
		return cands[0]
	}
	if master == e.self {
		return netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	return netip.Addr{}
}

// snapshotNodes fetches the current node views (for port lookup). Small helper so
// repointLocked stays readable; called under e.mu but Snapshot has its own lock.
func (e *Engine) snapshotNodes() []nodeView {
	snap := e.p.Cluster.Snapshot()
	out := make([]nodeView, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		out = append(out, nodeView{ID: n.ID, SourcePort: n.SourcePort, StreamPort: n.StreamPort})
	}
	return out
}

type nodeView struct {
	ID         id.ID
	SourcePort int
	StreamPort int
}
