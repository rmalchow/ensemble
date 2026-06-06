package group

// failover.go (P7.1) is the bounded-gap master-failover continuity helper
// (01 §6d, 04 §4.4.4 failover, 05 §5.8, A.14.4). On promotion (follower→master)
// the just-promoted node must NOT reset playout to sample 0: it seeds its fresh
// MasterTimeline from the last sample index it was projecting as a follower, then
// drives the origin's generation bump so receivers flush + re-prime onto the new
// master's stream. The audible gap is then bounded by the suspicion window (when
// SWIM declared the old master dead) plus one buffer lead (LeadMs = 300 ms,
// A.12) — not by a from-scratch restart.
//
// This is glue, not new policy: the decision to promote lives in P3.2's
// Recompute (nextStreamGen already bumps StreamGen on becoming master); this
// helper threads the continuity anchor and the Playing flag into the timeline +
// origin so the wiring in cmd/ensemble (and the engine's OriginResumeAt hook) has
// one named, tested entry point.

// OriginResumer is the narrow subset of *origin.Origin the failover glue needs.
// Declaring it here (the consumer) keeps the group→origin edge one-directional:
// group depends on this interface, origin never imports group (01 §2). The
// concrete *origin.Origin satisfies it via ResumeAt (P4.8 §4.1 / streamgen.go).
type OriginResumer interface {
	// ResumeAt bumps streamGen (ReasonMasterChange), restarts seq at 0, resets FEC
	// state, re-anchors at sampleIndex, and forces a keyframe first chunk for every
	// listener (05 §5.8). It returns the new streamGen.
	ResumeAt(sampleIndex int64, playing bool) uint64
}

// SeedAndResume is the failover-continuity entry (01 §6d / 04 §4.4.4 / A.14.4). On
// promotion it:
//
//  1. Seeds the new MasterTimeline from lastSample with the replicated
//     GroupRecord.Playing flag (R4) — playout continues from where the follower
//     left off rather than resetting to 0.
//  2. Drives o.ResumeAt(lastSample, playing) — master failover ALWAYS bumps
//     streamGen (R11 / 05 §5.8: there is no "continue same generation" path), so
//     followers see a newer gen and flush + re-prime ~one buffer onto the new
//     master's stream.
//
// It returns the new streamGen. The ordering is deliberate: Seed first so the
// timeline reads the continuity anchor before the origin re-anchors and stamps
// the first chunk of the new generation at the same sampleIndex.
//
// lastSample is the demoting follower's last projected position
// (FollowerTimeline.NowSample, or the replicated GroupRecord position on a cold
// promotion where the follower never locked — risk Q3: the caller resolves which
// source is authoritative and passes the resolved value here).
func SeedAndResume(tl *MasterTimeline, o OriginResumer, lastSample int64, playing bool) uint64 {
	if tl != nil {
		tl.Seed(lastSample, playing)
	}
	if o == nil {
		return 0
	}
	return o.ResumeAt(lastSample, playing)
}
