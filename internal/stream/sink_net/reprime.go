package sink_net

// This file (P5.3) is the receiver's generation-change + flush/reprime wiring. It
// lives in the sink_net package so it can touch the Receiver's unexported reorder
// window, FEC handle, ring and prime state without widening the public API; the
// reusable adopt/pass/drop decision lives in the leaf internal/stream/streamgen
// package (Gate), unit-tested in isolation.
//
// Refs: doc 05 §5.6.4 (late-join prime) / §5.8 (generation gate); A.12 (LeadMs);
// A.14.4 (ReceiverFlushReprime fence) / R11 (one buffer on (re)start); D22.

// FlushAndReprime discards the reorder/dedupe window, resets FEC recover state,
// drops the stale ring tail, and re-enters the prime state: no playout (the
// FollowerTimeline reports ok=false because LatestChunkMeta ok=false) until one
// buffer lead (cfg.LeadMs) of keyframe-anchored audio has accumulated from the
// first decodable keyframe (05 §5.6.4 step 4 / §5.7).
//
// It is the Hooks.ReceiverFlushReprime entry (A.14.4 / R11): the group engine calls
// it when (re)starting the follower receiver so the follower re-primes ~one buffer
// on (re)start. It is also invoked internally on a Gate.Adopt (a newer generation,
// e.g. a seek, invalidates the buffered audio of the old generation, so flushing
// keeps the seek crisp instead of playing out ~300 ms of stale audio first). Safe
// to call only when Run is not concurrently mutating state (the role applier
// sequences (re)start, A.14.4).
func (r *Receiver) FlushAndReprime() { r.flushAndReprime() }

// flushAndReprime is the internal flush shared by the public hook and the
// generation-adopt branch of the recv loop. It preserves the cardinal rule: the
// ring is always filled at the correct sampleIndex (05 §5.6.3) — the flush
// re-anchors the index at the next keyframe, it never shifts subsequent audio.
func (r *Receiver) flushAndReprime() {
	r.win.reset()           // clear the reorder/dedupe window
	r.fec = resetFEC(r.fec) // reset FEC parity-group / dedupe recover state
	r.ring.Reset()          // drop the stale ring tail (full flush; the new gen re-anchors)
	if rp, ok := r.push.(*ringPusher); ok {
		// Drop the play cursor so the next push re-seeds it at the new sampleIndex
		// (a seek/failover may move the index backward as well as forward).
		rp.haveCursor = false
		rp.playCursor = 0
	}
	// Re-enter the prime state: withhold playout and require a keyframe-first decode.
	// awaitKeyframe/primeFrames are recv-goroutine-owned; primed/haveChunk/latest are
	// read by the FollowerTimeline render goroutine, so they are cleared under metaMu.
	r.awaitKeyframe = true
	r.primeFrames = 0
	r.metaMu.Lock()
	r.primed = false
	r.haveChunk = false
	r.latest = chunkMeta{}
	r.metaMu.Unlock()
}
