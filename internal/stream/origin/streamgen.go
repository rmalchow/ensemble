package origin

import (
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/streamgen"
)

// This file (P5.3) is the origin's generation-correctness wiring. It lives in the
// origin package so it can drive the Run loop's unexported gen/seq/FEC/keyframe
// state without widening the public API, but the reusable decision logic — when to
// bump, the seq/FEC-reset + keyframe directives — lives in the leaf
// internal/stream/streamgen package (Controller), unit-tested in isolation.
//
// Refs: doc 05 §5.6.4 (late join) / §5.8 (streamGen semantics); doc 04 §4.3.3
// (profile change) / §4.4.4 (failover resume); A.14.4 (OriginResumeAt fence);
// D22/R11 (master change ALWAYS bumps; timeline continuous).

// Bump regenerates the stream for reason (doc 05 §5.8): it advances streamGen via
// the Controller, latches the reset directives (seq→0, FEC reset, re-anchored
// sampleIndex) for Run to apply at the next chunk boundary, and forces a keyframe
// on that first chunk for every current listener. The new generation is published
// immediately (atomic gen) so a concurrent StreamGen status read observes it.
//
// atSample is the timeline position the new generation starts at, per reason:
//   - ReasonSeek          ⇒ the seek target sampleIndex
//   - ReasonMediaChange   ⇒ the continuing position (or 0 on a fresh media start)
//   - ReasonProfileChange ⇒ the continuing sampleIndex (timeline continuous, §4.3.3)
//   - ReasonMasterChange  ⇒ the current timeline sampleIndex at promotion (§4.4.4)
//
// A loop boundary is NOT a reason and never calls Bump (doc 05 §5.2.1): the
// timeline is continuous, the loop is invisible to listeners. Returns the new
// streamGen.
//
// The bump is LATCHED, not applied immediately: Run applies the directives (and
// publishes the now-live gen for status reads) atomically at the next chunk
// boundary, so the first chunk of the new generation always carries the new gen
// together with seq=0 and the re-anchored sampleIndex — never the new gen on an
// old seq/idx (05 §5.8). The returned value is the controller's new generation.
func (o *Origin) Bump(reason streamgen.Reason, atSample int64, playing bool) uint64 {
	o.mu.Lock()
	g := o.ctrl.Bump(reason, atSample, playing)
	o.pending = &g
	o.mu.Unlock()
	o.sender.armKeyframeAll() // first chunk of the new generation is a keyframe for all (step 4)
	return g.Gen
}

// ResumeAt is the A.14.4 / R4 failover-continuity entry: the just-promoted master
// resumes the stream at sampleIndex with the replicated Playing (doc 04 §4.4.4) and
// Bumps with ReasonMasterChange so receivers flush+reprime and resync cold (D22).
// The MasterTimeline.Seed for failover continuity is done by the caller
// (cmd/ensemble hookState.originResumeAt) before this is invoked, since the origin
// holds only the read-side Timeline projection (NowSample), not the seed authority.
// Wired as Hooks.OriginResumeAt (P3.2). Returns the new streamGen.
func (o *Origin) ResumeAt(sampleIndex int64, playing bool) uint64 {
	return o.Bump(streamgen.ReasonMasterChange, sampleIndex, playing)
}

// onLateJoin handles a fresh listener joining a playing group (doc 05 §5.6.4 /
// A.10): the origin does NOT rewind — the joiner enters the live timeline at the
// next chunk boundary at/after the current sampleIndex — but it forces a keyframe
// for codecs with inter-frame state so the joiner can decode cold. For PCM every
// chunk is already a keyframe (so this is a no-op flag-wise); for Opus the encoder
// resets and emits the keyframe immediately rather than waiting for the periodic
// one (~500 ms, doc 05 §5.4.2).
//
// Per-join keyframe forcing does NOT bump streamGen — it is a single keyframe flag
// for that listener's cold start, not a generation change (doc 05 §5.6.4). The
// sender.add already armed this listener's keyframe; onLateJoin is the named seam
// for that decision and the place to hang any future per-codec encoder-reset hook.
func (o *Origin) onLateJoin(id string) {
	o.sender.armKeyframe(id)
}

// takePending returns and clears a latched bump's directives, if any. Called by
// Run at a chunk boundary so the generation change is atomic w.r.t. a chunk.
func (o *Origin) takePending() (*streamgen.Generation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	g := o.pending
	o.pending = nil
	return g, g != nil
}

// resetFEC returns a fresh FEC of the same id so a generation change starts with
// clean parity state (doc 05 §5.8 step 2). For None this is a fresh stateless
// value; for a test fake whose id is unconstructible, the existing instance is
// kept.
func resetFEC(f fec.FEC) fec.FEC {
	fresh, err := fec.New(f.ID())
	if err != nil {
		return f
	}
	return fresh
}
