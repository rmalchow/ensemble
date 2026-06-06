// Package streamgen is the generation-correctness layer of the realtime audio
// path: the small, load-bearing decision logic that keeps a group's PCM/Opus
// stream sample-aligned across discontinuities (media change, operator seek,
// profile renegotiation, and master failover).
//
// It has two halves, both pure state machines over plain uint64/int64/bool with
// no I/O, no codec, no FEC, no ring and no socket — so they are trivially
// unit-testable and reusable by both the master origin (stream/origin, P4.8) and
// the follower receiver (stream/sink_net, P4.9) without a layering cycle (doc 01
// §2):
//
//   - Controller (master side) owns the streamGen counter and the bump rules.
//     It bumps on a media change, a seek, a profile change (doc 04 §4.3.3), and
//     on EVERY master change / failover / takeover / rejoin-flip (D22, R11, A.5)
//     — and deliberately NEVER on a loop boundary (doc 05 §5.2.1: the timeline is
//     continuous, just more PCM, so a loop is invisible to listeners). A bump
//     restarts seq at 0, resets FEC state, re-anchors sampleIndex, and forces a
//     keyframe on the first chunk of the new generation so even Opus (which
//     carries encoder state) resyncs cold (doc 05 §5.8 steps 1–4 / §5.4.2).
//
//   - Gate (follower side) compares an inbound chunk's StreamGen to the accepted
//     generation: greater ⇒ Adopt (the caller flushes the reorder window + FEC
//     state + the stale ring tail, then resumes at the new keyframe sampleIndex);
//     equal ⇒ Pass (normal decode); lesser ⇒ Drop (a stale straggler reordered
//     late from a prior generation). streamGen monotonicity is what disambiguates
//     stragglers (doc 05 §5.8, A.2 / doc 04 §4.4.3).
//
// There is no "continue the same generation if reconstructable" path: a new
// master, even one resuming identical media, starts a fresh generation, keeping
// failover unconditional and split-brain-safe (doc 05 §5.8, D22).
//
// Refs: README §6.3/§6.4/§6.2; doc 05 §5.6.4 (late join) / §5.8 (streamGen
// semantics); doc 04 §4.3.3 (profile change) / §4.4.4 (failover resume,
// Timeline.Seed continuity); Appendix A.5 (election → master change → gen++),
// A.10 (late join + receiver pipeline), A.12 (tunables), A.14.4 (applyRole
// OriginResumeAt / ReceiverFlushReprime fence).
package streamgen
